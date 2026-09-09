#!/bin/sh
# A guided run through the whole system, against the local environment.
#
#   docker compose up -d --wait
#   ./scripts/demo.sh
#
# It creates an account, signs in, registers a patient, records readings,
# runs a processing job through the Rust engine, and reads the results back.
# Every step prints the request and what came back, so the output is the
# explanation.
#
# It is idempotent: run it as often as you like. Each run uses a fresh
# patient reference and a fresh idempotency key, and the account is created
# only if it is not already there.
set -eu

GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
EMAIL="${DEMO_EMAIL:-demo@vitalmesh.local}"
# A development password for a development account on a local machine. It is
# not a secret, and the account it opens exists only in your own database.
PASSWORD="${DEMO_PASSWORD:-demo-password-demo-password}"

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
die()  { printf '\n  %s\n' "$*" >&2; exit 1; }

# json <body> <key> -> the first value of a top-level string field.
#
# The space after the colon is optional on purpose. A replayed response is
# the same document as the original but not the same bytes: it comes back
# from a jsonb column, which re-serialises it. A parser that assumed the
# handler's exact spelling would work until the first replay.
json() { printf '%s' "$1" | sed -n 's/.*"'"$2"'":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1; }

step "0. Is the environment up?"
health=$(curl --silent --show-error --fail --max-time 5 "$GATEWAY/health" 2>/dev/null || true)
[ -n "$health" ] || die "The gateway is not answering at $GATEWAY. Start it with: docker compose up -d --wait"
note "$health"
note "$(curl --silent --max-time 5 "$GATEWAY/ready")"

step "1. Create an operator account"
# The gateway's own command, run in a one-off container on the compose
# network. The password is read from standard input so it never appears in
# a process list or a shell history.
if printf '%s' "$PASSWORD" | docker compose run --rm --no-deps -T \
        -e DATABASE_URL="postgres://vitalmesh:vitalmesh@postgres:5432/vitalmesh?sslmode=disable" \
        api-gateway users create "$EMAIL" OPERATOR >/dev/null 2>&1; then
    note "created $EMAIL as OPERATOR"
else
    note "$EMAIL already exists, which is fine: this script can be run again"
fi

step "2. Sign in"
login=$(curl --silent --show-error --max-time 10 -X POST "$GATEWAY/api/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
TOKEN=$(json "$login" access_token)
[ -n "$TOKEN" ] || die "Sign-in failed: $login"
note "got an access token (not printed: it is a credential)"

auth="Authorization: Bearer $TOKEN"

step "3. Register a patient"
reference="demo-$(date +%s)"
patient=$(curl --silent --show-error --max-time 10 -X POST "$GATEWAY/api/v1/patients" \
    -H "$auth" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: demo-patient-$reference" \
    -d "{\"external_reference\":\"$reference\",\"date_of_birth\":\"1985-04-12\",\"sex\":\"FEMALE\"}")
PATIENT_ID=$(json "$patient" id)
[ -n "$PATIENT_ID" ] || die "Could not create a patient: $patient"
note "patient $PATIENT_ID (reference $reference)"

step "4. Record heart-rate readings"
# Sixty readings a minute apart, ending an hour ago so they sit inside the
# engine's acceptance window. One is deliberately high, so the anomaly
# detector has something to find.
now=$(date -u +%s)
items=""
i=0
while [ "$i" -lt 60 ]; do
    at=$(( now - 3600 + i * 60 ))
    stamp=$(date -u -d "@$at" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$at" +%Y-%m-%dT%H:%M:%SZ)
    value=72
    [ "$i" -eq 30 ] && value=185
    [ -n "$items" ] && items="$items,"
    items="$items{\"patient_id\":\"$PATIENT_ID\",\"type\":\"HEART_RATE\",\"value\":$value,\"unit\":\"bpm\",\"recorded_at\":\"$stamp\",\"source\":\"demo-monitor\"}"
    i=$((i + 1))
done
batch=$(curl --silent --show-error --max-time 30 -X POST "$GATEWAY/api/v1/measurements/batch" \
    -H "$auth" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: demo-batch-$reference" \
    -d "{\"items\":[$items]}")
stored=$(printf '%s' "$batch" | grep -o '"id"' | wc -l | tr -d ' ')
[ "$stored" -gt 0 ] || die "No readings were stored: $batch"
note "stored $stored readings, one of them well above the healthy range"

step "5. Send the readings through the Rust engine"
job=$(curl --silent --show-error --max-time 60 -X POST "$GATEWAY/api/v1/processing/jobs" \
    -H "$auth" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: demo-job-$reference" \
    -d "{\"patient_id\":\"$PATIENT_ID\",\"measurement_types\":[\"HEART_RATE\"],\"windows\":[\"5m\",\"1h\"],\"percentiles\":[50,95]}")
JOB_ID=$(json "$job" id)
STATUS=$(json "$job" status)
[ -n "$JOB_ID" ] || die "The job was refused: $job"
note "job $JOB_ID finished as $STATUS"

step "6. Read the job back"
note "$(curl --silent --max-time 10 "$GATEWAY/api/v1/processing/jobs/$JOB_ID" -H "$auth" | cut -c1-300)"

step "7. Read the results"
results=$(curl --silent --show-error --max-time 10 \
    "$GATEWAY/api/v1/patients/$PATIENT_ID/processing-results" -H "$auth")
windows=$(printf '%s' "$results" | grep -o '"window"' | wc -l | tr -d ' ')
anomalies=$(printf '%s' "$results" | grep -o '"severity"' | wc -l | tr -d ' ')
[ "$windows" -gt 0 ] || die "the engine returned no results: $results"
# The 185 bpm reading is above the critical bound the processor is
# configured with, so finding nothing means the rules did not load.
[ "$anomalies" -gt 0 ] || die "no anomaly was found, though one reading was 185 bpm: check ANOMALY_RULES"
note "$windows result windows, $anomalies anomalies"
note "$(printf '%s' "$results" | cut -c1-300)"

step "8. Replay a request to show idempotency"
# The same key and the same body: the gateway returns the stored response
# rather than creating a second patient.
# -i keeps the headers in the same stream as the body, so nothing needs a
# temporary file. A curl built for Windows cannot write to a path its shell
# invented, and this script should run wherever the environment does.
replay=$(curl --silent --show-error --max-time 10 -i \
    -X POST "$GATEWAY/api/v1/patients" \
    -H "$auth" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: demo-patient-$reference" \
    -d "{\"external_reference\":\"$reference\",\"date_of_birth\":\"1985-04-12\",\"sex\":\"FEMALE\"}")
code=$(printf '%s' "$replay" | head -1 | tr -d '\r' | cut -d' ' -f2)
replayed=$(printf '%s' "$replay" | grep -i '^idempotency-replayed:' | tr -d '\r' | cut -d' ' -f2)
[ "$(json "$replay" id)" = "$PATIENT_ID" ] || die "the replay returned a different patient: $replay"
note "status $code, Idempotency-Replayed: ${replayed:-<absent>}"
note "the same patient $PATIENT_ID came back: no second patient was created"

step "9. What the system recorded"
note "metrics:  $(curl --silent --max-time 5 "$GATEWAY/metrics" | grep -c '^vitalmesh_') gateway series"
note "traces:   docker compose logs otel-collector | grep spans"
note "logs:     docker compose logs api-gateway processor"
note "Grafana:  http://localhost:3000  (four dashboards, no login needed)"
note "Prometheus: http://localhost:9090"

printf '\n\033[1mDone.\033[0m The readings, the job and its results are in PostgreSQL,\n'
printf 'and the request you just made crossed both services under one trace.\n\n'
