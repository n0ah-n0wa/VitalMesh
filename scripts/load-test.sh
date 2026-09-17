#!/bin/sh
# Reproducible load test against the local environment (SPECIFICATIONS.md
# section 50), with k6 in a pinned container. docs/LOAD_TESTING.md explains
# the workload and how to read the report.
#
#   make load-test                      # the standard profile, about 70 s
#   make load-test LOAD_PROFILE=smoke   # a 15 s check that everything runs
#   PROFILE=standard DURATION=120s sh scripts/load-test.sh
#
# What it does: starts the environment if it is not up, creates the load
# accounts (there is no API for that; the gateway's own command does it),
# records the environment the run happens in, runs k6 on the compose network
# and writes the report and the raw summary to tests/load/results/.
#
# Everything it creates is synthetic and lives only in the local database:
# accounts on the reserved `.invalid` domain, patients that are a reference
# and a date, readings that are a fixed pattern.
#
#   PROFILE        smoke | standard (default standard)
#   DURATION       overrides the profile's run length, e.g. 120s
#   ACCOUNTS       accounts to create; the profiles need up to 27 (default 32)
#   BATCH_SIZE     readings per batch request (default 100, the gateway accepts up to 1000)
#   GATEWAY_URL    where the gateway is from this host (default http://localhost:8080)
#   K6_IMAGE       the k6 image (default grafana/k6:1.4.0)
set -eu

cd "$(dirname "$0")/.."

PROFILE="${PROFILE:-standard}"
ACCOUNTS="${ACCOUNTS:-32}"
BATCH_SIZE="${BATCH_SIZE:-100}"
GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
K6_IMAGE="${K6_IMAGE:-grafana/k6:1.4.0}"
# A development password for development accounts in a local database. It
# is not a secret.
PASSWORD="${LOAD_PASSWORD:-load-test-password-not-a-secret}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RESULTS="tests/load/results"

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

note() { printf '  %s\n' "$*"; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die()  { printf '\n  %s\n' "$*" >&2; exit 1; }

# Docker on Windows wants a Windows path for a bind mount; Git Bash has
# pwd -W for exactly that, and elsewhere pwd is right already.
host_pwd() { pwd -W 2>/dev/null || pwd; }

step "Environment"
if ! curl --silent --fail --max-time 5 "$GATEWAY/health" >/dev/null 2>&1; then
    note "the gateway is not answering at $GATEWAY; starting the services (docker compose up -d --wait)"
    docker compose up -d --wait postgres redis migrate processor api-gateway >/dev/null 2>&1 || die "docker compose up failed"
fi
gateway_health=$(curl --silent --max-time 5 "$GATEWAY/health")
processor_health=$(curl --silent --max-time 5 "http://localhost:${PROCESSOR_PORT:-8081}/health" || echo '{}')
version() { printf '%s' "$1" | sed -n 's/.*"version":[[:space:]]*"\([^"]*\)".*/\1/p'; }
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
dirty=$(git status --porcelain 2>/dev/null | grep -q . && echo " (working tree modified)" || true)
docker_info=$(docker info --format 'Docker {{.ServerVersion}} on {{.OperatingSystem}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes memory available to containers' 2>/dev/null || echo "docker info unavailable")
ENV_INFO="date: $STAMP
commit: $commit$dirty
host: $(uname -srm)
$docker_info
compose: $(docker compose version --short 2>/dev/null || echo unknown)
gateway: $(version "$gateway_health") at $GATEWAY, processor: $(version "$processor_health")
k6: $K6_IMAGE
services: docker-compose.yml defaults (no CPU or memory limits, PostgreSQL 16, Redis 7, processor MAX_CONCURRENT_JOBS 4, gateway rate limit 300/min per account)
load generator: k6 in a container on the same Docker host, on the compose network (shares the host's CPU with the services)"
printf '%s\n' "$ENV_INFO" | sed 's/^/  /'

step "Accounts"
# One command per account, each a short-lived container; existing accounts
# are left alone, so a second run only pays for the check.
i=1
created=0
existing=0
while [ "$i" -le "$ACCOUNTS" ]; do
    if printf '%s' "$PASSWORD" | docker compose run --rm --no-deps -T \
            -e DATABASE_URL="postgres://vitalmesh:vitalmesh@postgres:5432/vitalmesh?sslmode=disable" \
            api-gateway users create "load-$i@vitalmesh.invalid" OPERATOR >/dev/null 2>&1; then
        created=$((created + 1))
    else
        existing=$((existing + 1))
    fi
    i=$((i + 1))
done
note "$created created, $existing already there (load-1..load-$ACCOUNTS@vitalmesh.invalid, OPERATOR)"

step "k6: profile $PROFILE"
mkdir -p "$RESULTS"
HOST="$(host_pwd)"
docker run --rm --network vitalmesh \
    -u "$(id -u 2>/dev/null || echo 0)" \
    -v "$HOST/tests/load/k6:/load:ro" \
    -v "$HOST/$RESULTS:/results" \
    -e BASE_URL=http://api-gateway:8080 \
    -e ACCOUNTS="$ACCOUNTS" -e PASSWORD="$PASSWORD" -e PROFILE="$PROFILE" -e BATCH_SIZE="$BATCH_SIZE" \
    -e DURATION="${DURATION:-}" -e STAMP="$STAMP" -e ENV_INFO="$ENV_INFO" \
    "$K6_IMAGE" run --quiet /load/scenarios.js
status=$?

step "Report"
note "$RESULTS/report-$PROFILE-$STAMP.md   (the table above)"
note "$RESULTS/summary-$PROFILE-$STAMP.json (every metric k6 collected)"
exit "$status"
