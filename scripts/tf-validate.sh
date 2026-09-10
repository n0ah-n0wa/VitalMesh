#!/bin/sh
# Static checks for the Terraform, none of which needs an AWS account.
#
#   terraform fmt       every file formatted the one way
#   terraform validate  each root, with the modules it calls, internally
#                       consistent against the pinned providers' schemas
#   terraform test      each root planned against a mocked AWS provider,
#                       which evaluates what validate cannot: the validation
#                       rules on every module's inputs, among them the
#                       production floors in modules/platform, which
#                       environments/production/tests/floors.tftest.hcl
#                       breaks one at a time
#   trivy config        known misconfigurations in the Terraform itself
#   checkov             the same question asked by a different rule set
#
# `init -backend=false` skips the S3 backend, `validate` never configures a
# provider, and the tests replace the AWS provider with a mock, so no
# credentials are needed and none are read. What this cannot say is whether
# AWS will accept a value, such as an instance type in a region, or whether an
# add-on exists for a Kubernetes version: those are a real `plan`'s job,
# against a real account.
#
# Everything runs in a pinned container, as the rest of this repository's
# tooling does.
set -eu

TERRAFORM_IMAGE="hashicorp/terraform:1.16.2"
TRIVY_IMAGE="aquasec/trivy:0.58.2"
CHECKOV_IMAGE="bridgecrew/checkov:3.2.334"

TF_DIR="infrastructure/terraform"
ROOTS="bootstrap environments/staging environments/production"

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
indent() { sed 's/^/        /'; }

# Docker on Windows needs a Windows path; MSYS hands this script a POSIX one.
host_path() {
    if command -v cygpath >/dev/null 2>&1; then
        cygpath -m "$1"
    else
        printf '%s' "$1"
    fi
}

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

if [ ! -d "$TF_DIR/modules" ]; then
    echo "run this from the repository root: $TF_DIR/modules not found" >&2
    exit 2
fi

repo="$(host_path "$(pwd)")"

# One provider cache shared by every root, so the AWS provider (well over a
# hundred megabytes) is downloaded once rather than once per root. The roots
# run one after another because the cache is not safe for two writers.
tf() {
    workdir="$1"
    shift
    docker run --rm \
        -v "$repo/$TF_DIR:/tf" -w "/tf/$workdir" \
        -v vitalmesh-tf-plugins:/plugins -e TF_PLUGIN_CACHE_DIR=/plugins \
        -e TF_IN_AUTOMATION=1 \
        "$TERRAFORM_IMAGE" "$@"
}

# Pulled first, so no step's captured output can be Docker's progress rather
# than the tool's. That exact mix-up once put "Pulling from ..." inside a
# rendered Kubernetes manifest (see scripts/k8s-validate.sh).
printf '\nTools\n'
pulled=0
for image in "$TERRAFORM_IMAGE" "$TRIVY_IMAGE" "$CHECKOV_IMAGE"; do
    docker image inspect "$image" >/dev/null 2>&1 && continue
    pulled=$((pulled + 1))
    docker pull -q "$image" >/dev/null 2>&1 || fail "could not pull $image"
done
if [ "$pulled" -eq 0 ]; then
    ok "all three tool images already present"
else
    ok "pulled $pulled tool image(s)"
fi

printf '\nFormat (terraform fmt)\n'
if out="$(tf . fmt -check -recursive -diff -no-color 2>&1)"; then
    ok "every file is formatted"
else
    fail "some files need terraform fmt"
    printf '%s\n' "$out" | indent
fi

printf '\nValidate, then plan against a mocked AWS (terraform validate, terraform test)\n'
for root in $ROOTS; do
    # -lockfile=readonly: the committed .terraform.lock.hcl is used as it is.
    # A provider that does not match its recorded checksum, or a lock file
    # that would need changing, fails here rather than being quietly
    # rewritten, which is the point of committing it.
    if ! out="$(tf "$root" init -backend=false -input=false -lockfile=readonly -no-color 2>&1)"; then
        fail "$root: terraform init failed"
        printf '%s\n' "$out" | indent
        continue
    fi
    if ! out="$(tf "$root" validate -no-color 2>&1)"; then
        fail "$root: terraform validate"
        printf '%s\n' "$out" | indent
        continue
    fi
    # validate never evaluates the validation rules on a module's inputs, so
    # on its own it passes production with a single-AZ database. The tests
    # (tests/*.tftest.hcl) plan the root with the AWS provider mocked, which
    # evaluates them and everything else a plan would.
    if out="$(tf "$root" test -no-color 2>&1)"; then
        ok "$root: $(printf '%s\n' "$out" | tail -n 1)"
    else
        fail "$root: terraform test"
        printf '%s\n' "$out" | indent
    fi
done

# Suppressions for both scanners are inline, as a #trivy:ignore or
# #checkov:skip comment on the resource concerned with the argument beside
# it. The one exception is the policy-wide skip in .checkov.yaml.
printf '\nMisconfiguration (trivy)\n'
if out="$(docker run --rm -v vitalmesh-trivy:/root/.cache/trivy \
    -v "$repo/$TF_DIR:/scan:ro" \
    "$TRIVY_IMAGE" config --severity HIGH,CRITICAL --exit-code 1 --quiet \
    --skip-dirs "**/.terraform" /scan 2>&1)"; then
    ok "no HIGH or CRITICAL misconfiguration"
else
    fail "trivy findings"
    printf '%s\n' "$out" | indent
fi

printf '\nMisconfiguration, second opinion (checkov)\n'
if out="$(docker run --rm -v "$repo/$TF_DIR:/scan:ro" \
    "$CHECKOV_IMAGE" -d /scan --config-file /scan/.checkov.yaml --compact --quiet 2>&1)"; then
    ok "no findings outside the documented skips"
else
    fail "checkov findings"
    printf '%s\n' "$out" | grep -E "^Check:|FAILED for resource" | head -40 | indent
fi

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'terraform: %d check(s) failed\n' "$failures"
    exit 1
fi
printf 'terraform: all checks passed (%s roots)\n' "$(printf '%s' "$ROOTS" | wc -w | tr -d ' ')"
