#!/bin/sh
# Go statement coverage against a floor.
#
#   sh scripts/coverage-floor.sh <profile> <minimum percent> <label>
#   sh scripts/coverage-floor.sh merge <output> <profile>...
#
# The first form prints the profile's total and fails if it is below the
# minimum. The second concatenates several profiles into one: `go tool
# cover` merges blocks that appear in more than one profile, so a
# statement hit by a unit test and again by an end-to-end test is counted
# once, and the total is what the whole suite covers.
#
# The floors live in the Makefile (GO_COVERAGE_MIN for the unit profile,
# GO_COVERAGE_MIN_ALL for the merged one) and only ever go up.
set -eu

if [ "${1:-}" = "merge" ]; then
    out="$2"; shift 2
    first=1
    : > "$out"
    for profile in "$@"; do
        [ -s "$profile" ] || continue
        if [ "$first" = 1 ]; then
            cat "$profile" >> "$out"
            first=0
        else
            tail -n +2 "$profile" >> "$out"
        fi
    done
    [ -s "$out" ] || { echo "coverage: no profiles to merge" >&2; exit 1; }
    exit 0
fi

profile="${1:?profile}"; minimum="${2:?minimum percent}"; label="${3:-coverage}"
[ -s "$profile" ] || { echo "coverage ($label): $profile is missing or empty" >&2; exit 1; }

total="$(go tool cover -func="$profile" | tail -n 1 | awk '{print $NF}' | tr -d '%')"
case "$total" in ''|*[!0-9.]*) echo "coverage ($label): could not read a total from $profile" >&2; exit 1 ;; esac

if awk -v t="$total" -v m="$minimum" 'BEGIN { exit !(t + 0 >= m + 0) }'; then
    printf 'coverage (%s): %s%% of statements, floor %s%%\n' "$label" "$total" "$minimum"
else
    printf 'coverage (%s): %s%% of statements is below the floor of %s%%\n' "$label" "$total" "$minimum" >&2
    printf 'The floor only ever goes up (docs/DEVELOPMENT.md): add tests rather than lowering it.\n' >&2
    exit 1
fi
