#!/bin/sh
# Checks that the security gates still enforce the policy documented in
# docs/SECURITY.md ("Vulnerability and dependency policy").
#
# The scanners themselves answer "is this code vulnerable?". This answers a
# different question, and one nothing else asks: "does this repository still
# block what it says it blocks?" Every gate here is one line in a Makefile
# or a workflow, and every one of them can be weakened by deleting a flag —
# lowering a severity, dropping --exit-code, quietly adding
# --ignore-unfixed, or removing a gate from ci-local. That change would make
# every scan pass, which is the failure mode this guards against.
#
# It also checks that every suppression carries a reason, because a
# suppression without one cannot be reviewed.
#
# Needs nothing but a shell: it reads the repository, runs no scanner.
set -eu

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
step() { printf '\n%s\n' "$1"; }

cd "$(dirname "$0")/.." || exit 1

want() {
    # want <description> <file> <extended regex>
    if grep -Eq -- "$3" "$2"; then ok "$1"; else fail "$1 ($2 does not match: $3)"; fi
}
reject() {
    # reject <description> <file> <extended regex that must NOT appear>
    if grep -Eq -- "$3" "$2"; then fail "$1 ($2 matches what the policy forbids: $3)"; else ok "$1"; fi
}

step "Severity thresholds"
want "trivy blocks HIGH and CRITICAL" Makefile \
    '^TRIVY_SEVERITY[[:space:]]*:?=.*--severity[[:space:]]+HIGH,CRITICAL'
want "trivy fails the build on a finding" Makefile \
    '^TRIVY_SEVERITY[[:space:]]*:?=.*--exit-code[[:space:]]+1'
want "trivy scans vulnerabilities, secrets and misconfiguration in images" Makefile \
    '^TRIVY_VULN[[:space:]]*:?=.*--scanners[[:space:]]+vuln,secret,misconfig'
# An unfixed vulnerability still blocks: the policy says the escape hatch is
# a documented suppression, not a blanket flag.
reject "no blanket --ignore-unfixed anywhere" Makefile '--ignore-unfixed'
want "gosec blocks at medium severity and above" Makefile \
    '\$\(GOSEC\).*-severity=medium'

step "The gates exist and run"
want "deps-scan runs govulncheck over the call graph" Makefile 'deps-scan:'
want "govulncheck is pinned" Makefile '^GOVULNCHECK[[:space:]]*\?=.*govulncheck@v'
want "gosec is pinned" Makefile '^GOSEC[[:space:]]*\?=.*gosec@v'
want "gitleaks scans the git history" Makefile 'git --config /repo/\.gitleaks\.toml'
want "gitleaks scans the working tree" Makefile 'dir --config /repo/\.gitleaks\.toml'
want "gitleaks fails on a finding" Makefile '\$\(GITLEAKS_IMAGE\).*--exit-code 1'
want "lockfile integrity is checked for Go" Makefile 'go mod verify'
want "go.mod is checked for drift" Makefile 'go mod tidy -diff'
want "Cargo.lock is checked for drift" Makefile 'cargo fetch --locked'
want "an SBOM is generated" Makefile '^sbom:'

step "Every security gate is in ci-local"
for gate in verify sast deps-scan secret-scan docker-scan sbom k8s-validate tf-validate; do
    if grep -Eq "^ci-local:.*[[:space:]]$gate([[:space:]]|$)" Makefile; then
        ok "ci-local runs $gate"
    else
        fail "ci-local does not run $gate"
    fi
done

step "Every security gate is in CI"
CI=".github/workflows/ci.yml"
for target in "make sast" "make deps-scan" "make secret-scan" "make docker-scan" "make sbom" "make k8s-validate" "make tf-validate" "make deps-verify-go" "make deps-verify-rust"; do
    if grep -Fq "$target" "$CI"; then ok "ci.yml runs $target"; else fail "ci.yml does not run $target"; fi
done

step "Suppressions carry a reason"
# gosec: every #nosec must name the rule and give a reason after `--`.
bad_nosec="$(grep -rn '#nosec' --include='*.go' services/api-gateway | grep -Ev '#nosec [A-Z][0-9]+ -- .+' || true)"
if [ -z "$bad_nosec" ]; then
    ok "every #nosec names a rule and a reason"
else
    fail "a #nosec has no rule or no reason:"
    printf '%s\n' "$bad_nosec" | sed 's/^/        /'
fi

# trivy manifest suppressions: every id must have a statement.
IGNORE="infrastructure/kubernetes/.trivyignore.yaml"
if [ -f "$IGNORE" ]; then
    ids="$(grep -c '^[[:space:]]*- id:' "$IGNORE" || true)"
    statements="$(grep -c 'statement:' "$IGNORE" || true)"
    if [ "${ids:-0}" -eq "${statements:-0}" ]; then
        ok "every trivy manifest suppression has a statement ($ids)"
    else
        fail "$ids trivy suppressions but $statements statements"
    fi
fi

# checkov in Terraform: every inline skip must carry a reason after the id.
bad_skip="$(grep -rn 'checkov:skip=' infrastructure/terraform 2>/dev/null | grep -Ev 'checkov:skip=[A-Za-z0-9_]+:.+' || true)"
if [ -z "$bad_skip" ]; then
    ok "every Terraform checkov skip carries a reason"
else
    fail "a checkov skip has no reason:"
    printf '%s\n' "$bad_skip" | sed 's/^/        /'
fi

step "Dependency updates are proposed automatically"
DEP=".github/dependabot.yml"
for eco in github-actions gomod cargo docker; do
    if grep -Fq "package-ecosystem: $eco" "$DEP"; then
        ok "dependabot covers $eco"
    else
        fail "dependabot does not cover $eco"
    fi
done

step "Third-party actions are pinned to a commit"
# Any `uses:` naming a third-party action must end in a 40-hex SHA. Local
# (./) and reusable workflow references in this repository are exempt.
unpinned="$(grep -rhn 'uses:' .github/workflows/*.yml \
    | sed 's/.*uses:[[:space:]]*//' \
    | grep -v '^\./' \
    | grep -Ev '@[0-9a-f]{40}([[:space:]]|$)' || true)"
if [ -z "$unpinned" ]; then
    ok "every third-party action is pinned to a 40-character commit"
else
    fail "an action is not pinned to a commit:"
    printf '%s\n' "$unpinned" | sed 's/^/        /'
fi

step "Images are referenced immutably"
want "ECR refuses to move a tag" infrastructure/terraform/bootstrap/ecr.tf \
    'image_tag_mutability[[:space:]]*=[[:space:]]*"IMMUTABLE"'
want "ECR scans on push" infrastructure/terraform/bootstrap/ecr.tf 'scan_on_push[[:space:]]*=[[:space:]]*true'
want "both service images pin their base by digest" services/api-gateway/Dockerfile 'FROM.*@sha256:'
want "the processor image pins its base by digest" services/processor/Dockerfile 'FROM.*@sha256:'
reject "no image is referenced as :latest in a deployment" .github/workflows/deploy.yml ':latest'

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'security policy: %d check(s) failed\n' "$failures"
    exit 1
fi
printf 'security policy: the gates match docs/SECURITY.md\n'
