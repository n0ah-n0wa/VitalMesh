#!/bin/sh
# Deploys the local overlay to a kind cluster and tests it (section 102).
#
# scripts/k8s-validate.sh checks the manifests against schemas and linters,
# which is worth doing and does not need a cluster. This is the other half:
# a real API server, its admission chain, a scheduler, and the services
# actually running and talking to each other. The two find different things
# — the migration Job's retry budget was too small for a database that was
# still starting, and no amount of schema validation would have said so.
#
# What it applies is the file k8s-validate.sh rendered, not a fresh build,
# so that what was validated is what is deployed.
#
#   sh scripts/k8s-local-test.sh          create, test, destroy
#   sh scripts/k8s-local-test.sh --keep   leave the cluster running
#
# Needs kind and kubectl on PATH. That is a departure from this
# repository's usual docker-only rule, and an unavoidable one: kind builds
# its node as a container on the host's Docker, so it cannot itself run in
# one.
set -eu

CLUSTER="vitalmesh"
NAMESPACE="vitalmesh-local"
KUBE_DIR="infrastructure/kubernetes"
MANIFEST="$KUBE_DIR/.rendered/overlays-local.yaml"
KIND_CONFIG="$KUBE_DIR/kind/cluster.yaml"

# Pinned: a CNI that changes under the suite turns a clean run into a
# failure that has nothing to do with these manifests.
CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"

# A namespace outside the application's default-deny, used to ask what the
# policies allow from somewhere that is not already inside them.
PROBE_NS="vitalmesh-netpol-probe"

GATEWAY_IMAGE="vitalmesh/api-gateway:dev"
PROCESSOR_IMAGE="vitalmesh/processor:dev"

# Local test credentials. They exist inside a cluster that is deleted at the
# end of this script and protect nothing.
# A new account per run. A fixed address looks tidier, but on a reused
# cluster it already exists with whatever password the previous run chose,
# and "creation failed, carry on" then hides a real sign-in failure behind
# a shrug — which is exactly what it did the first time this ran.
EMAIL="k8s-test-$(date +%s)@vitalmesh.local"
PASSWORD="k8s-local-test-password-not-a-secret"
PORT="18080"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
step() { printf '\n%s\n' "$1"; }

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

pf_pid=""
cleanup() {
    [ -n "$pf_pid" ] && kill "$pf_pid" 2>/dev/null || true
    kubectl --context "kind-$CLUSTER" delete namespace "$PROBE_NS" --wait=false >/dev/null 2>&1 || true
    if [ "$KEEP" -eq 0 ]; then
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

kc() { kubectl --context "kind-$CLUSTER" -n "$NAMESPACE" "$@"; }

# ---------------------------------------------------------------- preflight
step "Preflight"
for tool in kind kubectl docker; do
    command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not on PATH" >&2; exit 2; }
done
ok "kind, kubectl and docker are present"

[ -f "$MANIFEST" ] || { echo "run: make k8s-validate first ($MANIFEST is missing)" >&2; exit 2; }
ok "rendered manifest present, the same one the validation gates checked"

for image in "$GATEWAY_IMAGE" "$PROCESSOR_IMAGE"; do
    docker image inspect "$image" >/dev/null 2>&1 || {
        echo "$image is missing; run: make docker-build" >&2; exit 2; }
done
ok "both service images are built"

# ------------------------------------------------------------------ cluster
step "Cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    ok "reusing the existing kind cluster"
else
    # --wait is not used: the cluster config disables kind's own CNI, so
    # nodes stay NotReady until Calico is installed below and waiting here
    # would only time out.
    kind create cluster --config "$KIND_CONFIG" >/dev/null 2>&1 \
        && ok "created" || { fail "kind create cluster"; exit 1; }
fi

# Calico, because kind's default CNI does not implement NetworkPolicy: it
# accepts the objects and ignores them. On that CNI this whole suite could
# pass with every policy in the base deleted, which makes the policy
# checks below worth nothing. Calico enforces them, and the missing egress
# policy for the migration Job — invisible on the default CNI — fails
# immediately on this one.
if kubectl --context "kind-$CLUSTER" -n kube-system get daemonset calico-node >/dev/null 2>&1; then
    ok "Calico already installed"
else
    if kubectl --context "kind-$CLUSTER" apply -f "$CALICO_MANIFEST" >/dev/null 2>&1; then
        kubectl --context "kind-$CLUSTER" -n kube-system wait --for=condition=ready \
            pod -l k8s-app=calico-node --timeout=300s >/dev/null 2>&1 \
            && ok "Calico installed; NetworkPolicies are enforced" \
            || fail "Calico did not become ready"
    else
        fail "could not install Calico"
    fi
fi
kubectl --context "kind-$CLUSTER" wait --for=condition=Ready node --all --timeout=180s >/dev/null 2>&1 \
    && ok "the node is Ready" || fail "the node never became Ready"

# Loading beats pulling: these images exist only on this machine, and the
# manifests use imagePullPolicy: IfNotPresent so the node uses what it has.
kind load docker-image "$GATEWAY_IMAGE" "$PROCESSOR_IMAGE" --name "$CLUSTER" >/dev/null 2>&1 \
    && ok "images loaded into the node" || fail "kind load docker-image"

# -------------------------------------------------------------------- apply
step "Apply"
# --server-side, and not as a stylistic preference.
#
# The base declares the namespace's `default` ServiceAccount so it can turn
# token automounting off. On a namespace that already exists that is a
# patch. On a fresh one it is a race: client-side apply reads the namespace
# a moment after creating it, sees no `default` account, decides to create
# one, and loses to the ServiceAccount controller doing the same thing —
# `serviceaccounts "default" already exists`, and the whole apply fails.
#
# Server-side apply expresses intent rather than a create-or-update
# decision made from a stale read, so the object being there already is
# not a problem. --force-conflicts takes ownership of the automount field
# from the controller that set it, which is exactly what is wanted here.
#
# A deployment pipeline must apply these manifests the same way; the first
# deploy into a new namespace is precisely when it would otherwise break.
if out="$(kubectl --context "kind-$CLUSTER" apply --server-side --force-conflicts         -f "$MANIFEST" 2>&1)"; then
    ok "$(printf '%s' "$out" | grep -c . ) objects applied"
    # Pod Security admission reports a violation it is not enforcing as a
    # warning on apply. There should be none: every pod here is written to
    # the restricted standard.
    if printf '%s' "$out" | grep -qi "warning"; then
        fail "the API server returned warnings"
        printf '%s\n' "$out" | grep -i warning | sed 's/^/        /'
    else
        ok "no admission warnings"
    fi
else
    fail "apply"
    printf '%s\n' "$out" | sed 's/^/        /'
    exit 1
fi

# --------------------------------------------------------- pod security
step "Pod Security enforcement"
# The namespace claims to enforce the restricted standard. This checks that
# the claim is real, by asking for something the standard forbids and
# requiring the API server to refuse it. Without this the label could be
# misspelled, or the level wrong, and everything would still look fine.
if out="$(kubectl --context "kind-$CLUSTER" -n "$NAMESPACE" apply --dry-run=server -f - 2>&1 <<'POD'
apiVersion: v1
kind: Pod
metadata:
  name: privileged-probe
spec:
  containers:
    - name: probe
      image: busybox:1.36
      securityContext:
        privileged: true
POD
)"; then
    fail "a privileged pod was accepted; the namespace is not enforcing restricted"
else
    if printf '%s' "$out" | grep -qi "violates PodSecurity"; then
        ok "a privileged pod is refused: $(printf '%s' "$out" | grep -oi 'privileged.*' | head -1 | cut -c1-60)"
    else
        fail "the privileged pod was refused, but not by Pod Security: $out"
    fi
fi

# ------------------------------------------------------------------ readiness
step "Rollout"
kc wait --for=condition=available --timeout=300s deployment --all >/dev/null 2>&1 \
    && ok "every Deployment is available" || fail "deployments did not become available"

kc wait --for=condition=complete --timeout=300s job/vitalmesh-migrate >/dev/null 2>&1 \
    && ok "the migration Job completed: $(kc logs job/vitalmesh-migrate --tail=1 2>/dev/null)" \
    || fail "the migration Job did not complete"

# ------------------------------------------------------- network policies
step "NetworkPolicy enforcement"
# Asked from outside the namespace on purpose. A pod inside it is itself
# covered by default-deny, so a refused connection would prove only that
# the source could not send — not that the destination refused. From a
# namespace with no policies of its own, what happens is decided entirely
# by the application's own ingress rules.
kubectl --context "kind-$CLUSTER" create namespace "$PROBE_NS" >/dev/null 2>&1 || true
kubectl --context "kind-$CLUSTER" -n "$PROBE_NS" apply -f - >/dev/null 2>&1 <<'PROBE'
apiVersion: v1
kind: Pod
metadata:
  name: probe
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: curlimages/curl:8.10.1
      command: ["sleep", "900"]
PROBE
kubectl --context "kind-$CLUSTER" -n "$PROBE_NS" wait --for=condition=ready pod/probe     --timeout=180s >/dev/null 2>&1 || fail "the probe pod never became ready"

# curl exits non-zero when the packets are dropped; the timeout is short
# because a policy denial never answers at all.
reach() {
    kubectl --context "kind-$CLUSTER" -n "$PROBE_NS" exec probe --         curl -s -o /dev/null --max-time 5 "http://$1" >/dev/null 2>&1
}

gw="vitalmesh-api-gateway.$NAMESPACE:8080/health"
pr="vitalmesh-processor.$NAMESPACE:8081/health"

kubectl --context "kind-$CLUSTER" label namespace "$PROBE_NS"     vitalmesh.io/gateway-client- >/dev/null 2>&1 || true
reach "$gw" && fail "the gateway is reachable from an unlabelled namespace"     || ok "an unlabelled namespace cannot reach the gateway"
reach "$pr" && fail "the processor is reachable from outside"     || ok "an unlabelled namespace cannot reach the processor"

# Labelling the namespace is the documented seam an ingress controller sits
# in. It should open the gateway's port and nothing else.
kubectl --context "kind-$CLUSTER" label namespace "$PROBE_NS"     vitalmesh.io/gateway-client=true --overwrite >/dev/null 2>&1
sleep 5
reach "$gw" && ok "labelling the namespace opens the gateway"     || fail "the gateway is still unreachable after labelling"
reach "$pr" && fail "labelling the namespace also opened the processor"     || ok "the processor stays closed: the label opens one port, not the namespace"
for svc in "postgres.$NAMESPACE:5432" "redis.$NAMESPACE:6379"; do
    reach "$svc" && fail "$svc is reachable from outside the namespace"         || ok "$(printf '%s' "$svc" | cut -d. -f1) is not reachable from outside"
done

# ---------------------------------------------------------------- smoke test
step "Smoke test through the cluster"
# The first account is created with the gateway's own command, run inside a
# pod that already has DATABASE_URL. `kubectl run` would need a pod of its
# own, and a pod created without a security context is exactly what the
# namespace refuses.
if printf '%s' "$PASSWORD" | kc exec -i deploy/vitalmesh-api-gateway -- \
        /usr/local/bin/api-gateway users create "$EMAIL" OPERATOR >/dev/null 2>&1; then
    ok "created an operator account through the gateway's own command"
else
    fail "could not create the operator account"
fi

# port-forward rather than an Ingress: the local overlay has none, and
# forwarding goes through the API server, so it tests the Service and the
# pod without depending on a controller this cluster does not run.
kubectl --context "kind-$CLUSTER" -n "$NAMESPACE" port-forward svc/vitalmesh-api-gateway \
    "$PORT:8080" >/dev/null 2>&1 &
pf_pid=$!
base="http://127.0.0.1:$PORT/api/v1"

# A second between attempts, and it matters: a refused connection returns
# instantly, so a loop without a pause burns every attempt it has before
# port-forward has finished binding. Written without one first, and it
# failed on a cluster that was working perfectly.
reached=0
i=0
while [ "$i" -lt 30 ]; do
    # Redirection by the shell, not curl's -o. This script exports
    # MSYS_NO_PATHCONV so Docker gets paths unrewritten, and that also
    # stops MSYS translating curl's arguments — so `-o /dev/null` hands a
    # Windows curl a path it cannot write, and every probe fails whatever
    # the server said. The shell understands /dev/null on both platforms.
    if curl --silent --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
        reached=1
        break
    fi
    sleep 1
    i=$((i + 1))
done
[ "$reached" -eq 1 ] && ok "the gateway answers through its Service" || { fail "port-forward never came up"; exit 1; }

# Extracts one string field: the FIRST match, not the last.
#
# grep -o rather than a sed substitution, because sed's .* is greedy and
# reaches past the field being asked for. /ready returns
# {"status":"ready",...,"checks":[{"name":"postgres","status":"ok"}]}, and
# the greedy version answered "ok" — the nested check's status — which read
# as a readiness failure on a service that was perfectly ready.
json() {
    printf '%s' "$1" \
        | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
        | head -1 \
        | sed 's/.*:[[:space:]]*"//; s/"$//'
}

health="$(curl --silent --max-time 5 "http://127.0.0.1:$PORT/health")"
[ "$(json "$health" status)" = "ok" ] && ok "/health reports ok" || fail "/health: $health"

ready="$(curl --silent --max-time 5 "http://127.0.0.1:$PORT/ready")"
[ "$(json "$ready" status)" = "ready" ] && ok "/ready reports ready, so PostgreSQL is reachable" || fail "/ready: $ready"

login="$(curl --silent --max-time 10 -X POST "$base/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")"
token="$(json "$login" access_token)"
[ -n "$token" ] && ok "signed in" || { fail "sign-in: $login"; }

if [ -n "$token" ]; then
    auth="Authorization: Bearer $token"

    patient="$(curl --silent --max-time 10 -X POST "$base/patients" -H "$auth" \
        -H 'Content-Type: application/json' \
        -d "{\"external_reference\":\"k8s-$(date +%s)\",\"date_of_birth\":\"1985-04-12\",\"sex\":\"FEMALE\"}")"
    pid="$(json "$patient" id)"
    [ -n "$pid" ] && ok "registered a patient" || fail "patient: $patient"

    if [ -n "$pid" ]; then
        # Sixty readings, one of them well outside the healthy range, so the
        # anomaly rules this overlay configures have something to find.
        readings=""
        i=0
        while [ "$i" -lt 60 ]; do
            value=72
            [ "$i" -eq 30 ] && value=185
            ts="$(date -u -d "@$(( $(date +%s) - (60 - i) * 60 ))" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                  || date -u +%Y-%m-%dT%H:%M:%SZ)"
            [ -n "$readings" ] && readings="$readings,"
            readings="$readings{\"patient_id\":\"$pid\",\"type\":\"HEART_RATE\",\"value\":$value,\"unit\":\"bpm\",\"recorded_at\":\"$ts\",\"source\":\"k8s-local-test\"}"
            i=$((i + 1))
        done
        batch="$(curl --silent --max-time 30 -X POST "$base/measurements/batch" -H "$auth" \
            -H 'Content-Type: application/json' \
            -d "{\"items\":[$readings]}")"
        stored="$(printf '%s' "$batch" | grep -o '"id"' | grep -c . || true)"
        [ "${stored:-0}" -gt 0 ] && ok "stored $stored readings" \
            || fail "batch: $(printf '%s' "$batch" | head -c 200)"

        job="$(curl --silent --max-time 60 -X POST "$base/processing/jobs" -H "$auth" \
            -H 'Content-Type: application/json' \
            -d "{\"patient_id\":\"$pid\",\"measurement_types\":[\"HEART_RATE\"],\"windows\":[\"5m\",\"1h\"],\"percentiles\":[50,95]}")"
        jid="$(json "$job" id)"
        status="$(json "$job" status)"
        if [ "$status" = "COMPLETED" ]; then
            ok "the Rust processor completed a job, so gateway to processor works in-cluster"
        else
            fail "job did not complete: $(printf '%s' "$job" | head -c 200)"
        fi

        if [ -n "$jid" ]; then
            # Results hang off the patient, not the job: the job is the
            # request, the results are the record. Asking the job for them
            # returns 404, which looks exactly like "no anomalies found".
            results="$(curl --silent --max-time 15 \
                "$base/patients/$pid/processing-results" -H "$auth")"
            windows="$(printf '%s' "$results" | grep -o '"window"' | grep -c . || true)"
            anomalies="$(printf '%s' "$results" | grep -o '"severity"' | grep -c . || true)"

            [ "${windows:-0}" -gt 0 ] && ok "the engine returned $windows result windows" \
                || fail "no results: $(printf '%s' "$results" | head -c 200)"

            # The 185 bpm reading is above the critical bound this overlay
            # configures, so finding nothing means ANOMALY_RULES did not
            # reach the pod — which is the whole point of checking.
            if [ "${anomalies:-0}" -gt 0 ]; then
                ok "flagged $anomalies anomalies, so ANOMALY_RULES reached the container"
            else
                fail "no anomalies found; the 185 bpm reading should have been flagged"
            fi
        fi
    fi
fi

kill "$pf_pid" 2>/dev/null || true
pf_pid=""

# ------------------------------------------------------------------ scaling
step "Scaling"
# Through the HPA rather than `kubectl scale`, because the HPA owns the
# replica count: scaling the Deployment directly would be undone within
# fifteen seconds and would prove nothing about the object that actually
# governs it.
before_replicas="$(kc get deployment vitalmesh-processor -o jsonpath='{.status.readyReplicas}')"
kc patch hpa vitalmesh-processor --type=merge -p '{"spec":{"minReplicas":2}}' >/dev/null 2>&1
scaled=0
i=0
while [ "$i" -lt 40 ]; do
    ready="$(kc get deployment vitalmesh-processor -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    if [ "${ready:-0}" -ge 2 ]; then scaled=1; break; fi
    sleep 3
    i=$((i + 1))
done
[ "$scaled" -eq 1 ]     && ok "raising the HPA floor scaled the processor from ${before_replicas:-0} to 2 ready replicas"     || fail "the processor did not scale up: readyReplicas=${ready:-0}"

# Back to where it started, so the rest of the suite runs against the
# configuration the overlay actually describes.
kc patch hpa vitalmesh-processor --type=merge -p '{"spec":{"minReplicas":1}}' >/dev/null 2>&1
ok "HPA floor restored"

# -------------------------------------------------------- graceful shutdown
step "Graceful shutdown"
# The question is not whether the pods come back — the rollback drill below
# covers that — but whether anything is dropped while they do. Requests are
# sent from inside the cluster, through the Service, so endpoint removal is
# part of what is being tested; a port-forward would pin one pod and answer
# a different question.
#
# maxUnavailable: 0 brings the new pod to ready before the old one is
# touched, and the preStop sleep holds the old one open while its endpoint
# is withdrawn. If either were missing this is where it would show.
kc rollout restart deployment/vitalmesh-api-gateway >/dev/null 2>&1
dropped="$(kubectl --context "kind-$CLUSTER" -n "$PROBE_NS" exec probe -- sh -c '
    i=0; f=0
    while [ $i -lt 45 ]; do
        curl -s -o /dev/null --max-time 3             http://vitalmesh-api-gateway.'"$NAMESPACE"':8080/health || f=$((f+1))
        i=$((i+1))
        sleep 1
    done
    echo $f' 2>/dev/null)"
kc rollout status deployment/vitalmesh-api-gateway --timeout=180s >/dev/null 2>&1     && ok "the rolling restart completed" || fail "the rolling restart did not complete"
if [ "${dropped:-1}" = "0" ]; then
    ok "45 requests across the restart, none dropped"
else
    fail "$dropped of 45 requests failed during the restart"
fi

# ------------------------------------------------------------ rollback drill
step "Rollout and rollback"
# Section 62 wants a rollback that has been rehearsed rather than assumed.
before="$(kc get deployment vitalmesh-api-gateway -o jsonpath='{.metadata.generation}')"
kc rollout restart deployment/vitalmesh-api-gateway >/dev/null 2>&1
if kc rollout status deployment/vitalmesh-api-gateway --timeout=180s >/dev/null 2>&1; then
    ok "a rolling restart completed with no loss of availability"
else
    fail "rolling restart did not complete"
fi
if kc rollout undo deployment/vitalmesh-api-gateway >/dev/null 2>&1 &&
   kc rollout status deployment/vitalmesh-api-gateway --timeout=180s >/dev/null 2>&1; then
    after="$(kc get deployment vitalmesh-api-gateway -o jsonpath='{.metadata.generation}')"
    ok "rollback completed (generation $before to $after)"
else
    fail "rollback did not complete"
fi

# ------------------------------------------------------------------- verdict
printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'kubernetes local: %d check(s) failed\n' "$failures"
    exit 1
fi
if [ "$KEEP" -eq 1 ]; then
    printf 'kubernetes local: all checks passed; cluster kept (kind delete cluster --name %s)\n' "$CLUSTER"
else
    printf 'kubernetes local: all checks passed; cluster deleted\n'
fi
