#!/bin/sh
# Tests the monitoring stack on a real cluster (SPECIFICATIONS.md section
# 104, "enable monitoring"; docs/PRODUCTION_READINESS.md P1).
#
# The reason this script exists is the reason the stack was not built
# earlier. The production-readiness review left metrics collection unbuilt
# and said why: "an untested monitoring stack is worse than a documented
# gap: it looks like coverage." That argument is sound and it is answered by
# testing, not by asserting. kind gives a real API server, a real CNI
# enforcing the NetworkPolicies, and real pods, which is enough to exercise
# every hop of the path that matters:
#
#   both services serve /metrics
#     -> Prometheus discovers them through the Kubernetes API and scrapes
#       -> the recording rules produce series from what it scraped
#         -> the alerting rules evaluate those series
#           -> a real failure makes one fire
#             -> and it arrives at Alertmanager
#
# plus the other half that had nothing on the other end of it: both services
# export spans to a collector whose name now resolves.
#
# What it cannot test is SNS delivery, which needs an AWS account. That is
# stated in infrastructure/kubernetes/monitoring/alertmanager.yaml and in
# docs/OPERATIONS.md rather than papered over: the base configuration here
# notifies nobody, on purpose, and the install script is what adds delivery.
#
#   sh scripts/k8s-monitoring-test.sh          create, test, destroy
#   sh scripts/k8s-monitoring-test.sh --keep   leave the cluster running
#
# Needs kind and kubectl on PATH, for the same unavoidable reason as
# scripts/k8s-local-test.sh: kind builds its node on the host's Docker.
set -eu

CLUSTER="vitalmesh"
NAMESPACE="vitalmesh-local"
MONITORING_NS="monitoring"
KUBE_DIR="infrastructure/kubernetes"
APP_MANIFEST="$KUBE_DIR/.rendered/overlays-local.yaml"
MONITORING_MANIFEST="$KUBE_DIR/.rendered/monitoring.yaml"
KIND_CONFIG="$KUBE_DIR/kind/cluster.yaml"

CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"

GATEWAY_IMAGE="vitalmesh/api-gateway:dev"
PROCESSOR_IMAGE="vitalmesh/processor:dev"
# The local overlay runs its own PostgreSQL and Redis. Loading them
# rather than letting the node pull them is not an optimisation: the
# first run of this script spent its whole gateway rollout budget in
# ImagePullBackOff on postgres:16-alpine and failed before asserting
# anything.
FIXTURE_IMAGES="postgres:16-alpine redis:7-alpine"
PROMETHEUS_IMAGE="prom/prometheus:v3.1.0"
ALERTMANAGER_IMAGE="prom/alertmanager:v0.28.0"
COLLECTOR_IMAGE="otel/opentelemetry-collector-contrib:0.117.0"

# The eleven alerts this repository ships. Named here so that adding one
# and forgetting to ship it to a deployed environment fails.
EXPECTED_ALERTS="ServiceDown GatewayAbsent ProcessorAbsent GatewayServerErrorsHigh GatewayLatencyHigh DatabasePoolNearlyExhausted RedisDegraded ProcessingFailuresHigh ProcessorSaturated ProcessingSlow SpanExportFailing"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
note() { printf '        %s\n' "$1"; }
step() { printf '\n%s\n' "$1"; }

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

# kubectl is a native Windows binary under Git Bash and cannot open an MSYS
# /tmp path. Everything handed to it as a file goes through this.
host_path() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

WORK="$(mktemp -d)"
prom_pf="" am_pf="" gw_pf=""
cleanup() {
    for pid in $prom_pf $am_pf $gw_pf; do kill "$pid" 2>/dev/null || true; done
    rm -rf "$WORK" 2>/dev/null || true
    if [ "$KEEP" -eq 0 ]; then
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

kc()  { kubectl --context "kind-$CLUSTER" -n "$NAMESPACE" "$@"; }
kcm() { kubectl --context "kind-$CLUSTER" -n "$MONITORING_NS" "$@"; }

# Start a port-forward and print the port kubectl chose. Letting it choose
# avoids the stale-forward trap: a killed forward can survive on Windows and
# keep answering from the old pod, so a fixed port can silently describe the
# previous run.
forward() {
    _ns="$1"; _target="$2"; _port="$3"
    # A fresh log file per call, not one named after the target. Forwarding
    # to the same Service twice — which this script does, either side of the
    # restart — reused one file, and `head -1` then returned the port the
    # first, already-killed forward had printed. The caller got a port
    # nothing was listening on and reported the gateway as down while it was
    # serving perfectly well.
    _forward_seq=$((${_forward_seq:-0} + 1))
    _log="$WORK/pf-$_forward_seq-$(printf '%s' "$_target" | tr '/' '-').log"
    kubectl --context "kind-$CLUSTER" -n "$_ns" port-forward "$_target" ":$_port" >"$_log" 2>&1 &
    _pid=$!
    _n=0
    while [ "$_n" -lt 60 ]; do
        _got="$(sed -n 's/^Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "$_log" | head -1)"
        [ -n "$_got" ] && { printf '%s %s' "$_pid" "$_got"; return 0; }
        # A forward whose kubectl has already exited will never print one.
        kill -0 "$_pid" 2>/dev/null || break
        _n=$((_n + 1))
        sleep 1
    done
    kill "$_pid" 2>/dev/null || true
    return 1
}

# Poll until a command succeeds, or give up. Used instead of a flat sleep so
# a fast machine is not punished and a slow one is not failed early.
until_ok() {
    _tries="$1"; shift
    _n=0
    while [ "$_n" -lt "$_tries" ]; do
        if "$@" >/dev/null 2>&1; then return 0; fi
        _n=$((_n + 1))
        sleep 2
    done
    return 1
}

api() { curl -sS --max-time 10 "$@"; }

# ---------------------------------------------------------------- preflight
step "Preflight"
for tool in kind kubectl docker curl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not on PATH" >&2; exit 2; }
done
ok "kind, kubectl, docker and curl are present"

for manifest in "$APP_MANIFEST" "$MONITORING_MANIFEST"; do
    [ -f "$manifest" ] || { echo "run: make k8s-validate first ($manifest is missing)" >&2; exit 2; }
done
ok "both rendered manifests present, the same ones the validation gates checked"

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
    kind create cluster --config "$KIND_CONFIG" >/dev/null 2>&1 \
        && ok "created" || { fail "kind create cluster"; exit 1; }
fi

# Calico rather than kind's default CNI, for the same reason as the local
# suite: the default accepts NetworkPolicy objects and ignores them. This
# whole test would pass with every policy deleted, and the one thing it most
# needs to prove — that the scrape is allowed through a default-deny
# namespace — would prove nothing.
#
# The wait is longer than the local suite's 300s because this test loads five
# images on top of Calico's own, and a machine that has just torn down a
# cluster is still busy doing it. A timeout here is not a soft failure:
# without Calico the NetworkPolicies are decorative, so the run stops rather
# than reporting on policies that nothing enforced.
if kubectl --context "kind-$CLUSTER" -n kube-system get daemonset calico-node >/dev/null 2>&1; then
    ok "Calico already installed; NetworkPolicies are enforced"
else
    if kubectl --context "kind-$CLUSTER" apply -f "$CALICO_MANIFEST" >/dev/null 2>&1; then
        kubectl --context "kind-$CLUSTER" -n kube-system wait --for=condition=ready \
            pod -l k8s-app=calico-node --timeout=420s >/dev/null 2>&1 \
            && ok "Calico installed; NetworkPolicies are enforced" \
            || fail "Calico did not become ready"
    else
        fail "could not install Calico"
    fi
fi
kubectl --context "kind-$CLUSTER" wait --for=condition=Ready node --all --timeout=180s >/dev/null 2>&1 \
    && ok "the node is Ready" || fail "the node never became Ready"

# The monitoring images are pulled once on the host and loaded, rather than
# pulled by the node on every run.
for image in "$PROMETHEUS_IMAGE" "$ALERTMANAGER_IMAGE" "$COLLECTOR_IMAGE" $FIXTURE_IMAGES; do
    docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image" >/dev/null 2>&1 || true
done
kind load docker-image "$GATEWAY_IMAGE" "$PROCESSOR_IMAGE" \
    "$PROMETHEUS_IMAGE" "$ALERTMANAGER_IMAGE" "$COLLECTOR_IMAGE" $FIXTURE_IMAGES \
    --name "$CLUSTER" >/dev/null 2>&1 \
    && ok "application, fixture and monitoring images loaded into the node" \
    || fail "kind load docker-image"

# -------------------------------------------------------------------- apply
step "Apply"
# Server-side, for the reason set out in scripts/k8s-local-test.sh: the
# first apply into a fresh namespace races the ServiceAccount controller
# otherwise.
if out="$(kubectl --context "kind-$CLUSTER" apply --server-side --force-conflicts \
    -f "$(host_path "$PWD/$APP_MANIFEST")" 2>&1)"; then
    ok "application: $(printf '%s' "$out" | grep -c .) objects applied"
else
    fail "applying the application manifest"
    printf '%s\n' "$out" | sed 's/^/        /'
    exit 1
fi

if out="$(kubectl --context "kind-$CLUSTER" apply --server-side --force-conflicts \
    -f "$(host_path "$PWD/$MONITORING_MANIFEST")" 2>&1)"; then
    ok "monitoring: $(printf '%s' "$out" | grep -c .) objects applied"
    # The namespace enforces the restricted Pod Security Standard. A warning
    # here means one of these pods does not meet it, which is a defect in the
    # manifest rather than a note.
    if printf '%s' "$out" | grep -qi "warning"; then
        fail "Pod Security admission warned about the monitoring pods"
        printf '%s' "$out" | grep -i warning | sed 's/^/        /'
    else
        ok "no Pod Security warnings: every monitoring pod meets the restricted standard"
    fi
else
    fail "applying the monitoring manifest"
    printf '%s\n' "$out" | sed 's/^/        /'
    exit 1
fi

# The local overlay ships OTEL_EXPORTER_OTLP_ENDPOINT empty, because until
# now a kind cluster had no collector to export to. Point both services at
# the one that is running, rather than changing that overlay: the other kind
# suite deploys it without a monitoring namespace, and an endpoint that does
# not resolve there would be a name it degrades over for no reason.
#
# Done here, before anything waits on a rollout or opens a port-forward, so
# that each service starts once with its final configuration. Patching after
# the forwards were open meant restarting the pod underneath them and then
# re-establishing one, which is a race this test does not need to run.
step "Configure"
for service in api-gateway processor; do
    kc patch "configmap/vitalmesh-$service" --type merge \
        -p '{"data":{"OTEL_EXPORTER_OTLP_ENDPOINT":"http://otel-collector.monitoring.svc.cluster.local:4318"}}' >/dev/null 2>&1 \
        && ok "$service exports traces to the collector" \
        || fail "could not point $service at the collector"
done
# Mounted configuration is read at start-up, so the pods that are already
# running are still on the old value.
kc rollout restart deployment/vitalmesh-api-gateway deployment/vitalmesh-processor >/dev/null 2>&1 \
    || fail "could not restart the services"

step "Rollout"
kc rollout status deployment/vitalmesh-api-gateway --timeout=300s >/dev/null 2>&1 \
    && ok "the gateway is available" || fail "the gateway never became available"
kc rollout status deployment/vitalmesh-processor --timeout=300s >/dev/null 2>&1 \
    && ok "the processor is available" || fail "the processor never became available"

for deployment in prometheus alertmanager otel-collector; do
    kcm rollout status "deployment/$deployment" --timeout=300s >/dev/null 2>&1 \
        && ok "$deployment is available" || fail "$deployment never became available"
done

if [ "$failures" -ne 0 ]; then
    printf '\nmonitoring: %d check(s) failed before any assertion could run\n' "$failures"
    exit 1
fi

# ---------------------------------------------------------------- forwards
#
# `forward` prints two words, a pid and a port, and the splitting below is
# what separates them, so each unquoted expansion is deliberate. The
# directives say so rather than leaving them to look like an oversight.
step "Connect"
# shellcheck disable=SC2046  # two words, split on purpose
set -- $(forward "$MONITORING_NS" service/prometheus 9090) || { fail "port-forward to Prometheus"; exit 1; }
prom_pf="$1"; PROM="http://127.0.0.1:$2"
ok "Prometheus on $PROM"

# shellcheck disable=SC2046  # two words, split on purpose
set -- $(forward "$MONITORING_NS" service/alertmanager 9093) || { fail "port-forward to Alertmanager"; exit 1; }
am_pf="$1"; AM="http://127.0.0.1:$2"
ok "Alertmanager on $AM"

# shellcheck disable=SC2046  # two words, split on purpose
set -- $(forward "$NAMESPACE" service/vitalmesh-api-gateway 8080) || { fail "port-forward to the gateway"; exit 1; }
gw_pf="$1"; GW="http://127.0.0.1:$2"
ok "gateway on $GW"

until_ok 30 curl -sf --max-time 5 "$PROM/-/ready" && ok "Prometheus is ready" || fail "Prometheus never became ready"
until_ok 30 curl -sf --max-time 5 "$AM/-/ready" && ok "Alertmanager is ready" || fail "Alertmanager never became ready"

# Traffic, so the counters and histograms the rules read have something in
# them, and so the gateway emits spans to the collector.
i=0
while [ "$i" -lt 40 ]; do
    curl -sf --max-time 5 -o /dev/null "$GW/health" 2>/dev/null || true
    i=$((i + 1))
done
ok "generated 40 requests through the gateway"

# ------------------------------------------------------------------ scrape
step "Scrape: Prometheus discovers and reads both services"

# Asserted through the query API rather than by reading /targets. The first
# version of this test parsed the targets JSON with tr and grep, and got it
# wrong in the direction that matters: it reported the collector as not
# scraped while Prometheus had it up. `up` is one series per target, and its
# value is the answer.
promq() { curl -sS -G --max-time 15 "$PROM/api/v1/query" --data-urlencode "query=$1" 2>/dev/null; }

for service in api-gateway processor otel-collector; do
    if until_ok 45 sh -c "curl -sS -G --max-time 15 \"$PROM/api/v1/query\" --data-urlencode \"query=up{service=\\\"$service\\\"}\" | grep -q '\"1\"]'"; then
        ok "$service is a healthy scrape target"
    else
        fail "$service is not being scraped successfully"
        promq "up{service=\"$service\"}" | head -c 300 | sed 's/^/        /'
    fi
done

# Discovery, not a static list: only a target that came from the Kubernetes
# API carries the pod name the relabelling copies into this label.
if promq "up{service=\"api-gateway\"}" | grep -q '"pod":"vitalmesh-api-gateway-'; then
    ok "targets carry a pod label, so discovery is through the Kubernetes API"
else
    fail "no pod label on the gateway target; the scrape config is not discovering through Kubernetes"
    promq "up{service=\"api-gateway\"}" | head -c 300 | sed 's/^/        /'
fi

# -------------------------------------------------------------------- rules
step "Rules: recorded, evaluated, and all eleven loaded"

# A recording rule produces a series only if the scrape worked, the rule
# parsed, and the rule group ran. One value proves the whole chain.
if until_ok 30 sh -c "curl -sS --max-time 10 '$PROM/api/v1/query?query=job:gateway_http_requests:rate5m' | grep -q '\"value\"'"; then
    ok "recording rules are producing series (job:gateway_http_requests:rate5m)"
else
    fail "no recording rule output; the rules are loaded but nothing evaluated"
    promq "job:gateway_http_requests:rate5m" | head -c 300 | sed 's/^/        /'
fi

RULES="$WORK/rules.json"
api "$PROM/api/v1/rules" > "$RULES" 2>/dev/null || true
missing=""
for alert in $EXPECTED_ALERTS; do
    grep -q "\"name\":\"$alert\"" "$RULES" || missing="$missing $alert"
done
if [ -z "$missing" ]; then
    ok "all eleven alerting rules are loaded"
else
    fail "alerting rules missing from the running Prometheus:$missing"
fi

# A rule that failed to evaluate reports its error rather than refusing to
# load, so a green "loaded" count alone would not catch a broken expression.
if grep -q '"lastError":"[^"]' "$RULES"; then
    fail "a rule reported an evaluation error"
    grep -o '"name":"[^"]*","query":"[^"]*","lastError":"[^"]*"' "$RULES" | head -3 | sed 's/^/        /'
else
    ok "no rule reported an evaluation error"
fi

# ------------------------------------------------------------------- traces
step "Traces: the collector name resolves and spans arrive"

# The collector own metrics are the evidence. otelcol_receiver_accepted_spans
# rising proves the services resolved otel-collector.monitoring.svc.cluster.local,
# that the NetworkPolicy on both sides allowed 4318, and that the collector
# accepted what arrived. The series does not exist at all until the first
# span, so its absence and a zero mean the same thing here.
if until_ok 45 sh -c "curl -sS -G --max-time 15 \"$PROM/api/v1/query\" --data-urlencode 'query=sum(otelcol_receiver_accepted_spans_total) or sum(otelcol_receiver_accepted_spans)' | grep -qE '\"[1-9][0-9]*\"]'"; then
    ok "the collector has accepted spans from the services"
else
    fail "no spans reached the collector; the OTLP path is not working"
    promq "sum(otelcol_receiver_accepted_spans_total) or sum(otelcol_receiver_accepted_spans)" \
        | head -c 300 | sed 's/^/        /'
fi

# ------------------------------------------------------------------ alerting
step "Alerting: a real failure fires and reaches Alertmanager"

# Point the gateway's Service at a port nothing is listening on. The pod
# stays up and Ready, so its endpoint stays in discovery and Prometheus keeps
# the target; the scrape is then refused and `up` goes to 0, which is exactly
# the condition ServiceDown describes: "{{ $labels.job }} is not answering
# scrapes". Pointing a Service at the wrong port is also a real way to break
# a service without anything else noticing.
#
# Two other injections were tried first, and both failed for reasons worth
# keeping:
#
#   scale --replicas=0    removes the pod, so the endpoint goes too, so the
#                         target leaves discovery and `up` stops existing
#                         rather than becoming 0. `x == 0` matches nothing
#                         when x is absent, which means ServiceDown is silent
#                         on a service that has vanished entirely. That is a
#                         real gap in the rule, not in this test, and it is
#                         recorded as finding 12 in docs/FINAL_AUDIT.md.
#
#   delete the NetworkPolicy that admits the scrape
#                         blocks new connections and not established ones.
#                         Calico is stateful, and Prometheus holds its scrape
#                         connection open with keep-alive, so scraping
#                         carried on through a policy that no longer allowed
#                         it. Worth knowing before relying on a policy change
#                         to stop traffic that is already flowing.
kc patch service/vitalmesh-api-gateway --type merge \
    -p '{"spec":{"ports":[{"name":"http","port":8080,"targetPort":9,"protocol":"TCP"}]}}' >/dev/null 2>&1 \
    && ok "pointed the gateway Service at a dead port; the pod stays up and Ready" \
    || fail "could not repoint the gateway Service"

note "waiting for ServiceDown to pass its 2m 'for' clause"
# Asked as a query, not by reading /api/v1/alerts. Prometheus publishes a
# synthetic `ALERTS` series for every rule it is evaluating, with
# `alertstate` as a label, so one query answers exactly the question. The
# first version of this check split the alerts JSON on `}` and looked for
# the alert name and the state on the same line — they are in different
# objects, so it reported "never fired" while printing the firing alert
# underneath itself.
fired=0
n=0
while [ "$n" -lt 100 ]; do
    if promq 'ALERTS{alertname="ServiceDown",alertstate="firing"}' | grep -q '"1"]'; then
        fired=1
        break
    fi
    n=$((n + 1))
    sleep 3
done
if [ "$fired" -eq 1 ]; then
    ok "ServiceDown is firing in Prometheus"
else
    fail "ServiceDown never fired"
    promq 'ALERTS{alertname="ServiceDown"}' | head -c 400 | sed 's/^/        /'
fi

# The half that was missing entirely: an alert that fires and goes nowhere
# is the state this work exists to end.
delivered=0
n=0
while [ "$n" -lt 40 ]; do
    if api "$AM/api/v2/alerts" 2>/dev/null | grep -q '"alertname":"ServiceDown"'; then
        delivered=1
        break
    fi
    n=$((n + 1))
    sleep 3
done
if [ "$delivered" -eq 1 ]; then
    ok "the alert arrived at Alertmanager"
    receiver="$(api "$AM/api/v2/alerts" 2>/dev/null | grep -o '"receivers":\[[^]]*\]' | head -1)"
    note "Alertmanager routed it to: ${receiver:-unknown}"
    note "the base configuration notifies nobody; SNS delivery is added by scripts/eks-platform-install.sh"
else
    fail "the alert fired but never reached Alertmanager"
    api "$AM/api/v2/alerts" 2>/dev/null | head -c 300 | sed 's/^/        /'
fi

# Inhibition is configured; check it is in force rather than merely present.
if api "$AM/api/v2/status" 2>/dev/null | grep -q 'inhibit_rules'; then
    ok "Alertmanager loaded its inhibition rules"
else
    note "inhibition rules not visible in the status endpoint; not asserted"
fi

step "Restore"
# Put the Service back by re-applying the manifest it came from, rather than
# writing a second copy of it here that could drift from the first.
kubectl --context "kind-$CLUSTER" apply --server-side --force-conflicts \
    -f "$(host_path "$PWD/$APP_MANIFEST")" >/dev/null 2>&1 \
    && ok "the gateway Service points at its container again" \
    || note "could not restore the Service; the cluster is being deleted anyway"

# Prometheus must also stop alerting once the cause is gone, or every
# incident would need a manual silence.
resolved=0
n=0
while [ "$n" -lt 40 ]; do
    if ! promq 'ALERTS{alertname="ServiceDown",alertstate="firing"}' | grep -q '"1"]'; then
        resolved=1
        break
    fi
    n=$((n + 1))
    sleep 3
done
[ "$resolved" -eq 1 ] && ok "ServiceDown cleared once the Service answered again" \
    || fail "ServiceDown is still firing after the Service was restored"

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'monitoring: %d check(s) failed\n' "$failures"
    exit 1
fi
if [ "$KEEP" -eq 1 ]; then
    printf 'monitoring: the whole path works, from scrape to Alertmanager; cluster kept (kind delete cluster --name %s)\n' "$CLUSTER"
else
    printf 'monitoring: the whole path works, from scrape to Alertmanager\n'
fi
