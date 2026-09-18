#!/bin/sh
# Answers one question about a live environment: is it running exactly this
# release, all of it?
#
# This is the verification half of the rollback procedure (docs/ROLLBACK.md),
# and it is a script rather than a list in a runbook so that the steps an
# operator runs under pressure are the steps the rehearsal
# (scripts/rollback-test.sh) proves. It is equally the check after a normal
# deployment; nothing in it is specific to going backwards.
#
# It checks what is running, not what was asked for. A Deployment can report
# a finished rollout while a pod from the previous ReplicaSet is still
# serving, and an image reference can name a digest the node never pulled.
# So every pod is examined, and both the reference it asks for and the
# digest the node resolved are compared.
#
#   NAMESPACE=vitalmesh-production \
#   EXPECTED_VERSION=a1b2c3d \
#   EXPECTED_GATEWAY_DIGEST=sha256:... \
#   EXPECTED_PROCESSOR_DIGEST=sha256:... \
#   GATEWAY_URL=https://api.vitalmesh.example \
#       sh scripts/rollback-verify.sh
#
# The three expected values come from the release record of the release
# being rolled back to (release.json: .version, .images), so they are
# immutable references taken from the artifact rather than typed from
# memory. GATEWAY_URL is optional; without it the in-service checks are
# skipped and say so. KUBE_CONTEXT is optional and defaults to the current
# one.
#
# Needs kubectl, and curl if GATEWAY_URL is set. Reads only: it makes no
# change to the cluster.
set -eu

NAMESPACE="${NAMESPACE:?set NAMESPACE, such as vitalmesh-production}"
EXPECTED_VERSION="${EXPECTED_VERSION:-}"
EXPECTED_GATEWAY_DIGEST="${EXPECTED_GATEWAY_DIGEST:-}"
EXPECTED_PROCESSOR_DIGEST="${EXPECTED_PROCESSOR_DIGEST:-}"
GATEWAY_URL="${GATEWAY_URL:-}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
skip() { printf '  --    %s\n' "$1"; }
step() { printf '\n%s\n' "$1"; }

kc() {
    if [ -n "$KUBE_CONTEXT" ]; then
        kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"
    else
        kubectl -n "$NAMESPACE" "$@"
    fi
}

short() { printf '%s' "${1#sha256:}" | cut -c1-12; }

# ------------------------------------------------------------ the workloads
# A rollout that has not finished is not a rollback that has finished, and
# the difference is visible here before any image is compared: a pod from
# the release being left behind is still taking traffic.
check_rollout() {
    # check_rollout <deployment>
    _dep="$1"   # kept, because `set --` below replaces the positional parameters
    _spec="$(kc get "deployment/$_dep" -o jsonpath='{.spec.replicas} {.status.replicas} {.status.updatedReplicas} {.status.readyReplicas} {.metadata.generation} {.status.observedGeneration}' 2>/dev/null || true)"
    if [ -z "$_spec" ]; then
        fail "$_dep: no such Deployment in $NAMESPACE"
        return
    fi
    # shellcheck disable=SC2086 # deliberate word splitting of the jsonpath row
    set -- $_spec
    _want="${1:-0}"; _have="${2:-0}"; _updated="${3:-0}"; _ready="${4:-0}"; _gen="${5:-0}"; _seen="${6:-0}"
    if [ "$_gen" != "$_seen" ]; then
        fail "$_dep: the controller has not yet seen the current spec (generation $_gen, observed $_seen)"
    elif [ "$_updated" != "$_want" ] || [ "$_ready" != "$_want" ] || [ "$_have" != "$_want" ]; then
        fail "$_dep: $_ready of $_want ready, $_updated updated, $_have total: the rollout has not finished"
    else
        ok "$_dep: all $_want replicas are the current revision and Ready"
    fi
}

# Every pod, not the first: a half-finished rollout is exactly the state
# where looking at one pod gives the wrong answer.
check_images() {
    # check_images <app label> <expected digest> <what to call it>
    _label="$1"; _want="$2"; _name="$3"
    if [ -z "$_want" ]; then
        skip "$_name: no expected digest given"
        return
    fi
    _refs="$(kc get pods -l "app.kubernetes.io/name=$_label" -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}' 2>/dev/null || true)"
    _ids="$(kc get pods -l "app.kubernetes.io/name=$_label" -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{"\n"}{end}' 2>/dev/null || true)"
    if [ -z "$_refs" ]; then
        fail "$_name: no pods found for app.kubernetes.io/name=$_label"
        return
    fi

    _n=0; _bad=0
    for _ref in $_refs; do
        _n=$((_n + 1))
        case "$_ref" in
            *"@$_want") ;;
            *) fail "$_name: a pod asks for '$_ref', not @$_want"; _bad=$((_bad + 1)) ;;
        esac
        case "$_ref" in
            *@sha256:*) ;;
            *) fail "$_name: a pod names its image by tag ('$_ref'); a deployment must name a digest" ;;
        esac
    done
    [ "$_bad" -eq 0 ] && ok "$_name: all $_n pod(s) reference $(short "$_want")"

    _n=0; _bad=0
    for _id in $_ids; do
        _n=$((_n + 1))
        case "$_id" in
            *"$_want") ;;
            # An empty imageID is a pod that has not started its container.
            "") fail "$_name: a pod has no resolved image yet"; _bad=$((_bad + 1)) ;;
            *) fail "$_name: a node resolved the image to '$_id', not $_want"; _bad=$((_bad + 1)) ;;
        esac
    done
    [ "$_bad" -eq 0 ] && ok "$_name: all $_n node(s) resolved it to the same digest, so the bytes running are the bytes released"
}

step "Rollouts in $NAMESPACE"
check_rollout vitalmesh-api-gateway
check_rollout vitalmesh-processor

step "Go deployment (api-gateway)"
check_images api-gateway "$EXPECTED_GATEWAY_DIGEST" "gateway"

step "Rust deployment (processor)"
check_images processor "$EXPECTED_PROCESSOR_DIGEST" "processor"

# ----------------------------------------------------------- the migrate Job
# The Job runs the gateway image too, and a Job left behind from the release
# being rolled back from is confusing at best when the next deployment
# tries to create one with a different spec.
step "Migration Job"
_job_image="$(kc get job vitalmesh-migrate -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
if [ -z "$_job_image" ]; then
    skip "no vitalmesh-migrate Job present"
elif [ -n "$EXPECTED_GATEWAY_DIGEST" ]; then
    case "$_job_image" in
        *"@$EXPECTED_GATEWAY_DIGEST") ok "the migration Job ran the same image as the gateway" ;;
        *) fail "the migration Job is from another release ('$_job_image'); the next deployment will replace it" ;;
    esac
else
    skip "migration Job present; no expected digest to compare it with"
fi

# ---------------------------------------------------------------- in service
# The cluster's account of what it is running, and the service's own. They
# can disagree: a pod can carry the right image and be serving an old
# process if it never restarted.
step "What the services say about themselves"
if [ -z "$GATEWAY_URL" ]; then
    skip "GATEWAY_URL not set, so the in-service checks are skipped"
else
    _url="${GATEWAY_URL%/}"
    _health="$(curl -fsS -m 15 "$_url/health" 2>/dev/null || true)"
    _version="$(printf '%s' "$_health" | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p' | head -n 1)"
    if [ -z "$_version" ]; then
        fail "GET $_url/health did not answer with a version"
    elif [ -z "$EXPECTED_VERSION" ]; then
        ok "the gateway reports version $_version (nothing given to compare it with)"
    elif [ "$_version" = "$EXPECTED_VERSION" ]; then
        ok "the gateway reports version $_version, the release rolled back to"
    else
        fail "the gateway reports version '$_version', expected '$EXPECTED_VERSION'"
    fi
fi

# ------------------------------------------------------------- the database
# Not a check but a statement of fact, printed because it is the thing most
# likely to be assumed wrongly after a rollback.
step "Database schema"
_any_gateway_pod="$(kc get pods -l app.kubernetes.io/name=api-gateway -o name 2>/dev/null | head -n 1 || true)"
if [ -n "$_any_gateway_pod" ]; then
    printf '  --    the schema was NOT rolled back: migrations are forward-only.\n'
    printf '        The previous release runs against the current schema, which is\n'
    printf '        safe for as long as no migration has declared a breaking change\n'
    printf '        (docs/ROLLBACK.md, "How far back you can go").\n'
else
    skip "no gateway pod to report from"
fi

printf '\n'
if [ "$failures" -eq 0 ]; then
    printf 'rollback-verify: %s is running the expected release\n' "$NAMESPACE"
    exit 0
fi
printf 'rollback-verify: %d check(s) failed\n' "$failures"
exit 1
