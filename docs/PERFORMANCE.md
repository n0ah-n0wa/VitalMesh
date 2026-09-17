# Performance

The entry point for VitalMesh's performance work: how it is measured, what
was measured, what was changed on the strength of the measurements, and
what the final review found. The detail lives in three documents:

- [LOAD_TESTING.md](LOAD_TESTING.md): the k6 workload, its assumptions,
  and how to run and read it.
- [PERFORMANCE_BASELINE.md](PERFORMANCE_BASELINE.md): the first
  measurement, section by section, with the bottlenecks it identified.
- [PERFORMANCE_OPTIMIZATIONS.md](PERFORMANCE_OPTIMIZATIONS.md): each change
  with its before and after, and what was rejected.

Every number in these pages is a measurement of one run on one machine, a
laptop under Docker Desktop with eight virtual CPUs and 3.8 GiB for
containers, and describes that machine as much as the code. Nothing here
is a capacity claim or a promise about a deployment.

## Methodology

Three instruments, overlapping so that time can be attributed by
subtraction rather than guessed:

| Instrument | What it measures | How to run it |
|---|---|---|
| k6 in a pinned container (`grafana/k6:1.4.0`) on the compose network | client-side latency and throughput of a fixed mixed workload: single and batch ingestion, processing jobs with results, concurrent readers, a rate-limit probe | `make load-test` (`LOAD_PROFILE=smoke` or `standard`) |
| the baseline script | sign-in cost; the mixed load with container CPU and memory sampled; the server-side view from Prometheus (HTTP, operation and database-statement histograms, the processor's job histogram, CPU, memory, GC); batch ingestion by batch size; processing by job size; database sizes, scan counters and `EXPLAIN (ANALYZE, BUFFERS)` of the hot queries | `RESET=1 make perf-baseline`; `SKIP_LOAD=1` for the single-client sections only |
| benchmarks | the Rust engine in process (Criterion: statistics, windowing, rules, pipeline at 1,000 to 100,000 readings); the gateway's job read and results write against the real database and its request encoding (Go benchmarks) | `cargo bench` in `services/processor`; `go test -tags integration -bench` in `services/api-gateway` |

Rules that every measurement here follows: the database starts empty
(`RESET=1`), the load generator runs on the same host and is said so, the
environment block is recorded with every report, medians are read as the
cost of an operation and tails as a property of the machine on the day,
and two runs are quoted where one would not do.

## Measured results, after optimization

The final state of the code, from an empty database, on the machine above.

**Mixed load** (k6 standard profile, 120 s, client side): about 7,860
requests at 60 req/s, 77,000 readings at 580 to 610 readings/s, 241 jobs,
zero errors, the rate-limit probe refused a third of its requests as
designed.

| Operation | p50 (ms) | p95 (ms) |
|---|---|---|
| POST /measurements (one reading) | 6.1 | 10.9 |
| POST /measurements/batch (100 readings) | 23.1 | 50.2 |
| POST /processing/jobs (60 readings, end to end) | 17.7 | 30.5 |
| GET /patients/{id} | 1.6 | 2.7 |
| GET /patients/{id}/measurements | 2.2 | 3.9 |
| GET …/processing-results | 2.0 | 3.6 |

**Single client, sequential:** sign-in 0.19 to 0.33 s (argon2id at 64 MiB,
three passes); batch ingestion 28 ms for 100 readings, 95 for 500, 183 to
212 for 1,000 (4,700 to 5,500 readings/s at the median); processing 19 to
33 ms end to end for 60 readings, 64 to 73 ms for 6,000, 470 to 560 ms for
60,000, of which the processor itself takes 4 to 7 ms, 19 to 25 ms and
140 to 250 ms.

**The engine in process** (Criterion): descriptive statistics at 13
million values a second; the full pipeline at 1.2 million readings a
second over one window and 385,000 a second over every window; rule
evaluation the largest single cost inside it (4× the analysis without
rules).

**Against the baseline:** a 1,000-reading batch went from 494 to about 200
ms (2.3 to 2.5×), a 60,000-reading job from 1,742 to about 500 ms (3.1 to
3.7×), the gateway's share of that job from about 1,340 ms to about 300
ms; everything the changes did not touch measures the same.

## Benchmark reproducibility

Two consecutive runs of the single-client sections from an empty
database, minutes apart, and the Criterion suite run twice with the engine
unchanged:

| Measurement | run 1 | run 2 |
|---|---|---|
| batch of 100 readings, median (ms) | 27.7 | 31.3 |
| batch of 500 | 91.4 | 89.2 |
| batch of 1,000 | 170.5 | 173.8 |
| job over 60 readings, end to end / processor (ms) | 32.8 / 5.2 | 37.4 / 4.9 |
| job over 6,000 | 71.8 / 20.9 | 65.5 / 18.3 |
| job over 60,000 | 457 / 154 | 430 / 143 |
| sign-in, median (s) | 0.20 | 0.19 |

Medians agree within about 10% on every row. Criterion's large inputs
repeat within 3% (statistics over 100,000 values −3.0%, the full pipeline
over 100,000 readings −2.3%, analysis with rules +2.5%); its
sub-millisecond cases (1,000 readings) vary by 20 to 30% between runs, and
that is the machine, not the code, which did not change between them. So:
quote large-input benchmarks and single-client medians as repeatable to
about 10% on this machine, and treat anything under a millisecond, or any
tail, as a range.

## Memory behaviour under sustained load

The k6 standard profile for 300 s (18,516 requests, 189,121 readings,
591 jobs, zero errors), with the gateway's resident set, Go heap and
goroutines read from Prometheus every 30 s and the other containers from
`docker stats` every 10 s:

| Time into the run | gateway RSS (MiB) | Go heap in use (MiB) | goroutines |
|---|---|---|---|
| 0 s (accounts being created) | 33 | 14 | 13 |
| 120 s (k6 signing in 27 accounts) | 163 | 68 | 19 |
| 150 s (load under way) | 31 | 8.5 | 46 |
| 210 s | 33 | 9.5 | 51 |
| 300 s | 34 | 9.2 | 80 |
| 360 s | 34 | 9.6 | 71 |
| 420 s (end) | 35 | 9.1 | 71 |

The gateway holds a flat 31 to 35 MiB resident and 8 to 10 MiB of heap
for the whole run; the one spike is the 27 concurrent sign-ins of k6's
setup, argon2's 64 MiB per hash bounded by `PASSWORD_HASH_MAX_CONCURRENT`,
gone by the next sample. Goroutines settle at the number of open client
connections. The processor stays at 47 to 49 MiB and Redis at 12.5 to
12.8 MiB from first to last sample. PostgreSQL grows from 89 to 214 MiB as
its buffer cache fills with the 189,000 readings the run stores, which is
the database caching its data up to its configured limits, not a leak.
No unbounded growth was observed in any process.

## The review

Each item checked against the code and, where it could be, against a
measurement.

**Bounded memory.** Every structure that could grow with traffic has a
bound: request bodies at 1 MiB (`http.MaxBytesReader`), batches at 1,000
readings, a job at 100,000 readings and the processor's body limit
derived from it, the processor's response at 64 MiB in the gateway's
client, metadata at 2 KiB per reading, the rate limiter's local fallback
at 100,000 keys, the processor's finished-job records at 1,024 or fifteen
minutes whichever comes first, idempotency records at a 24-hour TTL with
an hourly sweep, the patient cache at a 30-second TTL in Redis. The
gateway's heap runs at 8 to 10 MiB under the mixed load and idles near
that; its peaks (68 to 130 MiB across the runs) are the bursts of
concurrent sign-ins, argon2's working memory, and pass within seconds;
the soak above is the measurement.

**Bounded concurrency.** The processor admits at most `MAX_CONCURRENT_JOBS`
(4) jobs at once through a semaphore and answers `PROCESSOR_BUSY` beyond
it; the gateway hashes at most `PASSWORD_HASH_MAX_CONCURRENT` (4)
passwords at once; the database pool holds at most 10 connections and the
Redis pool 10; the processor client retries at most 3 times inside the
request's own deadline; k6's executors cap their VUs. Nothing spawns per
request without a limit.

**PostgreSQL access.** Every query the gateway runs is an index scan
(section 7 of the baseline, re-checked after the changes): the reading
listing and its keyset pages, the job's read (now in the index's own
order, no sort), results by patient and by job, the audit trail by
resource, idempotency by its unique key, users by email and id. Batches
are single statements. The one sequential scan, on `patients`, is the
planner's correct choice for a 58-row table. The row-level validation
trigger became a per-statement one, and no new index was added.

**Batch sizes.** The API accepts up to 1,000 readings a batch; the
measured cost is about 0.2 ms a reading at 500 and 1,000, and the
per-request overhead about 25 ms, so 100 to 1,000 is the useful range and
the load test's 100 and the loader's 500 are inside it. A job's results
(about 1,200 rows for 60,000 readings over every window) are one
statement.

**Timeouts.** Requests are bounded at 10 s (`HTTP_REQUEST_TIMEOUT`), each
processor attempt at 5 s with up to 3 attempts, database connects at 5 s,
readiness checks at 2 s, Redis commands at 250 ms, shutdown at 10 s; the
processor bounds a request at 30 s and a job at 300 s. One nuance stays as
documented in [FAILURE_MODES.md](FAILURE_MODES.md): three attempts at 5 s
exceed the 10 s request bound, so a hung processor is reported as the
request's timeout rather than the more specific processor timeout; the
outcome (a bounded call and a `FAILED` job) is the same.

**No N+1 queries.** A batch checks each distinct patient it references
once, not each reading; a job reads each requested type once (the types
are bounded by the seven-entry catalogue), then one statement for the
results; listings are one query each; authentication is one indexed user
lookup per request, and the patient cache absorbs 99% of patient reads
under load. The measured per-request query rates (about 37 selects and
23 inserts a second at 60 requests a second in the baseline) are
consistent with one to three statements per request.

**Rust complexity.** Time ordering is one `O(n log n)` sort per job;
windowing, threshold and z-score rules and the summation statistics are
`O(n)`; percentiles sort each window once, `O(n log n)`; the
rolling-deviation rule is `O(n × window)` by design and measured at the
configured window (30) to be a small share, so its `O(n)` form was
rejected on the numbers and on the fixtures' exactness (see the
optimizations page). The engine's cost is linear in readings across the
1,000 to 100,000 range Criterion measures.

**Realistic load-test scenarios.** The mix follows the specification's
workload (ingestion-heavy, batches, jobs over recent readings, readers on
hot patients, a rate-limited client) with fixed rates under the
per-account limit. What it does not do is stated in
[LOAD_TESTING.md](LOAD_TESTING.md): no think time, no TLS or ingress, no
network distance, one replica, no search for a saturation point.

## Reproducing

```bash
make up
make load-test                             # the mixed load and its report
RESET=1 make perf-baseline                 # the whole baseline, about six minutes
SKIP_LOAD=1 RESET=1 make perf-baseline     # the single-client sections, about 90 s
cd services/processor && cargo bench --locked
cd services/api-gateway && go test -tags integration -run xxx -bench 'Job60k|Batch1200' -benchmem ./internal/infra/postgres/
```

Reports and raw summaries land in `tests/load/results/`, git-ignored;
keep a report with its environment block when quoting it.
