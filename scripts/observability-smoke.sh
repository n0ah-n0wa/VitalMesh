#!/bin/sh
# Checks that the local observability stack is actually observing something.
#
# It asserts what a human would otherwise check by clicking: that Prometheus
# is scraping both services, that the rules loaded and evaluated, that the
# metrics the dashboards read exist with samples, that Grafana has its
# datasource and dashboards, and that the collector received spans.
#
# It is deliberately not part of `make verify`. The observability stack is
# optional for correctness, so no build or test gate may depend on it.
#
# Usage:
#   make observability-up          # the stack
#   ./bin/api-gateway &            # and the two services, exporting to it
#   ./services/processor/target/debug/processor &
#   make observability-smoke
set -eu

PROMETHEUS="${PROMETHEUS_URL:-http://localhost:9090}"
GRAFANA="${GRAFANA_URL:-http://localhost:3000}"
COLLECTOR="${COLLECTOR_URL:-http://localhost:8888}"

failures=0

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

# get URL -> body on stdout, empty on failure.
get() { curl --silent --show-error --fail --max-time 10 "$1" 2>/dev/null || true; }

# A Prometheus instant query, returning the number of series in the result.
# jq is not assumed: the result is counted by its metric objects.
query_count() {
    encoded=$(printf '%s' "$1" | sed 's/ /%20/g; s/{/%7B/g; s/}/%7D/g; s/"/%22/g; s/=/%3D/g; s/~/%7E/g; s/|/%7C/g; s/+/%2B/g; s/(/%28/g; s/)/%29/g; s/\[/%5B/g; s/\]/%5D/g; s/,/%2C/g; s/\//%2F/g')
    body=$(get "$PROMETHEUS/api/v1/query?query=$encoded")
    printf '%s' "$body" | grep -o '"metric"' | wc -l | tr -d ' '
}

echo
echo "Prometheus ($PROMETHEUS)"

if [ -n "$(get "$PROMETHEUS/-/healthy")" ]; then
    pass "is healthy"
else
    fail "is not answering; start it with 'make observability-up'"
    echo
    echo "$failures check(s) failed."
    exit 1
fi

# Every configured target must be up. A target that is down is either a
# service that is not running or a scrape configuration that is wrong, and
# both make every dashboard below it empty.
for job in api-gateway processor otel-collector prometheus; do
    if [ "$(query_count "up{job=\"$job\"} == 1")" -ge 1 ]; then
        pass "is scraping $job"
    else
        fail "is not scraping $job (is the service running and exporting /metrics?)"
    fi
done

# Rules must have loaded and evaluated. A rule file with a typo loads as
# zero groups, which is silent until an alert fails to fire.
rules=$(get "$PROMETHEUS/api/v1/rules")
if printf '%s' "$rules" | grep -q '"name":"api-gateway.traffic"'; then
    pass "loaded the recording rules"
else
    fail "did not load the recording rules"
fi
if printf '%s' "$rules" | grep -q '"name":"GatewayServerErrorsHigh"'; then
    pass "loaded the alerting rules"
else
    fail "did not load the alerting rules"
fi
if printf '%s' "$rules" | grep -q '"health":"err"'; then
    fail "has a rule that failed to evaluate"
else
    pass "evaluated every rule without error"
fi

echo
echo "Metrics the dashboards read"

# One representative series per dashboard, so a dashboard that would render
# empty fails here instead of in front of someone.
for expr in \
    "vitalmesh_http_requests_total" \
    "vitalmesh_http_request_duration_seconds_bucket" \
    "vitalmesh_database_query_duration_seconds_bucket" \
    "vitalmesh_redis_command_duration_seconds_count" \
    "vitalmesh_rate_limit_decisions_total" \
    "vitalmesh_operation_in_flight" \
    "vitalmesh_processor_jobs_total" \
    "vitalmesh_processor_active_jobs" \
    "vitalmesh_processor_job_capacity" \
    "go_goroutines" \
    "process_cpu_seconds_total"
do
    if [ "$(query_count "$expr")" -ge 1 ]; then
        pass "$expr has samples"
    else
        fail "$expr has no samples"
    fi
done

echo
echo "Bounded cardinality"

# The method is the one label a client controls. Sending invented methods
# must not create series; if it does, anyone can grow the metric without
# bound. The services are reached through the same host the scrape uses.
GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
PROCESSOR="${PROCESSOR_URL:-http://localhost:8081}"
for method in SMOKEA SMOKEB SMOKEC; do
    curl --silent --output /dev/null --max-time 5 -X "$method" "$GATEWAY/health" || true
    curl --silent --output /dev/null --max-time 5 -X "$method" "$PROCESSOR/health" || true
done
for target in "$GATEWAY gateway" "$PROCESSOR processor"; do
    set -- $target
    if get "$1/metrics" | grep -q 'method="SMOKE'; then
        fail "$2 created a series for an invented method: the method label is unbounded"
    else
        pass "$2 folds invented methods into one bucket"
    fi
done

echo
echo "Recorded rules"

for rule in \
    "job:gateway_http_requests:rate5m" \
    "job:gateway_http_latency:p99" \
    "job:gateway_http_server_error_ratio:rate5m" \
    "job:processor_jobs:rate5m" \
    "job:processor_saturation:ratio"
do
    if [ "$(query_count "$rule")" -ge 1 ]; then
        pass "$rule is being recorded"
    else
        fail "$rule is not being recorded yet (rules need one evaluation interval)"
    fi
done

echo
echo "Grafana ($GRAFANA)"

if printf '%s' "$(get "$GRAFANA/api/health")" | grep -q '"database": *"ok"'; then
    pass "is healthy"
else
    fail "is not answering"
fi
if printf '%s' "$(get "$GRAFANA/api/datasources")" | grep -q 'vitalmesh-prometheus'; then
    pass "has the Prometheus datasource provisioned"
else
    fail "has no Prometheus datasource"
fi
dashboards=$(get "$GRAFANA/api/search?type=dash-db")
for uid in vitalmesh-api-traffic vitalmesh-latency-errors vitalmesh-processing vitalmesh-infrastructure; do
    if printf '%s' "$dashboards" | grep -q "$uid"; then
        pass "has dashboard $uid"
    else
        fail "is missing dashboard $uid"
    fi
done

echo
echo "OpenTelemetry collector ($COLLECTOR)"

collector=$(get "$COLLECTOR/metrics")
if [ -n "$collector" ]; then
    pass "is exposing its own metrics"
else
    fail "is not answering"
fi

# Spans arriving is what proves the trace pipeline end to end: the services
# exported, the collector accepted. A stack that has taken no traffic yet
# has none, which is a warning rather than a failure.
accepted=$(printf '%s' "$collector" | awk '/^otelcol_receiver_accepted_spans/ { total += $2 } END { printf "%d", total }')
if [ "${accepted:-0}" -gt 0 ]; then
    pass "has accepted $accepted spans"
else
    printf '  warn  has accepted no spans yet; send a request through the gateway and run this again\n'
fi
if printf '%s' "$collector" | awk '/^otelcol_exporter_send_failed_spans/ { total += $2 } END { exit !(total > 0) }'; then
    printf '  warn  the collector failed to send some spans; requests are unaffected\n'
else
    pass "has not failed to send spans"
fi

echo
if [ "$failures" -eq 0 ]; then
    echo "observability: all checks passed"
    exit 0
fi
echo "observability: $failures check(s) failed"
exit 1
