#!/bin/sh
# Checks that this build can say what it is.
#
# A release has to be identifiable after the fact: when something is wrong
# in production, the first question is "which build is this, exactly?" and
# every answer that follows -- which commit, which dependencies, which
# schema, which algorithm -- depends on the build being able to say. So this
# gate runs both binaries, reads the release record each one prints, and
# refuses a build whose record does not identify all of it
# (SPECIFICATIONS.md section 98, docs/RELEASE.md).
#
# It checks seven things are identifiable:
#
#   git commit            both records' version, and that they agree
#   Go version            the gateway's go_version
#   Rust version          the processor's rust_version
#   lockfile versions     a digest over each service's locked dependencies
#   image digest          that the deployment supplies it and the binary
#                         reports what it is given
#   migration version     the schema version the gateway's binary carries
#   algorithm version     both records, and that they agree
#
# And one property of the records themselves: that they disclose no
# secret, credential, endpoint, host or path. The binaries reject an
# incomplete record themselves, which is what makes the exit status
# above meaningful; that rejection is covered by unit tests.
#
# Needs both binaries built (`make build`) and nothing else: no database,
# no network, no Docker, no toolchain. The records are printed by binaries
# that read no configuration, so this gate works in a sandbox.
#
# It reads what the binaries say rather than what the source says, which
# is the point: a record that had stopped being derived from the build
# would still pass a source inspection.
set -eu

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
step() { printf '\n%s\n' "$1"; }

cd "$(dirname "$0")/.." || exit 1

GATEWAY_BIN=${GATEWAY_BIN:-bin/api-gateway}

# Cargo writes where CARGO_TARGET_DIR says, which the dev container sets to
# a volume so a Windows host is not in the build path. Both layouts are
# looked for, and either binary answers the same question.
processor_target=${CARGO_TARGET_DIR:-services/processor/target}
if [ -z "${PROCESSOR_BIN:-}" ]; then
    for candidate in "$processor_target/debug/processor" "$processor_target/release/processor"; do
        if [ -x "$candidate" ]; then PROCESSOR_BIN=$candidate; break; fi
    done
fi
PROCESSOR_BIN=${PROCESSOR_BIN:-$processor_target/debug/processor}

for binary in "$GATEWAY_BIN" "$PROCESSOR_BIN"; do
    if [ ! -x "$binary" ]; then
        printf 'release metadata: %s is not built; run `make build` first\n' "$binary" >&2
        exit 1
    fi
done

# field <json> <name> -- the value of a top-level field, without needing jq.
# The records are written by encoding/json and serde_json with one field per
# line, so a line-oriented read is exact here rather than approximate.
field() {
    printf '%s\n' "$1" |
        sed -n 's/^[[:space:]]*"'"$2"'"[[:space:]]*:[[:space:]]*"\{0,1\}\([^",]*\)"\{0,1\},\{0,1\}[[:space:]]*$/\1/p' |
        head -n 1
}

# matches <description> <value> <extended regex>
matches() {
    if printf '%s' "$2" | grep -Eq "^$3\$"; then
        ok "$1 = $2"
    else
        fail "$1 = '$2', which does not match $3"
    fi
}

step "Both binaries print a release record"

# A binary refuses to print an incomplete record: it exits non-zero and
# names the fields that identify nothing. So the exit status is the first
# check, and every field read below comes from a record the binary itself
# was willing to stand behind.
errors=$(mktemp)
trap 'rm -f "$errors"' EXIT INT TERM

if gateway_record=$("$GATEWAY_BIN" version 2>"$errors"); then
    ok "api-gateway version exits 0"
else
    fail "api-gateway version failed: $(cat "$errors")"
fi

if processor_record=$("$PROCESSOR_BIN" version 2>"$errors"); then
    ok "processor version exits 0"
else
    fail "processor version failed: $(cat "$errors")"
fi

printf '\n%s\n%s\n' "$gateway_record" "$processor_record"

step "1. Git commit"

gateway_commit=$(field "$gateway_record" version)
processor_commit=$(field "$processor_record" version)
# Seven or more hex characters: a short SHA. "dev" is what an unidentified
# build reports, and it is exactly what must not reach a release.
matches "gateway commit" "$gateway_commit" '[0-9a-f]{7,40}'
matches "processor commit" "$processor_commit" '[0-9a-f]{7,40}'

if [ "$gateway_commit" = "$processor_commit" ]; then
    ok "both services were built from the same commit"
else
    fail "the services disagree about the commit: gateway $gateway_commit, processor $processor_commit"
fi

if head=$(git rev-parse --short HEAD 2>/dev/null); then
    case "$gateway_commit" in
        "$head"*) ok "the commit reported is HEAD ($head)" ;;
        *) fail "the build reports $gateway_commit but HEAD is $head; the binaries are stale, run \`make build\`" ;;
    esac
else
    ok "no git repository here, so the reported commit cannot be cross-checked"
fi

step "2. Go version"

matches "gateway Go toolchain" "$(field "$gateway_record" go_version)" 'go1\.[0-9]+(\.[0-9]+)?'

step "3. Rust version"

matches "processor Rust toolchain" "$(field "$processor_record" rust_version)" 'rustc [0-9]+\.[0-9]+\.[0-9]+.*'

step "4. Dependency lockfile versions"

# Each service reports a digest over its locked dependency set. The digest
# names its own algorithm, so a record read years later says how to
# reproduce it; docs/RELEASE.md says why they differ.
matches "gateway module digest" "$(field "$gateway_record" dependencies)" 'sha256:[0-9a-f]{24}'
matches "processor lockfile digest" "$(field "$processor_record" dependencies)" 'fnv64:[0-9a-f]{16}'

for lockfile in services/api-gateway/go.sum services/processor/Cargo.lock; do
    if git ls-files --error-unmatch "$lockfile" >/dev/null 2>&1; then
        ok "$lockfile is committed, so the digest is reproducible from this tree"
    elif [ -f "$lockfile" ]; then
        fail "$lockfile exists but is not tracked; the locked versions are not in the repository"
    else
        fail "$lockfile is missing"
    fi
done

# A digest computed over no bytes identifies nothing, and it is the one
# failure the format check above cannot see: FNV-1a over an empty input is
# its offset basis, which still looks like a digest. That value means
# build.rs could not read Cargo.lock.
if [ "$(field "$processor_record" dependencies)" = "fnv64:cbf29ce484222325" ]; then
    fail "the processor's digest is the hash of no bytes; Cargo.lock was not read at build time"
else
    ok "the processor's digest was computed over a lockfile that was read"
fi
# That a digest follows its input is a property of the derivation rather
# than of this tree, so it is held by unit tests, where it can be checked
# against known inputs: the_lockfile_digest_is_stable_and_derived_from_the_file
# in services/processor/src/lib.rs and
# TestTheDigestDistinguishesDependencySetsAndIgnoresOrder in
# services/api-gateway/internal/buildinfo. This gate deliberately does not
# edit a lockfile to prove it: a gate that mutates a tracked file can leave
# it mutated.

step "5. Container image digest"

# A process cannot read its own image digest, so the deployment passes it
# in. Two halves, both of which have to hold: the binaries report what they
# are given, and the manifests give it to them.
digest=sha256:0b1e5b4c1a2f3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6
for pair in "api-gateway:$GATEWAY_BIN" "processor:$PROCESSOR_BIN"; do
    name=${pair%%:*}
    binary=${pair#*:}
    reported=$(IMAGE_DIGEST="$digest" "$binary" version 2>/dev/null | sed -n 's/.*"image_digest"[^"]*"\([^"]*\)".*/\1/p')
    if [ "$reported" = "$digest" ]; then
        ok "$name reports the image digest it is given"
    else
        fail "$name was given an image digest and reported '$reported'"
    fi
done

for manifest in infrastructure/kubernetes/base/deployment-api-gateway.yaml \
                infrastructure/kubernetes/base/deployment-processor.yaml; do
    if [ ! -f "$manifest" ]; then
        fail "$manifest is missing"
    elif grep -q 'IMAGE_DIGEST' "$manifest"; then
        ok "$(basename "$manifest") passes IMAGE_DIGEST to the container"
    else
        fail "$(basename "$manifest") does not set IMAGE_DIGEST, so the running pod cannot say which image it is"
    fi
done

step "6. Database migration version"

migration=$(field "$gateway_record" migration_version)
matches "gateway migration version" "$migration" '[1-9][0-9]*'

latest=$(ls services/api-gateway/migrations/*.up.sql 2>/dev/null |
    sed 's#.*/##; s/_.*//' | sort | tail -n 1 | sed 's/^0*//')
if [ -z "$latest" ]; then
    fail "no migrations were found to compare against"
elif [ "$migration" = "$latest" ]; then
    ok "the binary carries every migration in the repository (version $latest)"
else
    fail "the binary reports migration version $migration but the repository has $latest; the embedded migrations are stale"
fi

step "7. Processing algorithm version"

gateway_algorithm=$(field "$gateway_record" algorithm_version)
processor_algorithm=$(field "$processor_record" algorithm_version)
matches "gateway algorithm version" "$gateway_algorithm" '[0-9]+\.[0-9]+\.[0-9]+'
matches "processor algorithm version" "$processor_algorithm" '[0-9]+\.[0-9]+\.[0-9]+'

# Not cosmetic: the processor refuses a job whose algorithm version it does
# not implement, so a release whose halves disagree cannot process anything.
if [ "$gateway_algorithm" = "$processor_algorithm" ]; then
    ok "both services agree on the algorithm version"
else
    fail "the gateway requests $gateway_algorithm but the processor implements $processor_algorithm; every job would be refused"
fi

matches "processor contract version" "$(field "$processor_record" contract_version)" '[0-9]+\.[0-9]+\.[0-9]+'

step "The records disclose nothing sensitive"

# The records are printed on demand and the same values are published as
# metric labels, so anything sensitive that reached them would be
# published. Run with a full set of credentials in the environment: a
# record that read its configuration would show it here.
poisoned=$(
    DATABASE_URL='postgres://vitalmesh:canary-value-not-a-secret@db.internal:5432/vitalmesh' \
    REDIS_URL='redis://cache.internal:6379' \
    JWT_SECRET='canary-value-not-a-secret' \
    PROCESSOR_TOKEN='canary-value-not-a-secret' \
    AWS_SECRET_ACCESS_KEY='canary-value-not-a-secret' \
    OTEL_EXPORTER_OTLP_ENDPOINT='http://otel-collector.monitoring.svc.cluster.local:4318' \
        sh -c '"$1" version; "$2" version' sh "$GATEWAY_BIN" "$PROCESSOR_BIN" 2>/dev/null
)
leaked=0
for forbidden in canary password secret token \
                 'postgres://' 'redis://' amazonaws.com .svc.cluster.local \
                 db.internal cache.internal; do
    if printf '%s' "$poisoned" | grep -qiF -- "$forbidden"; then
        fail "a release record discloses '$forbidden'"
        leaked=1
    fi
done
[ "$leaked" -eq 0 ] && ok "neither record contains a credential, endpoint, host or path"

if [ "$failures" -eq 0 ]; then
    printf 'release metadata: every item is identifiable and nothing sensitive is exposed\n'
    exit 0
fi
printf 'release metadata: %d check(s) failed\n' "$failures"
exit 1
