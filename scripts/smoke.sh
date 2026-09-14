#!/bin/sh
# Smoke test against a running gateway: is the thing that was just deployed
# up, the version that was released, and behaving as the edge of the system
# should? No credentials, no data: everything here is answerable
# unauthenticated.
#
#   GATEWAY_URL=https://api.staging.vitalmesh.example EXPECTED_VERSION=f726f4b sh scripts/smoke.sh
#   GATEWAY_URL=http://localhost:8080 sh scripts/smoke.sh
#
# The deployment pipeline runs this after the rollout (release.yml). A new
# target behind a load balancer can take a minute to pass its health checks,
# so the first check retries for up to RETRY_SECONDS (default 300); the rest
# do not, since by then the gateway is known to be answering.
set -eu

GATEWAY="${GATEWAY_URL:?set GATEWAY_URL, such as https://api.staging.vitalmesh.example}"
GATEWAY="${GATEWAY%/}"
EXPECTED_VERSION="${EXPECTED_VERSION:-}"
RETRY_SECONDS="${RETRY_SECONDS:-300}"

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

host="${GATEWAY#*://}"; host="${host%%/*}"; host="${host%%:*}"
scheme="${GATEWAY%%://*}"

# One request: the body lands in the file named first, the status code is
# printed. The shell does the redirection rather than curl's -o, so the
# temporary file's path never has to be one curl understands (on Windows,
# under Git Bash, it would not be). Certificate validation is left on: a
# wrong or missing certificate is a finding, not something to work around.
get() {
    curl --silent --show-error --max-time 10 --write-out '\n%{http_code}' "$2" > "$1" 2>&1 || true
    tail -n 1 "$1"
    sed -i '$d' "$1" 2>/dev/null || true
}

printf '\nSmoke test: %s\n' "$GATEWAY"

if [ "$scheme" = "https" ]; then
    if command -v getent >/dev/null 2>&1; then
        if addrs="$(getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')" && [ -n "$addrs" ]; then
            ok "$host resolves to $addrs"
        else
            fail "$host does not resolve: the DNS record for the load balancer is missing (infrastructure/terraform/README.md, first-time order)"
        fi
    fi
fi

body="$(mktemp)"; trap 'rm -f "$body"' EXIT

deadline=$(( $(date +%s) + RETRY_SECONDS ))
attempt=0
while :; do
    attempt=$((attempt + 1))
    code="$(get "$body" "$GATEWAY/health" || true)"
    [ "$code" = "200" ] && break
    if [ "$(date +%s)" -ge "$deadline" ]; then
        fail "GET /health: $code after $attempt attempts ($(head -c 200 "$body"))"
        break
    fi
    sleep 5
done
if [ "$code" = "200" ]; then
    ok "GET /health 200 (attempt $attempt)"
    version="$(sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p' "$body" | head -n 1)"
    if [ -n "$EXPECTED_VERSION" ]; then
        if [ "$version" = "$EXPECTED_VERSION" ]; then
            ok "running version $version, the one released"
        else
            fail "running version '$version', expected '$EXPECTED_VERSION': the rollout did not put the released build into service"
        fi
    else
        ok "running version ${version:-unknown}"
    fi
fi

code="$(get "$body" "$GATEWAY/ready" || true)"
if [ "$code" = "200" ]; then ok "GET /ready 200: dependencies reachable"; else fail "GET /ready $code ($(head -c 200 "$body"))"; fi

# The edge refuses what it should: no token, no data.
code="$(get "$body" "$GATEWAY/api/v1/auth/me" || true)"
if [ "$code" = "401" ]; then ok "GET /api/v1/auth/me without a token: 401"; else fail "GET /api/v1/auth/me without a token: $code, expected 401"; fi

code="$(get "$body" "$GATEWAY/api/v1/patients/does-not-exist" || true)"
if [ "$code" = "401" ]; then ok "GET /api/v1/patients/... without a token: 401, not 404: nothing is revealed unauthenticated"; else fail "GET /api/v1/patients/... without a token: $code, expected 401"; fi

if [ "$scheme" = "https" ]; then
    code="$(curl --silent --show-error --max-time 10 -o /dev/null -w '%{http_code}' "http://$host/health" 2>&1 || true)"
    case "$code" in
        301|308) ok "http://$host redirects ($code) to https" ;;
        *) fail "http://$host/health answered $code, expected a redirect to https" ;;
    esac
fi

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'smoke: %d check(s) failed\n' "$failures"
    exit 1
fi
printf 'smoke: all checks passed\n'
