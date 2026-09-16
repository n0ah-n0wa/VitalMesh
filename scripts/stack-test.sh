#!/bin/sh
# The end-to-end suite against the real containerized stack (SPECIFICATIONS.md
# sections 44 and 52; docs/DEVELOPMENT.md, "The stack suite").
#
#   sh scripts/stack-test.sh          start a clean stack, run the suite, remove the stack
#   sh scripts/stack-test.sh up       only start the clean stack (and print how to reach it)
#   sh scripts/stack-test.sh test     only run the suite against a stack started with `up`
#   sh scripts/stack-test.sh down     only remove the stack and its volumes
#
# The stack is its own compose project (vitalmesh-e2e by default) on its
# own network and ports, so it never touches the development environment
# started by `make up`, and it is created from nothing every time: the
# volumes are removed before it starts, so the database holds only what the
# migrations and the suite put there. That is what makes the suite's
# answers reproducible.
#
# Environment:
#   COMPOSE            the compose command, files included (default: docker compose -f docker-compose.yml;
#                      CI passes the same with its cache override added). It is used as given, so
#                      the compose file is never named twice, which compose rejects.
#   E2E_PROJECT        compose project name (default vitalmesh-e2e)
#   E2E_GATEWAY_PORT   host port of the gateway (default 18080); processor +1, PostgreSQL 15433, Redis 16379
#   E2E_KEEP           set to 1 to leave the stack running after a failed suite, for a look around
set -eu

cd "$(dirname "$0")/.."

MODE="${1:-all}"
PROJECT="${E2E_PROJECT:-vitalmesh-e2e}"
GATEWAY_PORT="${E2E_GATEWAY_PORT:-18080}"
PROCESSOR_PORT=$((GATEWAY_PORT + 1))
POSTGRES_PORT="${E2E_POSTGRES_PORT:-15433}"
REDIS_PORT="${E2E_REDIS_PORT:-16379}"
COMPOSE="${COMPOSE:-docker compose -f docker-compose.yml}"

# Everything the compose file reads, so that `up` here and `stop`/`start`
# from the suite see the same project.
export COMPOSE_PROJECT_NAME="$PROJECT"
export VITALMESH_NETWORK="$PROJECT"
export GATEWAY_PORT PROCESSOR_PORT POSTGRES_PORT REDIS_PORT
# Deterministic on purpose: the suite never depends on these, but a run
# should not pick up a developer's shell either.
export ENVIRONMENT=local LOG_LEVEL=info

compose() { $COMPOSE "$@"; }

up() {
    echo "== stack: removing any previous $PROJECT stack, volumes included"
    compose down -v --remove-orphans -t 5 >/dev/null 2>&1 || true
    echo "== stack: building and starting $PROJECT (gateway on :$GATEWAY_PORT, PostgreSQL on :$POSTGRES_PORT)"
    # Only what the suite needs: the observability services are not part
    # of the application's behaviour and would bind more ports.
    compose up -d --wait --build postgres redis migrate processor api-gateway
    compose ps
    echo "gateway: http://localhost:$GATEWAY_PORT"
}

down() {
    echo "== stack: removing $PROJECT and its volumes"
    compose down -v --remove-orphans -t 5
}

run_tests() {
    echo "== stack: running the suite"
    E2E_GATEWAY_URL="${E2E_GATEWAY_URL:-http://localhost:$GATEWAY_PORT}" \
    E2E_DATABASE_URL="${E2E_DATABASE_URL:-postgres://vitalmesh:vitalmesh@localhost:$POSTGRES_PORT/vitalmesh?sslmode=disable}" \
    E2E_COMPOSE_PROJECT="$PROJECT" \
    E2E_COMPOSE_FILE="$(pwd)/docker-compose.yml" \
        go -C services/api-gateway test -tags stack -count=1 -timeout 25m -v ./tests/stack/...
}

case "$MODE" in
    up)   up ;;
    down) down ;;
    test) run_tests ;;
    all)
        up
        status=0
        run_tests || status=$?
        if [ "$status" -ne 0 ]; then
            echo "== stack: the suite failed; the services' last log lines follow"
            compose logs --no-color --tail 40 api-gateway processor || true
            if [ "${E2E_KEEP:-0}" = "1" ]; then
                echo "== stack: E2E_KEEP=1, leaving $PROJECT running (sh scripts/stack-test.sh down removes it)"
                exit "$status"
            fi
        fi
        down
        exit "$status"
        ;;
    *)
        echo "usage: sh scripts/stack-test.sh [up|test|down]" >&2
        exit 2
        ;;
esac
