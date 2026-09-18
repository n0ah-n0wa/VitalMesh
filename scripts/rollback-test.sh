#!/bin/sh
# Rehearses the production rollback procedure and checks that it actually
# rolls everything back (SPECIFICATIONS.md sections 32 and 109;
# docs/ROLLBACK.md records what this observes).
#
# A rollback is the one procedure nobody wants to read for the first time
# while production is broken, and the one least likely to have been run.
# This runs it, on a real cluster, between two real releases, and verifies
# each thing it is supposed to put back:
#
#   Go deployment          the gateway pods run release A's image and the
#                          gateway reports release A's version
#   Rust deployment        the same for the processor, checked at the
#                          processor itself rather than through the gateway
#   Kubernetes manifests   a configuration value that differed between the
#                          two releases is back to A's -- checked in the
#                          cluster AND in the running process, because an
#                          object that changed is not the same as a pod
#                          that noticed
#   image versions         the pods' image references are A's digests, and
#                          the node resolved them to A's digests
#
# and what it must NOT roll back:
#
#   the database schema    recorded before and after; migrations are
#                          forward-only (docs/DATABASE.md)
#
# It also runs the alternative mechanism, `kubectl rollout undo`, against
# the same pair, to show what that one does and does not restore. The two
# are not interchangeable and the difference is the reason this repository
# treats redeploying a release as the rollback and `rollout undo` as the
# emergency stop.
#
# Everything is referenced by digest. The cluster has a registry beside it
# (kind/cluster-rollback.yaml explains why) so that a digest exists at all:
# an image that has never been pushed has no repository digest, and a
# rehearsal between two moveable tags would prove nothing about a procedure
# whose whole claim is immutability.
#
#   sh scripts/rollback-test.sh          create, rehearse, destroy
#   sh scripts/rollback-test.sh --keep   leave the cluster and registry up
#
# Needs kind, kubectl and docker on PATH, and four images built:
#
#   for v in a1b2c3d e4f5a6b; do
#     for s in api-gateway processor; do
#       docker build --build-arg VERSION=$v -t vitalmesh/$s:sha-$v services/$s
#     done
#   done
#
# kind and kubectl are the one place this repository steps outside Docker,
# for the reason scripts/k8s-local-test.sh gives: kind builds its nodes as
# containers on the host's Docker and so cannot itself run in one.
set -eu

CLUSTER="vitalmesh-rollback"
NAMESPACE="vitalmesh-local"
KUBE_DIR="infrastructure/kubernetes"
KIND_CONFIG="$KUBE_DIR/kind/cluster-rollback.yaml"
REGISTRY_NAME="vitalmesh-rollback-registry"
REGISTRY_PORT="5001"
REGISTRY_IMAGE="registry:2"
REGISTRY_HOST="localhost:$REGISTRY_PORT"

CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"

# The two releases. Synthetic short commits: this rehearsal is about the
# procedure, so the pair only has to be two builds that can be told apart,
# and inventing the names makes it obvious they are not real history.
#
# They differ in two ways at once, which is the realistic case and the one
# a rollback has to handle: the binaries differ (the version compiled in)
# and the configuration differs. B's configuration asks the gateway for a
# processing algorithm version the processor does not implement, so B is a
# release that would refuse every job it was given -- a plausible reason to
# roll back, and a change that no amount of rolling back the image alone
# would undo.
RELEASE_A="a1b2c3d"
RELEASE_B="e4f5a6b"
ALGORITHM_A="1.0.0"
ALGORITHM_B="1.1.0"

# The local ports are not chosen here. kubectl is asked for any free one
# and says which it took, because a fixed port is a collision waiting to
# happen: a port-forward left behind by an earlier run (or by someone
# debugging) keeps its port and keeps answering from the pod it first
# selected, so a new forward fails to bind and every read below quietly
# comes from the previous release.
GATEWAY_PORT=""
PROCESSOR_PORT=""

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
step() { printf '\n%s\n' "$1"; }
note() { printf '        %s\n' "$1"; }

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

WORK="$(mktemp -d)"
gw_pf=""
pr_pf=""

cleanup() {
    [ -n "$gw_pf" ] && kill "$gw_pf" 2>/dev/null || true
    [ -n "$pr_pf" ] && kill "$pr_pf" 2>/dev/null || true
    rm -rf "$WORK"
    if [ "$KEEP" -eq 0 ]; then
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
        docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

kc() { kubectl --context "kind-$CLUSTER" -n "$NAMESPACE" "$@"; }

# kubectl, kind and docker are native Windows programs under Git Bash and
# cannot open an MSYS path such as /tmp/tmp.X; cygpath -m gives C:/like/this,
# which every one of them accepts, and on a Unix host this is the identity.
# The same helper is in k8s-validate.sh and tf-validate.sh.
host_path() {
    if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf %s "$1"; fi
}

# seconds since the epoch, for the durations this reports
now() { date +%s; }

# ---------------------------------------------------------------- preflight
step "Preflight"
for tool in kind kubectl docker; do
    command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not on PATH" >&2; exit 2; }
done
ok "kind, kubectl and docker are present"

for v in "$RELEASE_A" "$RELEASE_B"; do
    for s in api-gateway processor; do
        docker image inspect "vitalmesh/$s:sha-$v" >/dev/null 2>&1 || {
            echo "vitalmesh/$s:sha-$v is missing; the header says how to build it" >&2; exit 2; }
    done
done
ok "both releases of both services are built"

# ------------------------------------------------------------------ registry
step "Registry"
if docker inspect "$REGISTRY_NAME" >/dev/null 2>&1; then
    docker start "$REGISTRY_NAME" >/dev/null 2>&1 || true
    ok "reusing the registry"
else
    docker run -d --restart=no --name "$REGISTRY_NAME" \
        -p "127.0.0.1:$REGISTRY_PORT:5000" "$REGISTRY_IMAGE" >/dev/null \
        && ok "started a registry on $REGISTRY_HOST" || { fail "could not start the registry"; exit 1; }
fi

# Push both releases and read back the digest the registry assigned. That
# digest, not the tag, is what everything below refers to.
push() {
    # push <service> <version>; prints the repository digest
    _local="vitalmesh/$1:sha-$2"
    _remote="$REGISTRY_HOST/vitalmesh/$1:sha-$2"
    docker tag "$_local" "$_remote"
    docker push "$_remote" >/dev/null 2>&1 || return 1
    docker image inspect "$_remote" --format '{{range .RepoDigests}}{{println .}}{{end}}' \
        | sed -n "s#^$REGISTRY_HOST/vitalmesh/$1@##p" | head -n 1
}

GATEWAY_A_DIGEST="$(push api-gateway "$RELEASE_A")" || true
PROCESSOR_A_DIGEST="$(push processor "$RELEASE_A")" || true
GATEWAY_B_DIGEST="$(push api-gateway "$RELEASE_B")" || true
PROCESSOR_B_DIGEST="$(push processor "$RELEASE_B")" || true

for d in "$GATEWAY_A_DIGEST" "$PROCESSOR_A_DIGEST" "$GATEWAY_B_DIGEST" "$PROCESSOR_B_DIGEST"; do
    case "$d" in
        sha256:*) ;;
        *) fail "a push did not produce a digest ('$d')"; exit 1 ;;
    esac
done
ok "release $RELEASE_A: gateway ${GATEWAY_A_DIGEST%%:*}:$(printf '%s' "${GATEWAY_A_DIGEST#sha256:}" | cut -c1-12)"
ok "release $RELEASE_A: processor ${PROCESSOR_A_DIGEST%%:*}:$(printf '%s' "${PROCESSOR_A_DIGEST#sha256:}" | cut -c1-12)"
ok "release $RELEASE_B: gateway ${GATEWAY_B_DIGEST%%:*}:$(printf '%s' "${GATEWAY_B_DIGEST#sha256:}" | cut -c1-12)"
ok "release $RELEASE_B: processor ${PROCESSOR_B_DIGEST%%:*}:$(printf '%s' "${PROCESSOR_B_DIGEST#sha256:}" | cut -c1-12)"

if [ "$GATEWAY_A_DIGEST" = "$GATEWAY_B_DIGEST" ] || [ "$PROCESSOR_A_DIGEST" = "$PROCESSOR_B_DIGEST" ]; then
    fail "the two releases produced identical digests; they are the same build and nothing below would mean anything"
    exit 1
fi
ok "the two releases are different images"

# ------------------------------------------------------------------- cluster
step "Cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    ok "reusing the existing cluster"
else
    kind create cluster --config "$KIND_CONFIG" >/dev/null 2>&1 \
        && ok "created" || { fail "kind create cluster"; exit 1; }
fi

# The registry has to be on the cluster's network for containerd's mirror
# to reach it by name.
docker network connect kind "$REGISTRY_NAME" >/dev/null 2>&1 || true
ok "the registry is on the cluster's network"

if kubectl --context "kind-$CLUSTER" -n kube-system get daemonset calico-node >/dev/null 2>&1; then
    ok "Calico already installed"
else
    kubectl --context "kind-$CLUSTER" apply -f "$CALICO_MANIFEST" >/dev/null 2>&1 \
        && kubectl --context "kind-$CLUSTER" -n kube-system wait --for=condition=ready \
            pod -l k8s-app=calico-node --timeout=300s >/dev/null 2>&1 \
        && ok "Calico installed; the NetworkPolicies are enforced" \
        || fail "Calico did not become ready"
fi
kubectl --context "kind-$CLUSTER" wait --for=condition=Ready node --all --timeout=180s >/dev/null 2>&1 \
    && ok "the node is Ready" || fail "the node never became Ready"

# The two fixture images, loaded from the host rather than pulled. They
# are already here (the compose stack uses them), and a rehearsal that
# reaches out to Docker Hub fails for reasons that have nothing to do
# with rollback: a rate limit or a dropped pull leaves PostgreSQL in
# ImagePullBackOff and the migration Job retrying against nothing, which
# is what happened the first time this ran cold. Both manifests name a
# tag other than :latest, so the default pull policy is IfNotPresent and
# the loaded copy is the one used. The two service images are
# deliberately NOT loaded this way: they have to come from the registry,
# by digest, because that is the thing being rehearsed.
for image in postgres:16-alpine redis:7-alpine; do
    if ! docker image inspect "$image" >/dev/null 2>&1; then
        ok "$image is not on this host; the node will pull it"
    elif kind load docker-image "$image" --name "$CLUSTER" >/dev/null 2>&1; then
        ok "loaded $image into the node"
    else
        fail "could not load $image into the node"
    fi
done

# ------------------------------------------------------------------ rendering
step "Rendering the two releases"

# The manifests are rendered from a copy of the tracked tree, so that
# `kustomize edit set image` -- the same command deploy.yml runs -- does not
# edit the repository. Rendering the real overlay rather than a fixture is
# the point: what is rolled back has to be what is deployed.
cp -r "$KUBE_DIR" "$WORK/kubernetes"

kustomize() {
    docker run --rm --user "$(id -u):$(id -g)" \
        -v "$(host_path "$WORK/kubernetes"):/kube" \
        -w "/kube/overlays/local" registry.k8s.io/kustomize/kustomize:v5.4.3 "$@"
}

render() {
    # render <version> <gateway digest> <processor digest> <algorithm version> <output>
    kustomize edit set image \
        "vitalmesh/api-gateway=$REGISTRY_HOST/vitalmesh/api-gateway@$2" \
        "vitalmesh/processor=$REGISTRY_HOST/vitalmesh/processor@$3" >/dev/null
    # The configuration half of the release. PROCESSING_ALGORITHM_VERSION is
    # used because it is the one ConfigMap value this system reports from
    # the running process (as a vitalmesh_build_info label), so a rollback
    # of it can be observed where it matters rather than only in etcd.
    sed -i "s#^  LOG_LEVEL:#  PROCESSING_ALGORITHM_VERSION: \"$4\"\n  LOG_LEVEL:#" \
        "$WORK/kubernetes/overlays/local/configmap-api-gateway.yaml"
    kustomize build . > "$5"
    # put the patch file back for the next render
    cp "$KUBE_DIR/overlays/local/configmap-api-gateway.yaml" \
       "$WORK/kubernetes/overlays/local/configmap-api-gateway.yaml"
    # and stamp the image digests into the pods, as deploy.yml does
    for pair in "api-gateway=$2" "processor=$3"; do
        _c="${pair%%=*}"; _d="${pair#*=}"
        _expr="(.. | select(has(\"containers\")).containers[] | select(.name == \"$_c\").env[] | select(.name == \"IMAGE_DIGEST\")).value = \"$_d\""
        docker run --rm -i mikefarah/yq:4.44.6 "$_expr" < "$5" > "$5.stamped"
        mv "$5.stamped" "$5"
    done
}

render "$RELEASE_A" "$GATEWAY_A_DIGEST" "$PROCESSOR_A_DIGEST" "$ALGORITHM_A" "$WORK/release-a.yaml"
render "$RELEASE_B" "$GATEWAY_B_DIGEST" "$PROCESSOR_B_DIGEST" "$ALGORITHM_B" "$WORK/release-b.yaml"

for f in "$WORK/release-a.yaml" "$WORK/release-b.yaml"; do
    [ -s "$f" ] || { fail "$(basename "$f") rendered empty"; exit 1; }
done
ok "release $RELEASE_A rendered: $(grep -c '^kind:' "$WORK/release-a.yaml") objects"
ok "release $RELEASE_B rendered: $(grep -c '^kind:' "$WORK/release-b.yaml") objects"

if grep -q 'set-by-ci' "$WORK/release-a.yaml" || grep -q 'set-by-ci' "$WORK/release-b.yaml"; then
    fail "a rendered manifest still carries a placeholder reference"
fi
if grep -E '^\s+image: .*vitalmesh/(api-gateway|processor):' "$WORK/release-a.yaml" >/dev/null; then
    fail "release $RELEASE_A names an image by tag; the procedure requires digests"
else
    ok "every service image in both renders is named by digest"
fi

# ------------------------------------------------- deploy, and waiting
# settle <app label> <digest>: true once every pod for that app is the
# given image. `rollout status` returns when the NEW pods are Ready, while
# the old ones are still inside their termination grace period -- still
# running, still answering, still selectable by a port-forward. Reading a
# version without waiting for them reports the release that was just
# replaced, which is how this rehearsal first "passed" a deployment that
# had not happened.
settle() {
    _app="$1"; _want="$2"
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        _all="$(kc get pods -l "app.kubernetes.io/name=$_app" -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' 2>/dev/null || true)"
        _other=0
        for _img in $_all; do
            case "$_img" in *"@$_want") ;; *) _other=1 ;; esac
        done
        [ -n "$_all" ] && [ "$_other" -eq 0 ] && return 0
        sleep 3
    done
    return 1
}

deploy() {
    # deploy <label> <manifest> <gateway digest> <processor digest>
    printf '        applying %s\n' "$1"
    _manifest="$(host_path "$2")"

    # The migration Job goes first, and this is not tidiness. A Job pod
    # template is immutable, so applying a release whose Job names a
    # different image fails on that object -- and because the Job is
    # rendered before the Deployments, the apply stops there and leaves
    # the previous release running while reporting only a Job error. The
    # deployment workflow deletes it first for the same reason
    # (infrastructure/kubernetes/README.md); this rehearsal found out why.
    kc delete job vitalmesh-migrate --ignore-not-found >/dev/null 2>&1 || true

    # Not silenced: an apply that half-succeeds is the failure this whole
    # script exists to catch, so it is reported rather than swallowed.
    if ! kubectl --context "kind-$CLUSTER" apply --server-side --force-conflicts -f "$_manifest" >/dev/null; then
        fail "$1: the apply did not succeed"
        return 1
    fi

    # What the cluster now intends to run, checked before waiting for it:
    # `rollout status` on a Deployment that was never updated reports the
    # previous revision as a finished rollout, which reads as success.
    _intended="$(kc get deployment vitalmesh-api-gateway -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
    case "$_intended" in
        *"@$3") ;;
        *) fail "$1: after the apply the gateway Deployment still asks for '$_intended'"; return 1 ;;
    esac

    kc wait --for=condition=complete --timeout=10m job/vitalmesh-migrate >/dev/null 2>&1 || true
    for d in vitalmesh-processor vitalmesh-api-gateway; do
        kc rollout status "deployment/$d" --timeout=10m >/dev/null 2>&1 \
            || { fail "$1: rollout of $d did not complete"; kc get pods; return 1; }
    done

    # `rollout status` returns when the NEW pods are Ready, while the old
    # ones are still inside their termination grace period -- still
    # running, still answering, still selected by a port-forward. Reading
    # a version here without waiting for them to go reports the release
    # that was just replaced, which is how this rehearsal first "passed"
    # a deployment that had not happened.
    for pair in "api-gateway=$3" "processor=$4"; do
        settle "${pair%%=*}" "${pair#*=}" || { fail "$1: pods from the previous release are still running for ${pair%%=*}"; return 1; }
    done
    return 0
}

# The port kubectl reported taking, once it has said so.
chosen_port() { sed -n 's#^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) ->.*#\1#p' "$1" | head -n 1; }

forward() {
    [ -n "$gw_pf" ] && kill "$gw_pf" 2>/dev/null || true
    [ -n "$pr_pf" ] && kill "$pr_pf" 2>/dev/null || true
    : > "$WORK/gw-pf.log"
    : > "$WORK/pr-pf.log"
    kc port-forward "service/vitalmesh-api-gateway" ":8080" > "$WORK/gw-pf.log" 2>&1 &
    gw_pf=$!
    kc port-forward "service/vitalmesh-processor" ":8081" > "$WORK/pr-pf.log" 2>&1 &
    pr_pf=$!
    GATEWAY_PORT=""
    PROCESSOR_PORT=""
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
        [ -z "$GATEWAY_PORT" ] && GATEWAY_PORT="$(chosen_port "$WORK/gw-pf.log")"
        [ -z "$PROCESSOR_PORT" ] && PROCESSOR_PORT="$(chosen_port "$WORK/pr-pf.log")"
        if [ -n "$GATEWAY_PORT" ] && [ -n "$PROCESSOR_PORT" ]             && curl -fsS -m 3 "http://localhost:$GATEWAY_PORT/health" >/dev/null 2>&1             && curl -fsS -m 3 "http://localhost:$PROCESSOR_PORT/health" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    return 1
}

# What is actually running, asked four different ways.
gateway_version()   { curl -fsS -m 5 "http://localhost:$GATEWAY_PORT/health" 2>/dev/null | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p' | head -n 1; }
processor_version() { curl -fsS -m 5 "http://localhost:$PROCESSOR_PORT/health" 2>/dev/null | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p' | head -n 1; }
algorithm_in_service() {
    curl -fsS -m 5 "http://localhost:$GATEWAY_PORT/metrics" 2>/dev/null \
        | sed -n 's/^vitalmesh_build_info{.*algorithm_version="\([^"]*\)".*/\1/p' | head -n 1
}
schema_in_service() {
    curl -fsS -m 5 "http://localhost:$GATEWAY_PORT/metrics" 2>/dev/null \
        | sed -n 's/^vitalmesh_database_schema_version \([0-9]*\)$/\1/p' | head -n 1
}
# The gateway reads the schema version once, at start-up, so a pod that
# has only just replaced another may not have published it yet.
schema_when_published() {
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        _s="$(schema_in_service)"
        [ -n "$_s" ] && { printf %s "$_s"; return 0; }
        sleep 3
    done
    return 1
}
algorithm_in_cluster() { kc get configmap vitalmesh-api-gateway -o jsonpath='{.data.PROCESSING_ALGORITHM_VERSION}' 2>/dev/null; }
# The reference the pod asks for, and what the node actually resolved it to.
pod_image()  { kc get pods -l "app.kubernetes.io/name=$1" -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null; }
pod_imageid(){ kc get pods -l "app.kubernetes.io/name=$1" -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null; }

expect() {
    # expect <description> <actual> <wanted>
    if [ "$2" = "$3" ]; then ok "$1 = $3"; else fail "$1 = '$2', expected '$3'"; fi
}
expect_digest() {
    # expect_digest <description> <actual reference> <wanted digest>
    case "$2" in
        *"$3") ok "$1 is $(printf '%s' "${3#sha256:}" | cut -c1-12)" ;;
        *) fail "$1 is '$2', expected to end in $3" ;;
    esac
}

step "Deploy release $RELEASE_A (the release that will be rolled back to)"
deploy "release $RELEASE_A" "$WORK/release-a.yaml" "$GATEWAY_A_DIGEST" "$PROCESSOR_A_DIGEST" || exit 1
forward || { fail "could not reach the services"; exit 1; }
expect "gateway version" "$(gateway_version)" "$RELEASE_A"
expect "processor version" "$(processor_version)" "$RELEASE_A"
expect "algorithm version in service" "$(algorithm_in_service)" "$ALGORITHM_A"
# The schema version is read at the point it matters -- immediately
# before the rollback -- rather than at first boot, where a gateway that
# started while its database was still coming up reports nothing at all
# (the metric is absent rather than zero, by design).

step "Deploy release $RELEASE_B (the release with the problem)"
deploy "release $RELEASE_B" "$WORK/release-b.yaml" "$GATEWAY_B_DIGEST" "$PROCESSOR_B_DIGEST" || exit 1
forward || { fail "could not reach the services"; exit 1; }
expect "gateway version" "$(gateway_version)" "$RELEASE_B"
expect "processor version" "$(processor_version)" "$RELEASE_B"
expect "algorithm version in service" "$(algorithm_in_service)" "$ALGORITHM_B"
note "the gateway now asks for algorithm $ALGORITHM_B, which the processor does not implement"

# ------------------------------------------- what `rollout undo` does not do
step "For contrast: kubectl rollout undo"
# Run first, so the rollback proper starts from a known state either way.
for d in vitalmesh-api-gateway vitalmesh-processor; do
    kc rollout undo "deployment/$d" >/dev/null 2>&1 || true
done
for d in vitalmesh-api-gateway vitalmesh-processor; do
    kc rollout status "deployment/$d" --timeout=5m >/dev/null 2>&1 || true
done
# As after any rollout: the pods being replaced are still answering
# until their grace period ends, so wait for them to go before reading
# anything, or the reads describe the release that is leaving.
settle api-gateway "$GATEWAY_A_DIGEST" || fail "pods from release $RELEASE_B are still running after the undo"
settle processor "$PROCESSOR_A_DIGEST" || fail "pods from release $RELEASE_B are still running after the undo"
forward || true
undo_gateway="$(gateway_version)"
undo_algorithm_cluster="$(algorithm_in_cluster)"
undo_algorithm_service="$(algorithm_in_service)"
if [ "$undo_gateway" = "$RELEASE_A" ]; then
    ok "rollout undo restored the image: the gateway reports $undo_gateway"
else
    fail "rollout undo did not restore the image (gateway reports '$undo_gateway')"
fi
if [ "$undo_algorithm_cluster" = "$ALGORITHM_B" ] && [ "$undo_algorithm_service" = "$ALGORITHM_B" ]; then
    ok "rollout undo left the configuration at release $RELEASE_B's value ($ALGORITHM_B), as expected"
    note "the ConfigMap is a named object, not a generated one, so the restored"
    note "pod template still points at it and picks up whatever it holds now."
    note "This is why redeploying the previous release is the rollback and"
    note "rollout undo is only the emergency stop (docs/ROLLBACK.md)."
else
    fail "rollout undo changed the configuration unexpectedly (cluster '$undo_algorithm_cluster', in service '$undo_algorithm_service')"
fi

# Put release B back, so the rollback below starts from the broken release.
step "Back to release $RELEASE_B, to roll back from it properly"
deploy "release $RELEASE_B" "$WORK/release-b.yaml" "$GATEWAY_B_DIGEST" "$PROCESSOR_B_DIGEST" || exit 1
forward || { fail "could not reach the services"; exit 1; }
expect "gateway version" "$(gateway_version)" "$RELEASE_B"

# ------------------------------------------------------------------- rollback
SCHEMA_BEFORE="$(schema_when_published || true)"
note "schema version before the rollback: ${SCHEMA_BEFORE:-unread}"

step "Rollback: redeploy release $RELEASE_A by digest"
rollback_start="$(now)"
deploy "release $RELEASE_A" "$WORK/release-a.yaml" "$GATEWAY_A_DIGEST" "$PROCESSOR_A_DIGEST" || exit 1
rollback_end="$(now)"
forward || { fail "could not reach the services after the rollback"; exit 1; }

step "Verification"
printf '  Go deployment\n'
expect_digest "  the gateway pod's image reference" "$(pod_image api-gateway)" "$GATEWAY_A_DIGEST"
expect_digest "  the gateway image the node resolved" "$(pod_imageid api-gateway)" "$GATEWAY_A_DIGEST"
expect "  the gateway's own reported version" "$(gateway_version)" "$RELEASE_A"

printf '  Rust deployment\n'
expect_digest "  the processor pod's image reference" "$(pod_image processor)" "$PROCESSOR_A_DIGEST"
expect_digest "  the processor image the node resolved" "$(pod_imageid processor)" "$PROCESSOR_A_DIGEST"
expect "  the processor's own reported version" "$(processor_version)" "$RELEASE_A"

printf '  Kubernetes manifests\n'
expect "  the ConfigMap in the cluster" "$(algorithm_in_cluster)" "$ALGORITHM_A"
expect "  the value the running gateway is using" "$(algorithm_in_service)" "$ALGORITHM_A"

printf '  Container image versions\n'
if pod_image api-gateway | grep -q '@sha256:' && pod_image processor | grep -q '@sha256:'; then
    ok "  both pods name their image by digest, not by tag"
else
    fail "  a pod names its image by tag"
fi

printf '  Database schema (must NOT have been rolled back)\n'
SCHEMA_AFTER="$(schema_when_published || true)"
if [ -n "$SCHEMA_BEFORE" ] && [ "$SCHEMA_AFTER" = "$SCHEMA_BEFORE" ]; then
    ok "  schema version is still $SCHEMA_AFTER; the rollback did not touch the database"
elif [ -z "$SCHEMA_AFTER" ]; then
    fail "  the schema version could not be read after the rollback"
else
    fail "  schema version moved from $SCHEMA_BEFORE to $SCHEMA_AFTER; migrations must be forward-only"
fi

printf '\n        rollback took %s seconds (apply to both rollouts complete)\n' \
    "$((rollback_end - rollback_start))"

# --------------------------------- the operator-facing verification, run
# The same script docs/ROLLBACK.md tells an operator to run against
# staging or production, run here against this cluster. A runbook step
# that has never been executed is a guess; this makes the documented
# verification the tested one.
step "scripts/rollback-verify.sh, as an operator would run it"
if NAMESPACE="$NAMESPACE" KUBE_CONTEXT="kind-$CLUSTER" EXPECTED_VERSION="$RELEASE_A" EXPECTED_GATEWAY_DIGEST="$GATEWAY_A_DIGEST" EXPECTED_PROCESSOR_DIGEST="$PROCESSOR_A_DIGEST" GATEWAY_URL="http://localhost:$GATEWAY_PORT" sh scripts/rollback-verify.sh; then
    ok "rollback-verify.sh agrees the environment is running release $RELEASE_A"
else
    fail "rollback-verify.sh did not agree the environment is running release $RELEASE_A"
fi

# And it has to be able to say no, or its agreement means nothing.
step "rollback-verify.sh rejects the release that was rolled back from"
if NAMESPACE="$NAMESPACE" KUBE_CONTEXT="kind-$CLUSTER" EXPECTED_VERSION="$RELEASE_B" EXPECTED_GATEWAY_DIGEST="$GATEWAY_B_DIGEST" EXPECTED_PROCESSOR_DIGEST="$PROCESSOR_B_DIGEST" GATEWAY_URL="http://localhost:$GATEWAY_PORT" sh scripts/rollback-verify.sh >/dev/null 2>&1; then
    fail "rollback-verify.sh accepted release $RELEASE_B while release $RELEASE_A is running"
else
    ok "rollback-verify.sh refuses release $RELEASE_B: it checks, rather than agreeing"
fi

# ---------------------------------------------------------------------- done
printf '\n'
if [ "$failures" -eq 0 ]; then
    printf 'rollback: the procedure restored the images, the versions and the configuration, and left the schema alone\n'
    [ "$KEEP" -eq 1 ] && printf 'cluster kind-%s and registry %s left running\n' "$CLUSTER" "$REGISTRY_NAME"
    exit 0
fi
printf 'rollback: %d check(s) failed\n' "$failures"
exit 1
