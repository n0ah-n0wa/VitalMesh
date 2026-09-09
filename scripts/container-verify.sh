#!/bin/sh
# Checks the properties the Dockerfiles claim, against the built images.
#
# A Dockerfile can say USER 65532 and still ship a container that runs as
# root, if a base image or an entrypoint overrides it. Everything here is
# asserted against a real container or a real image, never against the
# Dockerfile text.
#
# Usage:
#   make docker-build
#   make docker-verify
set -eu

GATEWAY="${GATEWAY_IMAGE:-vitalmesh/api-gateway:dev}"
PROCESSOR="${PROCESSOR_IMAGE:-vitalmesh/processor:dev}"

failures=0
pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

cleanup() { docker rm -f verify-gw verify-pr >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

for image in "$GATEWAY" "$PROCESSOR"; do
    echo
    echo "$image"

    if ! docker image inspect "$image" >/dev/null 2>&1; then
        fail "is not built; run 'make docker-build' first"
        continue
    fi

    # --- what the image declares -------------------------------------
    user=$(docker inspect --format '{{.Config.User}}' "$image")
    case "$user" in
        ""|root|0|0:0) fail "runs as root by default (User=[$user])" ;;
        *) pass "declares a non-root user ($user)" ;;
    esac

    if [ -n "$(docker inspect --format '{{.Config.Healthcheck}}' "$image")" ]; then
        pass "declares a health check"
    else
        fail "declares no health check"
    fi

    # A credential in a build layer is readable by anyone who can pull the
    # image, however carefully it was removed afterwards.
    if docker history --no-trunc --format '{{.CreatedBy}}' "$image" |
        grep -qiE "password=|secret=|token=|api[_-]?key=|BEGIN [A-Z ]*PRIVATE KEY"; then
        fail "has a credential-shaped string in its build history"
    else
        pass "has no credential in its build history"
    fi

    if docker inspect --format '{{.Config.Env}}' "$image" |
        grep -qiE "password|secret|token|api[_-]?key"; then
        fail "bakes a credential-shaped variable into its environment"
    else
        pass "bakes no credential into its environment"
    fi

    # --- what the image contains -------------------------------------
    # A shell is what turns a code-execution bug into an interactive one,
    # and a package manager is what lets an intruder install tools.
    contents=$(docker create --name verify-contents "$image" >/dev/null 2>&1 &&
        docker export verify-contents 2>/dev/null | tar -t 2>/dev/null || true)
    docker rm verify-contents >/dev/null 2>&1 || true

    if printf '%s' "$contents" | grep -qE "(^|/)(sh|bash|dash|busybox|ash)$"; then
        fail "contains a shell"
    else
        pass "contains no shell"
    fi
    if printf '%s' "$contents" | grep -qE "(^|/)(apt|apt-get|dpkg|apk|yum|dnf|rpm)$"; then
        fail "contains a package manager"
    else
        pass "contains no package manager"
    fi
    if printf '%s' "$contents" | grep -qE "(^|/)(curl|wget|nc|netcat|python[0-9.]*|perl)$"; then
        fail "contains a network or scripting tool"
    else
        pass "contains no network or scripting tools"
    fi
done

echo
echo "Running containers"

# The image config is a claim; this is the kernel's answer. Each container
# is started with the hardening a deployment should apply, so the check also
# proves the service works under it.
#
# Nothing here needs a database, a Redis or a network of its own. The probe
# is liveness, which by design consults no dependency: a container waiting
# for its database is up, not broken. That keeps this runnable anywhere,
# including a CI runner with no compose stack. The values below are
# throwaway and go no further than a container deleted seconds later.
docker run -d --name verify-pr \
    -e ENVIRONMENT=local -e INTERNAL_TOKEN=container-verify-not-a-real-token \
    --read-only --cap-drop=ALL --security-opt no-new-privileges \
    "$PROCESSOR" >/dev/null 2>&1 || true

docker run -d --name verify-gw \
    -e DATABASE_URL="postgres://verify:verify@127.0.0.1:5432/verify?sslmode=disable" \
    -e JWT_SECRET=container-verify-not-a-real-signing-key \
    -e PROCESSOR_URL=http://127.0.0.1:8081 \
    -e PROCESSOR_TOKEN=container-verify-not-a-real-token \
    --read-only --cap-drop=ALL --security-opt no-new-privileges \
    "$GATEWAY" >/dev/null 2>&1 || true

for container in verify-gw verify-pr; do
    if ! docker inspect "$container" >/dev/null 2>&1; then
        fail "$container did not start"
        continue
    fi

    # The uid the kernel sees, not the one the image asked for. Docker
    # refuses a ps format with no PID column, so pid is asked for and
    # discarded.
    uid=$(docker top "$container" -o user,pid 2>/dev/null | awk 'NR == 2 { print $1 }')
    case "$uid" in
        "") fail "$container: the running uid could not be read" ;;
        root|0) fail "$container runs as root" ;;
        *) pass "$container runs as uid $uid" ;;
    esac

    # A read-only root filesystem is only useful if the service works under
    # it; a service that needs to write would have failed to start.
    if [ "$(docker inspect --format '{{.State.Running}}' "$container")" = true ]; then
        pass "$container runs with a read-only root and no capabilities"
    else
        fail "$container is not running under --read-only --cap-drop=ALL"
        docker logs "$container" 2>&1 | tail -3 | sed 's/^/        /'
    fi

    # There is nothing to exec into: no shell means no interactive foothold.
    if docker exec "$container" /bin/sh -c id >/dev/null 2>&1; then
        fail "$container has a shell that can be exec'd into"
    else
        pass "$container has no shell to exec into"
    fi
done

echo
echo "Health checks"

# The image's own HEALTHCHECK, run by Docker, using the binary's healthcheck
# argument. It is the only probe available in an image with no shell.
for container in verify-gw verify-pr; do
    docker inspect "$container" >/dev/null 2>&1 || continue
    status=starting
    i=0
    while [ "$status" = starting ] && [ "$i" -lt 60 ]; do
        status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || echo none)
        i=$((i + 1))
        [ "$status" = starting ] && sleep 1
    done
    case "$status" in
        healthy) pass "$container reports healthy through its own health check" ;;
        *) fail "$container health is '$status'"
           docker inspect --format '{{range .State.Health.Log}}{{.Output}}{{end}}' "$container" 2>/dev/null | tail -2 | sed 's/^/        /' ;;
    esac
done

echo
if [ "$failures" -eq 0 ]; then
    echo "containers: all checks passed"
    exit 0
fi
echo "containers: $failures check(s) failed"
exit 1
