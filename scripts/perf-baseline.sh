#!/bin/sh
# The performance baseline (docs/PERFORMANCE_BASELINE.md): one script that
# measures, in a fixed order, and writes a report. It optimises nothing.
#
#   make perf-baseline                 # against the running environment
#   RESET=1 make perf-baseline         # from an empty database first
#   DURATION=120s sh scripts/perf-baseline.sh
#   SKIP_LOAD=1 make perf-baseline     # only the single-client sections (2, 5, 6, 7), about 90 s:
#                                      # the quick before-and-after for a change
#
# Sections, each a measurement with a stated method:
#
#   1  environment           what the run happened on, and the idle footprint
#   2  sign-in cost          argon2 at the configured parameters, from a client
#   3  mixed load            the k6 standard profile (scripts/load-test.sh),
#                            with docker stats sampled every two seconds
#   4  server-side view      Prometheus over the load window: latency by
#                            route, by application operation and by database
#                            statement; processor job time; CPU and memory
#   5  batch ingestion       one client, batches of 100, 500 and 1000 readings
#   6  processing by size    one client, jobs over 60 to 60,000 readings:
#                            end-to-end time and the processor's own time
#   7  database              row counts, sizes, scan statistics and the plans
#                            of the hot queries on the data the run left
#
# Needs Docker, curl, and python3 (or python) for the Prometheus and
# job-size sections. Client-side timings (sections 2, 5 and 6) are taken
# from the host through Docker's port forwarding, over 127.0.0.1. The report goes to tests/load/results/baseline-<stamp>.md.
set -eu

cd "$(dirname "$0")/.."

# 127.0.0.1 rather than localhost: on Windows the name resolves to ::1 first,
# and the fallback to IPv4 costs about 200 ms per connection, which would
# be measured as if it were the gateway's.
GATEWAY="${GATEWAY_URL:-http://127.0.0.1:8080}"
PROM="${PROMETHEUS_URL:-http://localhost:9090}"
DURATION="${DURATION:-120s}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RESULTS="tests/load/results"
OUT="$RESULTS/baseline-$STAMP.md"
STATS="$RESULTS/stats-$STAMP.csv"
PASSWORD="perf-baseline-password-not-a-secret"
EMAIL="perf@vitalmesh.invalid"
# The first interpreter that actually runs: on Windows a `python3` on the
# PATH can be a Store shortcut that only prints an installation notice.
PY=""
for candidate in python3 python; do
    if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c 'import sys; sys.exit(0)' >/dev/null 2>&1; then
        PY="$candidate"; break
    fi
done

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
die()  { printf '\n  %s\n' "$*" >&2; exit 1; }
out()  { printf '%s\n' "$*" >> "$OUT"; }
psql() { docker compose exec -T postgres psql -U vitalmesh -d vitalmesh -X -q "$@"; }
json() { printf '%s' "$1" | sed -n 's/.*"'"$2"'":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1; }
# api <method> <path> <token> [body] -> body, with the request time appended
# on a last line as "time_total=<seconds>".
api() {
    method="$1"; path="$2"; token="$3"; body="${4:-}"
    if [ -n "$body" ]; then set -- --data-binary "@$body"; else set --; fi
    curl --silent --show-error --max-time 120 -X "$method" "$GATEWAY$path" \
        -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
        -w '\ntime_total=%{time_total}' "$@"
}
took() { printf '%s' "$1" | sed -n 's/^time_total=//p'; }
body() { printf '%s' "$1" | sed '$d'; }
# withbody <json> -> the path of a file holding it, for api; curl on
# Windows cannot read /dev/stdin, so a file it is.
BODY="$RESULTS/.body.json"
withbody() { printf '%s' "$1" > "$BODY"; printf '%s' "$BODY"; }

mkdir -p "$RESULTS"
: > "$OUT"

# ------------------------------------------------------------------ 1
step "1. Environment"
if [ "${RESET:-0}" = "1" ]; then
    note "RESET=1: removing the environment and its volumes, then starting it"
    docker compose down -v --remove-orphans -t 5 >/dev/null 2>&1 || true
    docker compose up -d --wait >/dev/null 2>&1 || die "docker compose up failed"
fi
curl --silent --fail --max-time 5 "$GATEWAY/health" >/dev/null 2>&1 || die "the gateway is not answering at $GATEWAY (make up)"
gw=$(curl --silent --max-time 5 "$GATEWAY/health")
pr=$(curl --silent --max-time 5 "http://127.0.0.1:${PROCESSOR_PORT:-8081}/health" || echo '{}')
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
dirty=$(git status --porcelain 2>/dev/null | grep -q . && echo " (working tree modified)" || true)
out "# Performance baseline, $STAMP"
out ""
out "Measurements of one run on one machine, produced by scripts/perf-baseline.sh."
out "docs/PERFORMANCE_BASELINE.md explains the method and what the numbers do not say."
out ""
out "## 1. Environment"
out ""
out '```'
out "date: $STAMP"
out "commit: $commit$dirty"
out "host: $(uname -srm)"
out "$(docker info --format 'Docker {{.ServerVersion}} on {{.OperatingSystem}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes memory available to containers' 2>/dev/null)"
out "compose: $(docker compose version --short 2>/dev/null || echo unknown)"
out "gateway: $(json "$gw" version), environment $(json "$gw" environment); processor: $(json "$pr" version)"
out "services: docker-compose.yml defaults; no CPU or memory limits; PostgreSQL 16, Redis 7; processor MAX_CONCURRENT_JOBS 4; gateway rate limit 300/min per account"
out "reset before the run: ${RESET:-0}"
out "other containers on the host during the run: $(docker ps --format '{{.Names}}' | grep -v '^vitalmesh-' | tr '\n' ' ')"
out '```'
out ""
out "Idle footprint before the run (docker stats, one sample):"
out ""
out '```'
docker stats --no-stream --format '{{.Name}}  cpu {{.CPUPerc}}  mem {{.MemUsage}}' | grep -E '^vitalmesh-(api-gateway|processor|postgres|redis)-1' >> "$OUT"
out '```'
out ""
note "written to $OUT"

# ------------------------------------------------------------------ 2
step "2. Sign-in cost"
printf '%s' "$PASSWORD" | docker compose run --rm --no-deps -T \
    -e DATABASE_URL="postgres://vitalmesh:vitalmesh@postgres:5432/vitalmesh?sslmode=disable" \
    api-gateway users create "$EMAIL" OPERATOR >/dev/null 2>&1 || true
login_times=""
i=0
while [ "$i" -lt 5 ]; do
    r=$(curl --silent --max-time 60 -X POST "$GATEWAY/api/v1/auth/login" -H 'Content-Type: application/json' \
        --data "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" -w '\ntime_total=%{time_total}')
    login_times="$login_times $(took "$r")"
    TOKEN=$(json "$r" access_token)
    i=$((i + 1))
done
[ -n "${TOKEN:-}" ] || die "sign-in failed"
hash_params=$(docker compose exec -T postgres psql -U vitalmesh -d vitalmesh -Atc "SELECT substring(password_hash from '^\\\$argon2id\\\$v=\\d+\\\$[^\\\$]+') FROM users WHERE email = '$EMAIL'" 2>/dev/null || echo unknown)
out "## 2. Sign-in cost"
out ""
out "Five sequential POST /api/v1/auth/login from the host, one at a time, as seen by curl (time_total, seconds)."
out "The password hash parameters are the gateway's defaults: \`$hash_params\`."
out ""
out '```'
out "login time_total (s):$login_times"
out '```'
out ""
note "$login_times"

# ------------------------------------------------------------------ 3
if [ "${SKIP_LOAD:-0}" = "1" ]; then
    step "3 and 4. Mixed load skipped (SKIP_LOAD=1): sections 5 to 7 measure single-client costs and need no load"
    out "## 3 and 4. Mixed load"
    out ""
    out "Skipped (SKIP_LOAD=1)."
    out ""
    START=$(date -u +%s); END=$START
else
step "3. Mixed load: k6 standard profile for $DURATION, docker stats every 2 s"
: > "$STATS"
( while :; do
      docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' 2>/dev/null | grep -E '^vitalmesh-(api-gateway|processor|postgres|redis)-1' | sed "s/^/$(date -u +%s),/"
      sleep 2
  done >> "$STATS" ) &
SAMPLER=$!
START=$(date -u +%s)
PROFILE=standard DURATION="$DURATION" sh scripts/load-test.sh > "$RESULTS/load-$STAMP.log" 2>&1 || note "the load run reported a failure; see $RESULTS/load-$STAMP.log"
END=$(date -u +%s)
kill "$SAMPLER" 2>/dev/null || true
wait "$SAMPLER" 2>/dev/null || true
report=$(ls -t "$RESULTS"/report-standard-*.md 2>/dev/null | head -1)
out "## 3. Mixed load"
out ""
out "The k6 standard profile (docs/LOAD_TESTING.md) for $DURATION, from $(date -u -d "@$START" +%H:%M:%SZ 2>/dev/null || date -u -r "$START" +%H:%M:%SZ) to $(date -u -d "@$END" +%H:%M:%SZ 2>/dev/null || date -u -r "$END" +%H:%M:%SZ) UTC. The client-side latency tables are in \`$(basename "$report")\`; the totals:"
out ""
sed -n '/^## Totals/,/^## Latency/p' "$report" | grep -E '^\|' >> "$OUT" || true
out ""
out "Container CPU and memory during the run (docker stats every 2 s; CPU is a share of one core, so 200% is two cores):"
out ""
out '| Container | samples | CPU avg | CPU max | memory max |'
out '|---|---|---|---|---|'
for c in api-gateway processor postgres redis; do
    grep ",vitalmesh-$c-1," "$STATS" | awk -F, -v name="$c" '
        { cpu=$3; sub(/%/, "", cpu); n++; sum+=cpu; if (cpu+0>max) max=cpu+0;
          mem=$4; sub(/ \/.*/, "", mem); memv=mem; unit=mem; sub(/[0-9.]+/, "", unit); sub(/[A-Za-z]+/, "", memv);
          mb = (unit=="GiB") ? memv*1024 : (unit=="MiB" ? memv : (unit=="KiB" ? memv/1024 : memv));
          if (mb>mmax) mmax=mb }
        END { if (n) printf "| %s | %d | %.0f%% | %.0f%% | %.0f MiB |\n", name, n, sum/n, max, mmax; else printf "| %s | 0 | - | - | - |\n", name }' >> "$OUT"
done
out ""
note "load run done; stats in $STATS"

# ------------------------------------------------------------------ 4
step "4. Server-side view from Prometheus over the load window"
out "## 4. Server-side view (Prometheus, over the load window)"
out ""
if [ -z "$PY" ]; then
    out "python3 was not found; this section was skipped."
    note "python3 not found: skipping"
elif ! curl --silent --fail --max-time 5 "$PROM/-/ready" >/dev/null 2>&1; then
    out "Prometheus is not running at $PROM; this section was skipped."
    note "Prometheus not running: skipping"
else
    W=$((END - START))
    "$PY" - "$PROM" "$END" "$W" >> "$OUT" <<'EOF'
import json, sys, urllib.parse, urllib.request
prom, at, window = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
w = f"{window}s"

def q(expr):
    url = f"{prom}/api/v1/query?" + urllib.parse.urlencode({"query": expr, "time": at})
    with urllib.request.urlopen(url, timeout=30) as r:
        d = json.load(r)
    return d.get("data", {}).get("result", [])

def table(title, keylabel, rows_by_key, cols):
    print(f"**{title}**\n")
    print("| " + keylabel + " | " + " | ".join(cols) + " |")
    print("|---|" + "---|" * len(cols))
    for k in sorted(rows_by_key):
        vals = rows_by_key[k]
        print("| " + k + " | " + " | ".join(vals.get(c, "-") for c in cols) + " |")
    print()

def quantiles(bucket, by, extra_rate=None, scale=1000.0, unit="ms"):
    rows = {}
    for p in ("0.5", "0.95", "0.99"):
        for s in q(f'histogram_quantile({p}, sum by (le, {by}) (rate({bucket}[{w}])))'):
            key = s["metric"].get(by, "?")
            v = float(s["value"][1])
            rows.setdefault(key, {})[f"p{int(float(p)*100)} ({unit})"] = "-" if v != v else f"{v*scale:.1f}"
    if extra_rate:
        for s in q(extra_rate):
            key = s["metric"].get(by, "?")
            rows.setdefault(key, {})["per second"] = f"{float(s['value'][1]):.1f}"
    return rows

http = quantiles("vitalmesh_http_request_duration_seconds_bucket", "route",
                 f'sum by (route) (rate(vitalmesh_http_requests_total[{w}]))')
table(f"HTTP latency by route, server side, over the last {w}", "route", http, ["per second", "p50 (ms)", "p95 (ms)", "p99 (ms)"])
ops = quantiles("vitalmesh_operation_duration_seconds_bucket", "operation",
                f'sum by (operation) (rate(vitalmesh_operation_total[{w}]))')
table("Application operations (handler-side, after decoding and auth)", "operation", ops, ["per second", "p50 (ms)", "p95 (ms)", "p99 (ms)"])
db = quantiles("vitalmesh_database_query_duration_seconds_bucket", "statement",
               f'sum by (statement) (rate(vitalmesh_database_query_duration_seconds_count[{w}]))')
table("Database statements from the gateway (per statement, including round trip)", "statement", db, ["per second", "p50 (ms)", "p95 (ms)", "p99 (ms)"])
jobs = quantiles("vitalmesh_processor_job_duration_seconds_bucket", "outcome",
                 f'sum by (outcome) (rate(vitalmesh_processor_jobs_total[{w}]))')
table("Processor job time (the processor's own histogram)", "outcome", jobs, ["per second", "p50 (ms)", "p95 (ms)", "p99 (ms)"])

print("**Resources over the window**\n")
print("| Metric | Value |")
print("|---|---|")
def one(expr, fmt):
    r = q(expr)
    return fmt(float(r[0]["value"][1])) if r else "-"
print(f"| gateway CPU (cores, average over the window) | {one(f'rate(process_cpu_seconds_total{{job=\"api-gateway\"}}[{w}])', lambda v: f'{v:.2f}')} |")
print(f"| gateway resident memory, max | {one(f'max_over_time(process_resident_memory_bytes{{job=\"api-gateway\"}}[{w}])', lambda v: f'{v/1048576:.0f} MiB')} |")
print(f"| gateway Go heap in use, max | {one(f'max_over_time(go_memstats_heap_inuse_bytes{{job=\"api-gateway\"}}[{w}])', lambda v: f'{v/1048576:.0f} MiB')} |")
print(f"| gateway goroutines, max | {one(f'max_over_time(go_goroutines{{job=\"api-gateway\"}}[{w}])', lambda v: f'{v:.0f}')} |")
print(f"| gateway GC cycles per second | {one(f'rate(go_gc_duration_seconds_count{{job=\"api-gateway\"}}[{w}])', lambda v: f'{v:.2f}')} |")
print(f"| gateway rate-limit decisions per second, refused | {one(f'sum(rate(vitalmesh_rate_limit_decisions_total{{decision!=\"allowed\"}}[{w}]))', lambda v: f'{v:.2f}')} |")
print(f"| processor active jobs, max | {one(f'max_over_time(vitalmesh_processor_active_jobs[{w}])', lambda v: f'{v:.0f}')} |")
print()
EOF
    note "written"
fi
fi

# ------------------------------------------------------------------ 5
step "5. Batch ingestion by batch size (one client, sequential)"
[ -n "$PY" ] || die "python3 is needed from here on"
pref="perf-$STAMP"
p=$(api POST /api/v1/patients "$TOKEN" "$(withbody "{\"external_reference\":\"$pref-batch\",\"date_of_birth\":\"1980-01-01\",\"sex\":\"UNKNOWN\"}")")
PATIENT=$(json "$(body "$p")" id)
[ -n "$PATIENT" ] || die "could not create the batch patient: $p"
gen() { # gen <patient> <base epoch> <first index> <count> <step seconds> > file
    "$PY" - "$@" <<'EOF'
import sys, json, datetime
pid, base, first, n, step = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), float(sys.argv[5])
items = []
for i in range(first, first + n):
    t = datetime.datetime.fromtimestamp(base + i * step, datetime.timezone.utc)
    items.append({"patient_id": pid, "type": "HEART_RATE", "value": 60 + (i % 30), "unit": "bpm",
                  "recorded_at": t.strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z", "source": "perf"})
print(json.dumps({"items": items}))
EOF
}
out "## 5. Batch ingestion by batch size"
out ""
out "One client, sequential POST /api/v1/measurements/batch, five requests per size, unique readings, from the host (curl time_total). readings/s is the batch size over the median request time; it is what one client gets, not the system's capacity."
out ""
out '| Batch size | min (ms) | median (ms) | max (ms) | readings/s at the median |'
out '|---|---|---|---|---|'
base=$(( $(date -u +%s) - 30 * 3600 ))
idx=0
for n in 100 500 1000; do
    times=""
    k=0
    while [ "$k" -lt 5 ]; do
        gen "$PATIENT" "$base" "$idx" "$n" 0.01 > "$RESULTS/.batch.json"
        r=$(api POST /api/v1/measurements/batch "$TOKEN" "$RESULTS/.batch.json")
        printf '%s' "$(body "$r")" | grep -q '"items"' || die "batch of $n refused: $(body "$r" | cut -c1-200)"
        times="$times $(took "$r")"
        idx=$((idx + n)); k=$((k + 1))
    done
    printf '%s\n' "$times" | tr ' ' '\n' | grep . | sort -n | awk -v n="$n" '{ a[NR]=$1*1000 } END { med=a[int((NR+1)/2)]; printf "| %d | %.1f | %.1f | %.1f | %.0f |\n", n, a[1], med, a[NR], n/(med/1000) }' >> "$OUT"
done
rm -f "$RESULTS/.batch.json"
out ""
note "done"

# ------------------------------------------------------------------ 6
step "6. Processing by job size (one client, sequential)"
p=$(api POST /api/v1/patients "$TOKEN" "$(withbody "{\"external_reference\":\"$pref-jobs\",\"date_of_birth\":\"1980-01-01\",\"sex\":\"UNKNOWN\"}")")
JOBP=$(json "$(body "$p")" id)
[ -n "$JOBP" ] || die "could not create the jobs patient: $p"
jbase=$(( $(date -u +%s) - 20 * 3600 ))
note "storing 60,000 readings one second apart (60 batches of 1000)"
k=0
while [ "$k" -lt 60 ]; do
    gen "$JOBP" "$jbase" $((k * 1000)) 1000 1 > "$RESULTS/.batch.json"
    r=$(api POST /api/v1/measurements/batch "$TOKEN" "$RESULTS/.batch.json")
    printf '%s' "$(body "$r")" | grep -q '"items"' || die "seeding batch $k refused: $(body "$r" | cut -c1-200)"
    k=$((k + 1))
done
rm -f "$RESULTS/.batch.json"
out "## 6. Processing by job size"
out ""
out "One client, sequential POST /api/v1/processing/jobs over one patient holding 60,000 HEART_RATE readings one second apart; windows 1m and 5m, percentiles 50 and 95; each size three times. End to end is curl time_total; processor time is the job's completed_at minus started_at, the Rust engine's work as the gateway recorded it. readings/s is the job size over the processor time."
out ""
out '| Readings in the job | end to end median (ms) | processor median (ms) | processor min (ms) | readings/s (processor, median) | result rows |'
out '|---|---|---|---|---|---|'
iso() { "$PY" -c "import datetime,sys; print(datetime.datetime.fromtimestamp(int(sys.argv[1]), datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$1"; }
for n in 60 600 6000 60000; do
    from=$(iso "$jbase"); to=$(iso $((jbase + n)))
    e2e=""; proc=""; rows="-"
    k=0
    while [ "$k" -lt 3 ]; do
        r=$(api POST /api/v1/processing/jobs "$TOKEN" "$(withbody "{\"patient_id\":\"$JOBP\",\"measurement_types\":[\"HEART_RATE\"],\"windows\":[\"1m\",\"5m\"],\"percentiles\":[50,95],\"from\":\"$from\",\"to\":\"$to\"}")")
        b=$(body "$r")
        [ "$(json "$b" status)" = "COMPLETED" ] || die "job over $n readings did not complete: $(printf '%s' "$b" | cut -c1-200)"
        ms=$("$PY" -c "import sys,json,datetime; j=json.loads(sys.argv[1]); f=lambda s: datetime.datetime.fromisoformat(s.replace('Z','+00:00')); print((f(j['completed_at'])-f(j['started_at'])).total_seconds()*1000)" "$b")
        e2e="$e2e $(took "$r")"; proc="$proc $ms"
        jid=$(json "$b" id)
        rows=$(psql -Atc "SELECT count(*) FROM processing_results WHERE job_id = '$jid'")
        k=$((k + 1))
    done
    e2emed=$(printf '%s\n' "$e2e" | tr ' ' '\n' | grep . | sort -n | awk '{a[NR]=$1*1000} END {print a[int((NR+1)/2)]}')
    procs=$(printf '%s\n' "$proc" | tr ' ' '\n' | grep . | sort -n)
    procmed=$(printf '%s\n' "$procs" | awk '{a[NR]=$1} END {print a[int((NR+1)/2)]}')
    procmin=$(printf '%s\n' "$procs" | head -1)
    rate=$(awk -v n="$n" -v m="$procmed" 'BEGIN { if (m > 0) printf "%.0f", n / (m / 1000); else print "-" }')
    out "| $n | $(printf '%.1f' "$e2emed") | $(printf '%.1f' "$procmed") | $(printf '%.1f' "$procmin") | $rate | $rows |"
    note "$n readings: end to end $(printf '%.0f' "$e2emed") ms, processor $(printf '%.0f' "$procmed") ms"
done
out ""

# ------------------------------------------------------------------ 7
step "7. Database: sizes, scan statistics and the plans of the hot queries"
out "## 7. Database"
out ""
out "On the data the sections above left behind. Sizes and PostgreSQL's own scan counters first, then EXPLAIN (ANALYZE, BUFFERS) of the queries the gateway runs, with the parameters of the 60,000-reading patient."
out ""
out '```'
psql -c "SELECT relname AS table, n_live_tup AS rows, pg_size_pretty(pg_total_relation_size(relid)) AS total_size, seq_scan, idx_scan, n_tup_ins AS inserted FROM pg_stat_user_tables WHERE relname IN ('measurements','audit_logs','processing_jobs','processing_results','patients','users','idempotency_keys') ORDER BY pg_total_relation_size(relid) DESC" >> "$OUT"
psql -c "SELECT indexrelname AS index, pg_size_pretty(pg_relation_size(indexrelid)) AS size, idx_scan FROM pg_stat_user_indexes WHERE relname IN ('measurements','audit_logs','processing_results') ORDER BY pg_relation_size(indexrelid) DESC" >> "$OUT"
psql -c "SELECT (SELECT count(*) FROM measurements) AS measurements, (SELECT count(*) FROM audit_logs WHERE action = 'MEASUREMENT_CREATED') AS measurement_audit_rows" >> "$OUT"
out '```'
out ""
out "Listing a patient's readings of one type in a time range, first page (the query behind GET /patients/{id}/measurements):"
out ""
out '```'
psql -c "EXPLAIN (ANALYZE, BUFFERS) SELECT id, patient_id, type, value, unit, recorded_at, created_at, source, metadata FROM measurements WHERE patient_id = '$JOBP' AND type = 'HEART_RATE' AND recorded_at >= '$(iso "$jbase")' AND recorded_at < '$(iso $((jbase + 60000)))' ORDER BY recorded_at, id LIMIT 50" >> "$OUT"
out '```'
out ""
out "The same listing deep into the data (a keyset cursor in the middle of the range):"
out ""
out '```'
psql -c "EXPLAIN (ANALYZE, BUFFERS) SELECT id, patient_id, type, value, unit, recorded_at, created_at, source, metadata FROM measurements WHERE patient_id = '$JOBP' AND (recorded_at, id) > ('$(iso $((jbase + 30000)))', '00000000-0000-0000-0000-000000000000') ORDER BY recorded_at, id LIMIT 50" >> "$OUT"
out '```'
out ""
out "What a job reads: every reading of the patient in the range, in recording order (the query behind processing; the processor adds the identifier tiebreak itself):"
out ""
out '```'
psql -c "EXPLAIN (ANALYZE, BUFFERS) SELECT id, patient_id, type, value, unit, recorded_at, created_at, source, metadata FROM measurements WHERE patient_id = '$JOBP' AND type = 'HEART_RATE' AND recorded_at >= '$(iso "$jbase")' AND recorded_at < '$(iso $((jbase + 60000)))' ORDER BY recorded_at LIMIT 100001" >> "$OUT"
out '```'
out ""
out "A patient's results, first page (GET /patients/{id}/processing-results):"
out ""
out '```'
psql -c "EXPLAIN (ANALYZE, BUFFERS) SELECT id, job_id, patient_id, measurement_type, \"window\", window_start, statistics, anomalies, algorithm_version, service_version, created_at FROM processing_results WHERE patient_id = '$JOBP' ORDER BY measurement_type, \"window\", window_start, job_id LIMIT 50" >> "$OUT"
out '```'
out ""
out "The audit trail of one resource (what an operator would query):"
out ""
out '```'
psql -c "EXPLAIN (ANALYZE, BUFFERS) SELECT id, actor_id, action, request_id, created_at FROM audit_logs WHERE resource_id = '$JOBP' ORDER BY created_at DESC LIMIT 50" >> "$OUT"
out '```'
out ""

rm -f "$BODY" "$RESULTS/.batch.json"
step "Done"
note "$OUT"
