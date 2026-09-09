#!/bin/sh
# Validates every Kubernetes manifest this repository can produce, without
# a cluster.
#
# "Every manifest" means every overlay, not the base alone. An overlay can
# render something the base never contained — an Ingress, a narrowed
# NetworkPolicy, a patched resource limit — and a base that validates says
# nothing about what is actually deployed. So each overlay is rendered and
# the rendered output is what the tools see, which is also what `kubectl
# apply -k` would send.
#
# Four tools, because each answers a different question and none of them
# answers another's:
#
#   kustomize    does the overlay assemble at all
#   kubeconform  is every object valid against the Kubernetes API schema,
#                in strict mode, so an unknown field is an error rather
#                than something the API server would quietly drop
#   kube-linter  are the objects consistent and sensibly configured — does
#                a Service select a pod that exists, does an HPA target a
#                Deployment that exists, does every container have probes,
#                limits and a security context
#   trivy        does any object trip a known Kubernetes misconfiguration
#   checkov      the same question asked by a different engine, which
#                disagrees usefully: it was the only one to object to the
#                local fixtures' low uids and to the image pull policy
#
# What none of them does is talk to an API server, so admission — including
# the namespaces' own Pod Security enforcement — is not covered here. That
# is what scripts/k8s-local-test.sh does against a real kind cluster.
#
# Everything runs in a pinned container: this repository has no local
# Kubernetes toolchain and does not want one.
set -eu

KUSTOMIZE_IMAGE="registry.k8s.io/kustomize/kustomize:v5.4.3"
KUBECONFORM_IMAGE="ghcr.io/yannh/kubeconform:v0.6.7"
KUBE_LINTER_IMAGE="stackrox/kube-linter:v0.7.1"
TRIVY_IMAGE="aquasec/trivy:0.58.2"
CHECKOV_IMAGE="bridgecrew/checkov:3.2.334"

# The versions the manifests are claimed to work on. 1.30 is the floor: the
# preStop sleep handler is GA there. Both are checked so that a field valid
# in one and not the other cannot pass unnoticed.
K8S_VERSIONS="1.30.0 1.31.0"

KUBE_DIR="infrastructure/kubernetes"
# Rendered output goes inside the repository rather than /tmp, because it
# has to be bind-mounted into a container and a Windows host cannot mount
# an MSYS temporary path. It is git-ignored and rebuilt every run.
RENDER_DIR="$KUBE_DIR/.rendered"

TARGETS="base overlays/local overlays/staging overlays/production"

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

# Docker on Windows needs a Windows path; MSYS hands this script a POSIX
# one. cygpath -m gives C:/like/this, which Docker accepts on both.
host_path() {
    if command -v cygpath >/dev/null 2>&1; then
        cygpath -m "$1"
    else
        printf '%s' "$1"
    fi
}

# Stops MSYS rewriting the container-side paths in the arguments below.
MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

if [ ! -d "$KUBE_DIR/base" ]; then
    echo "run this from the repository root: $KUBE_DIR/base not found" >&2
    exit 2
fi

repo="$(host_path "$(pwd)")"

rm -rf "$RENDER_DIR"
mkdir -p "$RENDER_DIR"

printf '\nRendering\n'
for target in $TARGETS; do
    slug="$(printf '%s' "$target" | tr '/' '-')"
    out="$RENDER_DIR/$slug.yaml"
    if rendered="$(docker run --rm -v "$repo/$KUBE_DIR:/work" -w /work \
        "$KUSTOMIZE_IMAGE" build "$target" 2>&1)"; then
        printf '%s\n' "$rendered" > "$out"
        ok "$target -> $(grep -c '^kind:' "$out") objects"
    else
        fail "$target: kustomize build failed"
        printf '%s\n' "$rendered" | sed 's/^/        /'
    fi
done

if [ "$failures" -ne 0 ]; then
    printf '\nkubernetes: nothing rendered, stopping\n'
    exit 1
fi

printf '\nSchema (kubeconform, strict)\n'
for target in $TARGETS; do
    slug="$(printf '%s' "$target" | tr '/' '-')"
    for version in $K8S_VERSIONS; do
        # -strict rejects unknown fields; -summary keeps the output to a
        # line.
        #
        # The `tail` that trims the summary is deliberately not part of
        # this pipeline. A pipeline's status in POSIX sh is its last
        # command's, so piping kubeconform into tail here would report
        # tail's success and this gate could never fail — which is how it
        # was first written, and what a deliberately invalid manifest
        # caught.
        out="$(docker run --rm -i "$KUBECONFORM_IMAGE" \
            -strict -summary -kubernetes-version "$version" - \
            < "$RENDER_DIR/$slug.yaml" 2>&1)" && rc=0 || rc=$?
        summary="$(printf '%s' "$out" | tail -1)"

        # Exit code and summary are both checked. Either would do today;
        # together they survive a tool that changes which one it reports
        # through. A skipped resource is one no schema was found for, so
        # nothing validated it at all.
        if [ "$rc" -ne 0 ]; then
            fail "$target on $version: $summary"
        elif ! printf '%s' "$summary" | grep -q 'Invalid: 0'; then
            fail "$target on $version: $summary"
        elif ! printf '%s' "$summary" | grep -q 'Errors: 0'; then
            fail "$target on $version: $summary"
        elif ! printf '%s' "$summary" | grep -q 'Skipped: 0'; then
            fail "$target on $version: resources were skipped, so nothing checked them"
        else
            ok "$target on $version: $(printf '%s' "$summary" | sed 's/^Summary: //')"
        fi
    done
done

printf '\nConsistency and configuration (kube-linter)\n'
# The check set lives in $KUBE_DIR/.kube-linter.yaml so that what is
# enforced, and what is excluded and why, is reviewable rather than being
# an argument list here.
for target in $TARGETS; do
    slug="$(printf '%s' "$target" | tr '/' '-')"
    if out="$(docker run --rm -i \
        -v "$repo/$KUBE_DIR/.kube-linter.yaml:/config.yaml:ro" \
        "$KUBE_LINTER_IMAGE" lint --config /config.yaml --format plain - \
        < "$RENDER_DIR/$slug.yaml" 2>&1)"; then
        ok "$target"
    else
        fail "$target: kube-linter findings"
        printf '%s\n' "$out" | sed 's/^/        /'
    fi
done

printf '\nMisconfiguration (trivy)\n'
# Scanned as rendered rather than as source. Source scanning would feed
# trivy the overlay patch fragments, which are not whole objects — a patch
# that sets only resource limits has no security context, and every one of
# them would be reported as missing everything it does not mention.
if out="$(docker run --rm -v vitalmesh-trivy:/root/.cache/trivy \
    -v "$repo/$RENDER_DIR:/scan" -v "$repo/$KUBE_DIR/.trivyignore.yaml:/ignore.yaml:ro" \
    "$TRIVY_IMAGE" config --severity HIGH,CRITICAL --exit-code 1 --quiet \
    --ignorefile /ignore.yaml /scan 2>&1)"; then
    ok "no HIGH or CRITICAL misconfiguration in any overlay"
else
    fail "trivy findings"
    printf '%s\n' "$out" | sed 's/^/        /'
fi

printf '\nMisconfiguration, second opinion (checkov)\n'
# A different rule set over the same rendered files. The three checks it is
# configured to skip are argued in $KUBE_DIR/.checkov.yaml; everything else
# is enforced.
if out="$(docker run --rm \
    -v "$repo/$RENDER_DIR:/scan:ro" \
    -v "$repo/$KUBE_DIR/.checkov.yaml:/checkov.yaml:ro" \
    "$CHECKOV_IMAGE" -d /scan --config-file /checkov.yaml --compact --quiet 2>&1)"; then
    ok "no findings outside the documented skips"
else
    fail "checkov findings"
    printf '%s\n' "$out" | grep -E "^Check:|FAILED for resource" | sed 's/^/        /' | head -20
fi

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'kubernetes: %d check(s) failed\n' "$failures"
    exit 1
fi
printf 'kubernetes: all checks passed (%s)\n' "$(printf '%s' "$TARGETS" | wc -w | tr -d ' ') targets"
