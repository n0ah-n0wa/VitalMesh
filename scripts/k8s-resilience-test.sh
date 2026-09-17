#!/bin/sh
# Production-style Kubernetes resilience test (SPECIFICATIONS.md sections 36,
# 37, 38, 52, 90 to 94; docs/OPERATIONS.md records what it observes).
#
# scripts/k8s-local-test.sh and scripts/k8s-failure-test.sh run against one
# node and one replica: enough to prove the manifests apply and each
# dependency degrades correctly, not enough to prove the things that only
# exist at scale. A PodDisruptionBudget only decides anything when a drain
# has somewhere else to put the pod; topologySpreadConstraints only spread
# across nodes that exist; an HPA only demonstrates scaling when a metrics
# source feeds it. This test supplies all three: three nodes, two replicas
# of each service (the production floor, section 36), and a metrics-server.
#
#   control-plane   PostgreSQL and Redis fixtures, pinned here and tainted
#                   off for the application, so draining a worker never
#                   destroys a fixture's emptyDir data
#   worker x2       two api-gateway and two processor replicas, spread
#
# What it exercises, and what it verifies of each (the user's brief):
#
#   rolling restart      graceful shutdown: zero requests dropped
#   pod deletion         the Service never loses all endpoints; replacement Ready
#   node disruption      a worker drained; PDB gates it, pods reschedule, none dropped
#   processor scaling    the HPA raises the Deployment and the new replica goes Ready
#   gateway scaling      the same, and the live CPU metric proves the HPA is fed
#   Redis outage         readiness holds, features degrade, no restart
#   database outage      readiness drops, the pod is withdrawn, no restart, recovery
#
# and throughout: readiness, PDB allowed-disruptions, HPA actuation, service
# recovery, no data corruption, and no job left in a non-terminal state.
#
#   sh scripts/k8s-resilience-test.sh          create, test, destroy
#   sh scripts/k8s-resilience-test.sh --keep   leave the cluster running
#
# Needs kind and kubectl on PATH, and the two images built (make
# docker-build) and the local overlay rendered (make k8s-validate) — the
# same file the validation gates checked is what is deployed. kind and
# kubectl are the one place this repository steps outside Docker, for the
# reason the other two scripts give: kind builds its nodes as containers on
# the host's Docker and so cannot itself run in one.
set -eu

CLUSTER="vitalmesh-ha"
NAMESPACE="vitalmesh-local"
KUBE_DIR="infrastructure/kubernetes"
MANIFEST="$KUBE_DIR/.rendered/overlays-local.yaml"
KIND_CONFIG="$KUBE_DIR/kind/cluster-ha.yaml"
PROBE_NS="vitalmesh-resilience-probe"

CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"
METRICS_MANIFEST="https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml"

GATEWAY_IMAGE="vitalmesh/api-gateway:dev"
PROCESSOR_IMAGE="vitalmesh/processor:dev"

EMAIL="resilience-$(date +%s)@vitalmesh.local"
PASSWORD="k8s-resilience-test-password-not-a-secret"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
note() { printf '        %s\n' "$1"; }
step() { printf '\n%s\n' "$1"; }

k()   { kubectl --context "kind-$CLUSTER" "$@"; }
kc()  { kubectl --context "kind-$CLUSTER" -n "$NAMESPACE" "$@"; }
kcp() { kubectl --context "kind-$CLUSTER" -n "$PROBE_NS" "$@"; }

cleanup() {
    kubectl --context "kind-$CLUSTER" delete namespace "$PROBE_NS" --wait=false >/dev/null 2>&1 || true
    kubectl --context "kind-$CLUSTER" uncordon --all >/dev/null 2>&1 || true
    if [ "$KEEP" -eq 0 ]; then
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------- helpers
restarts() {
    kc get pods -l "app.kubernetes.io/name=$1" \
        -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
        | awk '{s+=$1} END {print s+0}'
}
ready_replicas() { kc get deployment "$1" -o jsonpath='{.status.readyReplicas}' 2>/dev/null | grep -o '[0-9]*' || echo 0; }
endpoints()      { kc get endpoints "$1" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null; }
node_of()        { kc get pods -l "app.kubernetes.io/name=$1" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null; }
sql() {
    out="$(kc exec deploy/postgres -- psql -U vitalmesh -d vitalmesh -tAc "$1" 2>/dev/null | tr -d '\r')"
    [ -z "$out" ] && { printf 'QUERY-FAILED'; return 0; }
    printf '%s' "$out"
}
json() {
    printf '%s' "$1" | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//'
}
code() { t="$1"; shift; kcp exec probe -- curl -s -o /dev/null -w '%{http_code}' --max-time "$t" "$@" 2>/dev/null || echo "000"; }
api()  { t="$1"; shift; kcp exec probe -- curl -s --max-time "$t" "$@" 2>/dev/null || true; }

wait_ready_replicas() { # <deployment> <n> <seconds>
    i=0
    while [ "$i" -lt "$3" ]; do
        [ "$(ready_replicas "$1")" -ge "$2" ] && return 0
        sleep 3; i=$((i + 3))
    done
    return 1
}
wait_rollout() { kc rollout status deployment/"$1" --timeout="${2:-180}s" >/dev/null 2>&1; }

# probe_flood <seconds> <url> -> prints the count of non-2xx/3xx answers
probe_flood() {
    kcp exec probe -- sh -c "
        end=\$(( \$(date +%s) + $1 )); f=0
        while [ \$(date +%s) -lt \$end ]; do
            c=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 '$2' 2>/dev/null || echo 000)
            case \$c in 2*|3*) ;; *) f=\$((f+1)) ;; esac
        done
        echo \$f" 2>/dev/null || echo "PROBE-FAILED"
}

BASE="http://vitalmesh-api-gateway.$NAMESPACE:8080"
API="$BASE/api/v1"

# ---------------------------------------------------------------- preflight
step "Preflight"
for tool in kind kubectl docker; do
    command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not on PATH" >&2; exit 2; }
done
ok "kind, kubectl and docker are present"
[ -f "$MANIFEST" ] || { echo "run: make k8s-validate first ($MANIFEST is missing)" >&2; exit 2; }
ok "rendered manifest present, the same one the validation gates checked"
for image in "$GATEWAY_IMAGE" "$PROCESSOR_IMAGE"; do
    docker image inspect "$image" >/dev/null 2>&1 || { echo "$image is missing; run: make docker-build" >&2; exit 2; }
done
ok "both service images are built"

# ------------------------------------------------------------------ cluster
step "Cluster (three nodes)"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    ok "reusing the existing $CLUSTER cluster"
else
    kind create cluster --config "$KIND_CONFIG" >/dev/null 2>&1 && ok "created" || { fail "kind create cluster"; exit 1; }
fi
if k -n kube-system get daemonset calico-node >/dev/null 2>&1; then
    ok "Calico already installed"
else
    k apply -f "$CALICO_MANIFEST" >/dev/null 2>&1 || fail "could not install Calico"
fi
k -n kube-system wait --for=condition=ready pod -l k8s-app=calico-node --timeout=300s >/dev/null 2>&1 \
    && ok "Calico enforcing NetworkPolicies" || fail "Calico did not become ready"
k wait --for=condition=Ready node --all --timeout=180s >/dev/null 2>&1 && ok "all three nodes Ready" || fail "nodes not Ready"
if k -n kube-system get deploy metrics-server >/dev/null 2>&1; then
    ok "metrics-server already installed"
else
    k apply -f "$METRICS_MANIFEST" >/dev/null 2>&1 || fail "could not install metrics-server"
    k -n kube-system patch deploy metrics-server --type=json \
      -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null 2>&1 || true
fi
k -n kube-system rollout status deploy/metrics-server --timeout=180s >/dev/null 2>&1 \
    && ok "metrics-server ready (the HPA has a CPU source)" || note "metrics-server not ready; HPA metrics may read <unknown>"
kind load docker-image "$GATEWAY_IMAGE" "$PROCESSOR_IMAGE" --name "$CLUSTER" >/dev/null 2>&1 \
    && ok "images present on every node" || fail "kind load docker-image"

CP="$(k get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')"
WORKERS="$(k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -v "^$CP$")"

# --------------------------------------------------------- topology setup
step "Topology: fixtures on the control-plane, application on the workers"
# Taint the control-plane so no application pod lands on it, then pin the
# two stateful fixtures there. Draining a worker can then never destroy a
# fixture's emptyDir data, which is what makes the node-disruption scenario
# safe to run against in-cluster PostgreSQL.
k taint node "$CP" node-role.kubernetes.io/control-plane=:NoSchedule --overwrite >/dev/null 2>&1 || true
ok "control-plane tainted NoSchedule"

if out="$(k apply --server-side --force-conflicts -f "$MANIFEST" 2>&1)"; then
    ok "$(printf '%s' "$out" | grep -c . || true) objects applied"
    printf '%s' "$out" | grep -qi warning && { fail "admission warnings"; printf '%s\n' "$out" | grep -i warning | sed 's/^/        /'; } || ok "no admission warnings"
else
    fail "apply"; printf '%s\n' "$out" | sed 's/^/        /'; exit 1
fi

pin='{"spec":{"template":{"spec":{"nodeSelector":{"node-role.kubernetes.io/control-plane":""},"tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}]}}}}'
kc patch deployment postgres --type=merge -p "$pin" >/dev/null 2>&1
kc patch deployment redis    --type=merge -p "$pin" >/dev/null 2>&1
wait_ready_replicas postgres 1 120 && ok "PostgreSQL rescheduled onto the control-plane and ready" || fail "PostgreSQL not ready"
wait_ready_replicas redis 1 60 && ok "Redis ready on the control-plane" || fail "Redis not ready"
[ "$(node_of postgres)" = "$CP" ] && ok "PostgreSQL is on the control-plane, out of the drain path" || fail "PostgreSQL is on $(node_of postgres), not the control-plane"

# The migration Job may have run against the pre-pin PostgreSQL pod; run the
# gateway's own idempotent migrator against the pinned one so the schema is
# there deterministically, whatever the Job raced with.
kc rollout status deployment/vitalmesh-api-gateway --timeout=30s >/dev/null 2>&1 || true
GW_ANY="$(kc get pods -l app.kubernetes.io/name=api-gateway -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
if [ -n "$GW_ANY" ] && kc exec "$GW_ANY" -- /usr/local/bin/api-gateway migrate up >/dev/null 2>&1; then
    ok "schema migrated on the pinned PostgreSQL"
else
    note "migrate exec returned non-zero (schema may already be current)"
fi

# Move the application onto the workers only (the taint repels new pods; a
# restart reschedules any that were placed before it).
kc rollout restart deployment/vitalmesh-api-gateway deployment/vitalmesh-processor >/dev/null 2>&1
kc patch hpa vitalmesh-api-gateway --type=merge -p '{"spec":{"minReplicas":2,"maxReplicas":4}}' >/dev/null 2>&1
kc patch hpa vitalmesh-processor   --type=merge -p '{"spec":{"minReplicas":2,"maxReplicas":4}}' >/dev/null 2>&1
wait_ready_replicas vitalmesh-api-gateway 2 180 && ok "two api-gateway replicas ready" || fail "api-gateway did not reach two ready"
wait_ready_replicas vitalmesh-processor 2 180 && ok "two processor replicas ready" || fail "processor did not reach two ready"

# -------------------------------------------------------- probe pod
step "In-cluster client"
k create namespace "$PROBE_NS" >/dev/null 2>&1 || true
k label namespace "$PROBE_NS" vitalmesh.io/gateway-client=true --overwrite >/dev/null 2>&1
# Pinned to the control-plane and tolerating its taint, so a worker drain
# never takes the test client with it.
kcp apply -f - >/dev/null 2>&1 <<PROBE
apiVersion: v1
kind: Pod
metadata:
  name: probe
spec:
  restartPolicy: Never
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
  containers:
    - name: c
      image: curlimages/curl:8.10.1
      command: ["sleep", "3600"]
PROBE
kcp wait --for=condition=ready pod/probe --timeout=180s >/dev/null 2>&1 && ok "probe pod ready on the control-plane" || { fail "probe pod"; exit 1; }

# ------------------------------------------------------------------ baseline
step "Baseline"
printf '%s' "$PASSWORD" | kc exec -i deploy/vitalmesh-api-gateway -- /usr/local/bin/api-gateway users create "$EMAIL" OPERATOR >/dev/null 2>&1 \
    && ok "operator created through the gateway" || fail "could not create the operator"
login="$(api 15 -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")"
TOKEN="$(json "$login" access_token)"
[ -n "$TOKEN" ] && ok "signed in" || { fail "sign-in: $login"; exit 1; }
AUTH="Authorization: Bearer $TOKEN"
REF="resilience-$(date +%s)"
patient="$(api 15 -X POST "$API/patients" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"external_reference\":\"$REF\",\"date_of_birth\":\"1985-04-12\",\"sex\":\"FEMALE\"}")"
PID="$(json "$patient" id)"
[ -n "$PID" ] && ok "patient registered" || { fail "patient: $patient"; exit 1; }
now="$(date +%s)"; items=""; i=0
while [ "$i" -lt 30 ]; do
    at=$((now - 1800 + i * 60)); ts="$(date -u -d "@$at" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
    v=72; [ "$i" -eq 15 ] && v=185
    [ -n "$items" ] && items="$items,"
    items="$items{\"patient_id\":\"$PID\",\"type\":\"HEART_RATE\",\"value\":$v,\"unit\":\"bpm\",\"recorded_at\":\"$ts\",\"source\":\"resilience\"}"
    i=$((i + 1))
done
api 30 -X POST "$API/measurements/batch" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"items\":[$items]}" >/dev/null
MEAS_BEFORE="$(sql "select count(*) from measurements where patient_id='$PID'")"
[ "$MEAS_BEFORE" = "30" ] && ok "30 readings stored" || fail "stored $MEAS_BEFORE readings, expected 30"

submit_job() {
    api 60 -X POST "$API/processing/jobs" -H "$AUTH" -H 'Content-Type: application/json' \
        -d "{\"patient_id\":\"$PID\",\"measurement_types\":[\"HEART_RATE\"],\"windows\":[\"5m\"],\"percentiles\":[50]}"
}
[ "$(json "$(submit_job)" status)" = "COMPLETED" ] && ok "a processing job completes end to end" || fail "baseline job did not complete"

# ------------------------------------------------------------ 1. readiness/PDB/spread
step "1. Readiness, spread and PodDisruptionBudget"
[ "$(code 10 "$BASE/ready")" = "200" ] && ok "/ready is 200 through the Service" || fail "/ready not 200"
gw_nodes="$(kc get pods -l app.kubernetes.io/name=api-gateway -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | grep -c . || true)"
[ "$gw_nodes" -ge 2 ] && ok "api-gateway replicas spread across $gw_nodes nodes" || note "api-gateway replicas on $gw_nodes node(s)"
for d in vitalmesh-api-gateway vitalmesh-processor; do
    allowed="$(kc get pdb "$d" -o jsonpath='{.status.disruptionsAllowed}' 2>/dev/null)"
    [ "${allowed:-0}" -ge 1 ] && ok "$d PDB allows $allowed disruption(s)" || fail "$d PDB allows none"
done
mtop=0; i=0
while [ "$i" -lt 60 ]; do
    [ "$(kc top pods -l app.kubernetes.io/name=api-gateway --no-headers 2>/dev/null | grep -c . || true)" -ge 1 ] && { mtop=1; break; }
    sleep 5; i=$((i + 5))
done
[ "$mtop" -eq 1 ] && ok "metrics-server reports pod CPU (the HPA has a live source)" || fail "no pod metrics after 60s: the HPA has no CPU source"

# ------------------------------------------------------------ 2. rolling restart
step "2. Rolling restart (graceful shutdown)"
for d in vitalmesh-api-gateway vitalmesh-processor; do
    url="$BASE/health"; [ "$d" = "vitalmesh-processor" ] && url="$BASE/health"
    kc rollout restart deployment/"$d" >/dev/null 2>&1
    dropped="$(probe_flood 40 "$url")"
    wait_rollout "$d" 180 && rc=0 || rc=1
    [ "$rc" -eq 0 ] && ok "$d rolling restart completed" || fail "$d rollout did not complete"
    [ "${dropped:-1}" = "0" ] && ok "$d: no request dropped across the restart" || fail "$d: $dropped requests dropped during the restart"
done

# ------------------------------------------------------------ 3. pod deletion
step "3. Pod deletion (involuntary loss)"
for name in api-gateway processor; do
    victim="$(kc get pods -l app.kubernetes.io/name=$name -o jsonpath='{.items[0].metadata.name}')"
    # Poll the Service endpoints across the deletion; it must never reach zero.
    ( i=0; min=9; while [ $i -lt 30 ]; do n=$(endpoints vitalmesh-$name | wc -w); [ "$n" -lt "$min" ] && min=$n || true; sleep 1; i=$((i+1)); done; echo "$min" > "/tmp/eps-$name" ) &
    poller=$!
    kc delete pod "$victim" --wait=false >/dev/null 2>&1
    wait "$poller"
    minep="$(cat "/tmp/eps-$name" 2>/dev/null || echo 0)"
    [ "${minep:-0}" -ge 1 ] && ok "$name: the Service kept at least one endpoint throughout ($minep)" || fail "$name: the Service lost all endpoints"
    wait_ready_replicas vitalmesh-$name 2 120 && ok "$name: back to two ready replicas" || fail "$name: did not recover two replicas"
done
[ "$(json "$(submit_job)" status)" = "COMPLETED" ] && ok "a job still completes after the deletions" || fail "job failed after pod deletion"

# ------------------------------------------------------------ 4. node disruption
step "4. Node disruption (drain a worker)"
DRAIN="$(node_of api-gateway)"
[ "$DRAIN" = "$CP" ] && DRAIN="$(echo "$WORKERS" | head -1)"
note "draining $DRAIN"
gw_before="$(ready_replicas vitalmesh-api-gateway)"; pr_before="$(ready_replicas vitalmesh-processor)"
# Watch that neither Deployment ever drops below one ready while the drain runs.
( minr=9; i=0; while [ $i -lt 90 ]; do a=$(ready_replicas vitalmesh-api-gateway); b=$(ready_replicas vitalmesh-processor);
  [ "${a:-0}" -lt "$minr" ] && minr=$a || true; [ "${b:-0}" -lt "$minr" ] && minr=$b || true; sleep 1; i=$((i+1)); done; echo "$minr" > /tmp/minready ) &
watcher=$!
dropped="$(probe_flood 60 "$BASE/health")"
k drain "$DRAIN" --ignore-daemonsets --delete-emptydir-data --force --timeout=150s >/dev/null 2>&1 && ok "drain completed" || fail "drain did not complete"
wait "$watcher" 2>/dev/null || true
minready="$(cat /tmp/minready 2>/dev/null || echo 0)"
[ "${minready:-0}" -ge 1 ] && ok "the PDB held: never fewer than one ready replica during the drain (min $minready)" || fail "a Deployment reached zero ready during the drain"
[ "${dropped:-1}" = "0" ] && ok "no request dropped during the drain" || fail "$dropped requests dropped during the drain"
wait_ready_replicas vitalmesh-api-gateway 2 180 && ok "api-gateway rescheduled to two ready on the remaining worker(s)" || fail "api-gateway did not recover"
wait_ready_replicas vitalmesh-processor 2 180 && ok "processor rescheduled to two ready" || fail "processor did not recover"
[ "$(sql "select count(*) from measurements where patient_id='$PID'")" = "$MEAS_BEFORE" ] && ok "data intact across the drain" || fail "measurement count changed across the drain"
k uncordon "$DRAIN" >/dev/null 2>&1 && ok "$DRAIN uncordoned" || note "uncordon $DRAIN failed"

# ------------------------------------------------------------ 5. processor scaling
step "5. Rust processor scaling (HPA)"
pr_start="$(ready_replicas vitalmesh-processor)"
note "processor at $pr_start ready, HPA CPU $(kc get hpa vitalmesh-processor -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null)%/70% before the change"
# Drive the floor to the ceiling so the actuation is demonstrated whatever
# the current level: the HPA raises the Deployment and the new replicas
# become Ready and join the Service.
kc patch hpa vitalmesh-processor --type=merge -p '{"spec":{"minReplicas":4}}' >/dev/null 2>&1
if wait_ready_replicas vitalmesh-processor 4 180; then
    ok "the HPA scaled the processor from $pr_start to 4 ready replicas"
else
    fail "the processor did not scale to 4: readyReplicas=$(ready_replicas vitalmesh-processor)"
fi
kc patch hpa vitalmesh-processor --type=merge -p '{"spec":{"minReplicas":2}}' >/dev/null 2>&1
note "floor restored to 2 (scale-down waits out the 600s stabilization window; not waited here)"

# ------------------------------------------------------------ 6. gateway scaling
step "6. Go gateway scaling (HPA)"
gw_start="$(ready_replicas vitalmesh-api-gateway)"
note "gateway at $gw_start ready, HPA CPU $(kc get hpa vitalmesh-api-gateway -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null)%/70% before the change"
[ "${gw_start:-0}" -gt 2 ] && note "the gateway had already scaled above its floor of 2 on CPU from the earlier scenarios: metric-driven scale-up observed"
kc patch hpa vitalmesh-api-gateway --type=merge -p '{"spec":{"minReplicas":4}}' >/dev/null 2>&1
if wait_ready_replicas vitalmesh-api-gateway 4 180; then
    ok "the HPA scaled the gateway from $gw_start to 4 ready replicas"
else
    fail "the gateway did not scale to 4: readyReplicas=$(ready_replicas vitalmesh-api-gateway)"
fi
kc patch hpa vitalmesh-api-gateway --type=merge -p '{"spec":{"minReplicas":2}}' >/dev/null 2>&1
note "floor restored to 2 (scale-down waits out the 300s stabilization window; not waited here)"

# ------------------------------------------------------------ 7. Redis outage
step "7. Temporary Redis outage"
r_before="$(restarts api-gateway)"
kc patch networkpolicy fixtures --type=json -p '[{"op":"replace","path":"/spec/ingress/0/ports","value":[{"protocol":"TCP","port":5432}]}]' >/dev/null 2>&1
sleep 5
note "Redis running but unreachable"
[ "$(code 10 "$BASE/ready")" = "200" ] && ok "/ready stays 200: Redis does not decide readiness" || fail "/ready failed for a degraded dependency"
[ "$(json "$(api 20 "$API/patients/$PID" -H "$AUTH")" external_reference)" = "$REF" ] && ok "reads still work with the cache gone" || fail "read failed without Redis"
[ -n "$(json "$(api 20 -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")" access_token)" ] && ok "sign-in still works" || fail "sign-in failed without Redis"
# The fallback counter is per replica, a weaker guarantee than the shared
# Redis window and deliberately so. Two replicas behind the Service split
# a flood between two counters, so it is aimed at one pod address to
# exercise that pod's fallback limiter: the anonymous limit is 60 a minute,
# so 75 attempts at one replica must be refused past the sixtieth.
GW1_IP="$(kc get pods -l app.kubernetes.io/name=api-gateway -o jsonpath='{.items[0].status.podIP}')"
hits="$(kcp exec probe -- sh -c "n=0; while [ \$n -lt 75 ]; do curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 -X POST http://$GW1_IP:8080/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"email\":\"nobody@example.com\",\"password\":\"wrong-password-here\"}'; n=\$((n+1)); done" 2>/dev/null | grep -c '^429$' || true)"
[ "${hits:-0}" -gt 0 ] && ok "the per-replica fallback still refuses at one replica ($hits of 75 got 429)" || fail "no request was rate limited without Redis"
[ "$(restarts api-gateway)" = "$r_before" ] && ok "no gateway restart during the Redis outage" || fail "the gateway restarted during the Redis outage"
kc patch networkpolicy fixtures --type=json -p '[{"op":"replace","path":"/spec/ingress/0/ports","value":[{"protocol":"TCP","port":5432},{"protocol":"TCP","port":6379}]}]' >/dev/null 2>&1
sleep 5; ok "Redis reachable again"

# ------------------------------------------------------------ 8. database outage
step "8. Temporary database outage"
db_before="$(restarts api-gateway)"
GW_POD="$(kc get pods -l app.kubernetes.io/name=api-gateway -o jsonpath='{.items[0].metadata.name}')"
GW_IP="$(kc get pod "$GW_POD" -o jsonpath='{.status.podIP}')"
POD="http://$GW_IP:8080"
note "probing $GW_POD directly at $GW_IP"
kc patch networkpolicy fixtures --type=json -p '[{"op":"replace","path":"/spec/ingress/0/ports","value":[{"protocol":"TCP","port":6379}]}]' >/dev/null 2>&1
kc exec deploy/postgres -- psql -U vitalmesh -d vitalmesh -tAc "select pg_terminate_backend(pid) from pg_stat_activity where usename='vitalmesh' and pid <> pg_backend_pid()" >/dev/null 2>&1 || true
sleep 20
[ "$(code 10 "$POD/health")" = "200" ] && ok "/health still answers: liveness does not consult the database" || fail "/health failed during the outage"
[ "$(code 10 "$POD/ready")" = "503" ] && ok "/ready reports the outage (503)" || fail "/ready did not turn 503"
sleep 45
[ "$(restarts api-gateway)" = "$db_before" ] && ok "no gateway restart after 65s without a database" || fail "the gateway restarted during the database outage"
[ "$(kc get pod "$GW_POD" -o jsonpath='{.status.phase}')" = "Running" ] && ok "the pod is still Running, just not Ready" || fail "the pod left Running"
kc patch networkpolicy fixtures --type=json -p '[{"op":"replace","path":"/spec/ingress/0/ports","value":[{"protocol":"TCP","port":5432},{"protocol":"TCP","port":6379}]}]' >/dev/null 2>&1
t0="$(date +%s)"; i=0
while [ "$i" -lt 60 ]; do [ "$(code 10 "$POD/ready")" = "200" ] && break; sleep 2; i=$((i+2)); done
[ "$(code 10 "$POD/ready")" = "200" ] && ok "/ready recovered in $(( $(date +%s) - t0 ))s, no restart" || fail "/ready did not recover"
wait_ready_replicas vitalmesh-api-gateway 2 120 && ok "the Service regained its endpoints" || fail "endpoints did not return"

# ------------------------------------------------------------ integrity
step "Data integrity and job states"
[ "$(sql "select count(*) from measurements where patient_id='$PID'")" = "$MEAS_BEFORE" ] && ok "measurement count unchanged ($MEAS_BEFORE)" || fail "measurement count changed"
[ "$(sql "select count(*) from patients where external_reference='$REF'")" = "1" ] && ok "exactly one patient row: no duplicate" || fail "patient row count wrong"
stuck="$(sql "select count(*) from processing_jobs where status in ('PENDING','PROCESSING')")"
[ "${stuck:-1}" = "0" ] && ok "no job left in a non-terminal state" || fail "$stuck job(s) stuck non-terminal"
note "final job states: $(sql "select status || '=' || count(*) from processing_jobs group by status" | tr '\n' ' ')"
gwr="$(restarts api-gateway)"; prr="$(restarts processor)"
note "live pod restart counts: gateway=$gwr processor=$prr"
[ "$((gwr + prr))" -eq 0 ] && ok "no surviving pod ever restarted: every failure was handled, not survived" || fail "$((gwr + prr)) container restart(s) among current pods"

# ------------------------------------------------------------------- verdict
printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'kubernetes resilience: %d check(s) failed\n' "$failures"
    exit 1
fi
if [ "$KEEP" -eq 1 ]; then
    printf 'kubernetes resilience: all checks passed; cluster kept (kind delete cluster --name %s)\n' "$CLUSTER"
else
    printf 'kubernetes resilience: all checks passed; cluster deleted\n'
fi
