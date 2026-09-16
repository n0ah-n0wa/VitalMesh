#!/bin/sh
# The VitalMesh demonstration (SPECIFICATIONS.md section 108), end to end,
# against the local environment. docs/DEMO.md walks through what each step
# shows and what to look at afterwards.
#
#   make demo            # or: sh scripts/demo.sh
#
# Twelve steps: start the infrastructure, create a demo account, sign in,
# register a synthetic patient, generate synthetic readings, submit them,
# create a processing job, wait for it, read the results, then inspect the
# metrics, the trace and the logs the run produced. Every step prints the
# request and what came back, so the output is the explanation.
#
# Everything the demo creates is synthetic: the account is on a reserved
# `.invalid` domain, the patient is a reference and a date, the readings are
# a fixed series with one deliberate spike. No real person is described.
#
# It is repeatable: each run uses a fresh patient reference and fresh
# idempotency keys, and the account is created only if it is not there.
# It needs Docker (with compose), curl and a POSIX shell, nothing else.
#
#   DEMO_NO_START=1      skip step 1 (the environment is already running)
#   GATEWAY_URL          where the gateway is (default http://localhost:8080)
#   POSTGRES_PORT etc.   passed through to docker compose (see docs/DEVELOPMENT.md)
#
# Against a deployed environment (the release pipeline runs it against
# staging as a real client) set GATEWAY_URL to it and DEMO_ACCOUNT_EXISTS=1:
# the account is provisioned by the deployment, there is no docker compose
# to start or to read logs from, and steps 1, 2 and 10 to 12 say where the
# same things are found instead of showing them.
set -eu

cd "$(dirname "$0")/.."

GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
PROCESSOR="${PROCESSOR_URL:-http://localhost:${PROCESSOR_PORT:-8081}}"
PROMETHEUS="${PROMETHEUS_URL:-http://localhost:9090}"
EMAIL="${DEMO_EMAIL:-demo@vitalmesh.invalid}"
# A development password for a development account on a local machine. It is
# not a secret, and the account it opens exists only in your own database.
PASSWORD="${DEMO_PASSWORD:-demo-password-demo-password}"
RUN="$(date -u +%Y%m%dT%H%M%SZ)"

# Local: a gateway on this machine, started and observed through docker
# compose. Remote: a deployed gateway reached as a client only.
case "$GATEWAY" in
    http://localhost*|http://127.0.0.1*) LOCAL=1 ;;
    *) LOCAL=0 ;;
esac
[ "${DEMO_ACCOUNT_EXISTS:-0}" = "1" ] && LOCAL=0

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
show() { printf '  \033[2m%s\033[0m\n' "$*"; }
die()  { printf '\n  %s\n' "$*" >&2; exit 1; }

# json <body> <key> -> the first value of a top-level string field.
#
# The space after the colon is optional on purpose: a replayed response is
# the same document as the original but not the same bytes, since it comes
# back from a jsonb column, which re-serialises it.
json() { printf '%s' "$1" | sed -n 's/.*"'"$2"'":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1; }
# jsonnum <body> <key> -> the first value of a top-level number field.
jsonnum() { printf '%s' "$1" | sed -n 's/.*"'"$2"'":[[:space:]]*\([0-9.-]*\).*/\1/p' | head -1; }
# count <body> <needle> -> occurrences.
count() { printf '%s' "$1" | grep -o "$2" | wc -l | tr -d ' '; }

# api <method> <path> [json body] [extra curl args...] -> the response,
# headers and body, carriage returns removed. A command substitution runs in
# a subshell, so the headers travel with the body rather than in a variable;
# json, body and request_id read what they need from the whole.
api() {
    method="$1"; path="$2"; body="${3:-}"
    shift 2
    [ $# -gt 0 ] && shift
    if [ -n "$body" ]; then set -- --data "$body" "$@"; fi
    curl --silent --show-error --max-time 60 -i -X "$method" "$GATEWAY$path" \
        -H "Authorization: Bearer ${TOKEN:-}" -H 'Content-Type: application/json' "$@" | tr -d '\r'
}
# body <response> -> the body alone; request_id <response> -> its X-Request-ID.
body() { printf '%s\n' "$1" | sed '1,/^$/d'; }
request_id() { printf '%s\n' "$1" | sed -n 's/^[Xx]-[Rr]equest-[Ii][Dd]: *//p' | head -1; }

# ------------------------------------------------------------------ 1
step "1. Start the infrastructure"
if [ "$LOCAL" = "0" ]; then
    note "a deployed environment at $GATEWAY: nothing to start from here"
elif [ "${DEMO_NO_START:-0}" = "1" ]; then
    note "DEMO_NO_START=1: using the environment already running at $GATEWAY"
else
    note "docker compose up -d --wait (both services, PostgreSQL, Redis, Prometheus, Grafana, the OpenTelemetry collector)"
    note "The first run builds both images, which takes a few minutes; later runs reuse the layers."
    docker compose up -d --wait >/dev/null 2>&1 || die "docker compose up failed; run it by hand to see why (a port in use? see docs/DEVELOPMENT.md)"
    docker compose ps --format '  {{.Service}}: {{.Status}}' 2>/dev/null || docker compose ps
fi
health=$(curl --silent --show-error --fail --max-time 5 "$GATEWAY/health" 2>/dev/null || true)
[ -n "$health" ] || die "The gateway is not answering at $GATEWAY."
note "gateway /health: $health"
note "gateway /ready:  $(curl --silent --max-time 5 "$GATEWAY/ready")"

# ------------------------------------------------------------------ 2
step "2. Create the demo account"
# The gateway's own command, in a one-off container on the compose network;
# there is no self-registration by design. The password goes in on standard
# input, so it appears in neither a process list nor a shell history.
if [ "${DEMO_ACCOUNT_EXISTS:-0}" = "1" ]; then
    note "$EMAIL is provisioned by the environment; not creating it here"
elif [ "$LOCAL" = "0" ]; then
    die "Accounts of a deployed environment are created by its deployment; set DEMO_ACCOUNT_EXISTS=1 and DEMO_EMAIL/DEMO_PASSWORD to one."
elif printf '%s' "$PASSWORD" | docker compose run --rm --no-deps -T \
        -e DATABASE_URL="postgres://vitalmesh:vitalmesh@postgres:5432/vitalmesh?sslmode=disable" \
        api-gateway users create "$EMAIL" OPERATOR >/dev/null 2>&1; then
    note "created $EMAIL with the OPERATOR role"
else
    note "$EMAIL already exists, which is fine: the demo can be run again"
fi

# ------------------------------------------------------------------ 3
step "3. Authenticate"
TOKEN=""
login=$(api POST /api/v1/auth/login "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
TOKEN=$(json "$login" access_token)
[ -n "$TOKEN" ] || die "Sign-in failed: $login"
note "POST /api/v1/auth/login -> a $(json "$login" token_type) token for $(json "$login" email), role $(json "$login" role), valid $(jsonnum "$login" expires_in)s"
note "(the token is not printed: it is a credential)"

# ------------------------------------------------------------------ 4
step "4. Create a synthetic patient"
reference="demo-$RUN"
patient=$(api POST /api/v1/patients \
    "{\"external_reference\":\"$reference\",\"date_of_birth\":\"1985-04-12\",\"sex\":\"FEMALE\"}" \
    -H "Idempotency-Key: demo-patient-$reference")
PATIENT_ID=$(json "$patient" id)
[ -n "$PATIENT_ID" ] || die "Could not create a patient: $patient"
note "POST /api/v1/patients -> $PATIENT_ID (reference $reference, status $(json "$patient" status))"
show "$(body "$patient")"

# ------------------------------------------------------------------ 5
step "5. Generate synthetic measurements"
# Sixty heart-rate readings a minute apart, a resting rhythm between 68 and
# 76 bpm, ending an hour ago so they sit inside the engine's acceptance
# window. Reading 31 is 185 bpm on purpose: above the critical bound the
# local processor is configured with, so the detector has one thing to
# find. The series is the same on every run; only the timestamps move.
now=$(date -u +%s)
items=""
i=0
while [ "$i" -lt 60 ]; do
    at=$(( now - 3600 - 60*60 + i * 60 ))
    stamp=$(date -u -d "@$at" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$at" +%Y-%m-%dT%H:%M:%SZ)
    value=$(( 68 + (i * 7) % 9 ))
    [ "$i" -eq 30 ] && value=185
    [ -n "$items" ] && items="$items,"
    items="$items{\"patient_id\":\"$PATIENT_ID\",\"type\":\"HEART_RATE\",\"value\":$value,\"unit\":\"bpm\",\"recorded_at\":\"$stamp\",\"source\":\"demo-monitor\"}"
    i=$((i + 1))
done
note "60 HEART_RATE readings, one a minute, source demo-monitor; reading 31 is 185 bpm"
note "(for volume, every type, noise and anomaly shapes: make synth-generate, see docs/SYNTHETIC_DATA.md)"

# ------------------------------------------------------------------ 6
step "6. Submit the measurements"
batch=$(api POST /api/v1/measurements/batch "{\"items\":[$items]}" -H "Idempotency-Key: demo-batch-$reference")
stored=$(count "$batch" '"id"')
[ "$stored" -gt 0 ] || die "No readings were stored: $batch"
BATCH_REQUEST=$(request_id "$batch")
note "POST /api/v1/measurements/batch -> 201, $stored readings stored in one transaction (request $BATCH_REQUEST)"

# ------------------------------------------------------------------ 7
step "7. Create a processing job"
job_body="{\"patient_id\":\"$PATIENT_ID\",\"measurement_types\":[\"HEART_RATE\"],\"windows\":[\"5m\",\"1h\"],\"percentiles\":[50,95]}"
job=$(api POST /api/v1/processing/jobs "$job_body" -H "Idempotency-Key: demo-job-$reference")
JOB_ID=$(json "$job" id)
JOB_REQUEST=$(request_id "$job")
[ -n "$JOB_ID" ] || die "The job was refused: $job"
note "POST /api/v1/processing/jobs -> job $JOB_ID (request $JOB_REQUEST)"
note "The gateway hands the readings to the Rust processor and answers when it is done, so the job is already $(json "$job" status)."
again=$(api POST /api/v1/processing/jobs "$job_body" -H "Idempotency-Key: demo-job-$reference")
[ "$(json "$again" id)" = "$JOB_ID" ] && note "The same request with the same Idempotency-Key returns the same job: no second run."

# ------------------------------------------------------------------ 8
step "8. Wait for processing"
# The create call is synchronous, so this loop finishes at once; it is how
# a client that lost the response, or a slower deployment, would wait.
tries=0
while :; do
    current=$(api GET "/api/v1/processing/jobs/$JOB_ID")
    status=$(json "$current" status)
    case "$status" in
        COMPLETED|FAILED|CANCELLED) break ;;
    esac
    tries=$((tries + 1))
    [ "$tries" -lt 60 ] || die "The job did not reach a terminal state: $current"
    sleep 1
done
note "GET /api/v1/processing/jobs/$JOB_ID -> $status"
note "requested $(json "$current" requested_at), started $(json "$current" started_at), completed $(json "$current" completed_at)"
note "algorithm $(json "$current" algorithm_version), processed by processor $(json "$current" service_version), attempts $(jsonnum "$current" attempt_count)"
[ "$status" = "COMPLETED" ] || die "The job ended $status: $(json "$current" error_code) $(json "$current" error_message)"

# ------------------------------------------------------------------ 9
step "9. Retrieve the results"
results=$(api GET "/api/v1/patients/$PATIENT_ID/processing-results?limit=200")
windows=$(count "$results" '"window"')
anomalies=$(count "$results" '"severity"')
[ "$windows" -gt 0 ] || die "The engine returned no results: $results"
# The 185 bpm reading is above the critical bound the processor is
# configured with, so finding nothing means the rules did not load.
[ "$anomalies" -gt 0 ] || die "No anomaly was found, though one reading was 185 bpm: check ANOMALY_RULES"
note "GET /api/v1/patients/$PATIENT_ID/processing-results -> $windows result windows, $anomalies anomalies"
note "one window's statistics:"
show "$(printf '%s' "$results" | sed -n 's/.*"statistics":\({[^}]*}\).*/\1/p' | head -1 | cut -c1-300)"
note "the anomaly the detector found:"
show "$(printf '%s' "$results" | grep -o '{[^{}]*"severity"[^{}]*}' | head -1 | cut -c1-300)"

# ------------------------------------------------------------------ 10
step "10. Inspect the metrics"
note "Both services expose Prometheus metrics with no identifiers in them (no patient, user or request ids)."
if [ "$LOCAL" = "0" ]; then
    note "In a deployed environment /metrics is reachable only inside the cluster, where Prometheus scrapes it;"
    note "the dashboards and queries are the same as locally (docs/DEPLOYMENT.md). Locally this step prints them."
else
note "gateway  $GATEWAY/metrics:"
curl --silent --max-time 5 "$GATEWAY/metrics" | grep -E '^vitalmesh_(http_requests_total\{[^}]*processing/jobs[^}]*\}|operation_total\{[^}]*(measurement\.batch|processing)[^}]*\})' | head -6 | sed 's/^/    /'
note "processor $PROCESSOR/metrics:"
curl --silent --max-time 5 "$PROCESSOR/metrics" | grep -E '^vitalmesh_processor_(jobs_total|job_duration_seconds_count)' | head -4 | sed 's/^/    /'
if prom=$(curl --silent --fail --max-time 5 "$PROMETHEUS/api/v1/query?query=sum(vitalmesh_processor_jobs_total)" 2>/dev/null); then
    note "Prometheus sum(vitalmesh_processor_jobs_total) = $(printf '%s' "$prom" | sed -n 's/.*"value":\[[0-9.]*,"\([^"]*\)".*/\1/p')  ($PROMETHEUS)"
    note "Grafana http://localhost:3000: four dashboards (API traffic, latency and errors, processing and jobs, infrastructure), no login needed"
else
    note "Prometheus is not running at $PROMETHEUS (docker compose up -d prometheus grafana starts the dashboards)"
fi
fi

# ------------------------------------------------------------------ 11
step "11. Inspect the trace"
# Every request starts (or continues) a W3C trace; the gateway logs its id
# with the request id, propagates it to the processor, which logs it too,
# and both export spans to the collector. One id, two services.
TRACE_ID=""
if [ "$LOCAL" = "0" ]; then
    note "request $JOB_REQUEST: in a deployed environment the gateway's log line for it carries the trace id;"
    note "kubectl logs on both services, filtered by that request id, shows the same trace crossing them (docs/DEPLOYMENT.md)."
else
gateway_line=$(docker compose logs --no-color --no-log-prefix api-gateway 2>/dev/null | grep "\"request_id\":\"$JOB_REQUEST\"" | head -1 || true)
TRACE_ID=$(json "$gateway_line" trace_id)
if [ -n "$TRACE_ID" ]; then
    note "request $JOB_REQUEST ran under trace $TRACE_ID"
    proc_lines=$(docker compose logs --no-color --no-log-prefix processor 2>/dev/null | grep -c "$TRACE_ID" || true)
    note "the processor logged $proc_lines lines under the same trace id: the call crossed both services under one trace"
    note "the collector received the spans (debug exporter, its latest batch):"
    docker compose logs --no-color --no-log-prefix --since 2m otel-collector 2>/dev/null | grep -iE 'TracesExporter|spans' | tail -1 | cut -c1-140 | sed 's/^/    /'
    note "(docker compose logs otel-collector shows every trace; set the exporter to 'detailed' in observability/otel/collector.yaml to see attributes)"
else
    note "the gateway's log line for request $JOB_REQUEST was not found in docker compose logs"
fi
fi

# ------------------------------------------------------------------ 12
step "12. Inspect the logs"
note "Structured JSON, one line per event, with request_id and trace_id on every line and never a credential or a reading value."
if [ "$LOCAL" = "0" ]; then
    note "kubectl -n <namespace> logs deploy/vitalmesh-api-gateway | grep $JOB_REQUEST   (and the same for the processor, by trace id)"
    note "the audit trail is in the environment's PostgreSQL: SELECT ... FROM audit_logs WHERE request_id IN ('$BATCH_REQUEST', '$JOB_REQUEST')"
else
note "gateway events for the job request (distinct):"
docker compose logs --no-color --no-log-prefix api-gateway 2>/dev/null | grep "\"request_id\":\"$JOB_REQUEST\"" | sed -E 's/.*"level":"([A-Z]+)".*"message":"([^"]*)".*/    \1  \2/' | sort -u
if [ -n "$TRACE_ID" ]; then
    note "processor events under the same trace (distinct):"
    docker compose logs --no-color --no-log-prefix processor 2>/dev/null | grep "$TRACE_ID" | sed -E 's/.*"level":"([A-Z]+)".*"message":"([^"]*)".*/    \1  \2/' | sort -u
fi
note "the audit log in PostgreSQL, for this run (append-only; every change names its actor and request):"
docker compose exec -T postgres psql -U vitalmesh -d vitalmesh -At -F '  ' -c \
    "SELECT to_char(min(created_at), 'HH24:MI:SS') AS at, action, resource_type, request_id, count(*) AS records FROM audit_logs WHERE request_id IN ('$BATCH_REQUEST', '$JOB_REQUEST') OR resource_id = '$PATIENT_ID' GROUP BY action, resource_type, request_id ORDER BY at, action" 2>/dev/null | sed 's/^/    /'
note "(time, action, resource, request id, records: one record per reading of the batch, one for the patient, one for the job)"
note "docker compose logs -f api-gateway processor   follows both services live"
fi

printf '\n\033[1mDone.\033[0m Patient %s, job %s%s.\n' "$PATIENT_ID" "$JOB_ID" "${TRACE_ID:+, trace $TRACE_ID}"
printf 'The readings, the job, its results and the audit trail are in PostgreSQL; run it again for a fresh patient.\n\n'
