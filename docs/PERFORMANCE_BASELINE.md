# Performance baseline

The first measurement of where VitalMesh spends its time, taken before
any optimisation, so that later changes have something to be compared
with (SPECIFICATIONS.md sections 50 and 51). It records the method, the
environment, the numbers, and what the numbers point at. It changes no
code: the point of a baseline is to be the state before.

What was changed on the strength of it, and what each change measured before and after, is in [PERFORMANCE_OPTIMIZATIONS.md](PERFORMANCE_OPTIMIZATIONS.md).

The one sentence to keep, from [LOAD_TESTING.md](LOAD_TESTING.md): these
are measurements of one run on one machine, a laptop under Docker
Desktop, and they describe that machine as much as the code.

```bash
make up
RESET=1 make perf-baseline        # from an empty database; about six minutes
```

`scripts/perf-baseline.sh` produces `tests/load/results/baseline-<stamp>.md`
(git-ignored) with every table below, next to the k6 report and the
`docker stats` samples it took. The Rust benchmarks are run separately
with `cargo bench` in `services/processor`, because they need the Rust
toolchain and a quiet machine, not the running stack.

## Method

Seven measurements, in a fixed order, each with its own instrument. They
are chosen to answer one question each, and to overlap: the same request
is seen by the client (k6, curl), by the gateway's handler histogram, and
by its database histogram, so that where the time goes can be read off by
subtraction rather than guessed.

| # | What | Instrument | Question |
|---|---|---|---|
| 1 | Environment | `docker info`, `/health`, `docker stats` at rest | what ran, on what, and the idle footprint |
| 2 | Sign-in cost | five sequential logins, curl `time_total` | what argon2 costs at the configured parameters |
| 3 | Mixed load | the k6 standard profile for 120 s (LOAD_TESTING.md), `docker stats` sampled every 2 s | client-side latency per operation under a fixed mixed load; CPU and memory of each container while it runs |
| 4 | Server-side view | Prometheus over the same window: `histogram_quantile` of the HTTP, operation and database-statement histograms, the processor's job histogram, `process_cpu_seconds_total`, memory and GC | where inside the gateway the client's latency is spent, and what the services cost |
| 5 | Batch ingestion | one client, batches of 100, 500 and 1000 readings, five each, curl `time_total` | how ingestion cost scales with batch size, and the readings/s one client sees |
| 6 | Processing by size | one patient with 60,000 readings; jobs over 60, 600, 6,000 and 60,000 readings, three each; end to end from curl, processor time from the job's `started_at` and `completed_at` | how the Rust engine and the whole path scale with job size |
| 7 | Database | `pg_stat_user_tables`, `pg_stat_user_indexes`, `EXPLAIN (ANALYZE, BUFFERS)` of the listing, the job's read, the results listing and the audit lookup | whether the hot queries use their indexes, how big things are, and how many rows a reading costs |
| 8 | Rust engine alone | `cargo bench` (Criterion) in `services/processor`: the statistics kernels and the pipeline at several sizes | the engine's cost per reading with no HTTP, database or gateway in the way |

Definitions:

- **Latency** at the client is from first byte sent to last byte received,
  on the same host as the services. At the server, the HTTP histogram
  covers the handler from routing to the last byte written; the operation
  histogram covers the application service after decoding and
  authorisation; the database histogram covers one statement including
  its round trip.
- **Processing latency** is the job's `completed_at` minus `started_at`,
  which the gateway records around its call to the processor: it includes
  the HTTP hop between the two services and the processor's work, not the
  gateway's own validation, the read of the readings or the write of the
  results.
- **Throughput** is completed requests over the run's length; for
  ingestion, readings stored per second.
- **CPU** from `docker stats` is a share of one core (200% is two cores);
  from Prometheus it is `rate(process_cpu_seconds_total)`, in cores.
- **Memory** is the container's resident set (`docker stats`) and, for
  the gateway, the Go heap in use.

What is deliberately not done: no warm-up phase (cold start is part of
what a fresh environment does, and the k6 report shows it in the tails),
no search for a saturation point (the rates are fixed and under the
per-account limit; see LOAD_TESTING.md), no tuning of PostgreSQL, the
pool, GOMAXPROCS or the processor's concurrency (this is the state
before).

## Environment

Filled in by the script for each run; the numbers below come from the run
of the date given in the results.

- Windows 11 laptop, Docker Desktop (WSL 2 backend) with 8 virtual CPUs
  and 3.8 GiB for containers, Docker 26.1.4, compose 2.27.
- Every service at `docker-compose.yml` defaults: no CPU or memory limits;
  one replica each; PostgreSQL 16 and Redis 7 as containers with their
  data on the Docker volume; the gateway's pool, timeouts and rate limits
  at their defaults; the processor with `MAX_CONCURRENT_JOBS=4`.
- The load generator (k6), curl and `docker stats` run on the same host
  and compete with the services for the same CPUs.
- Prometheus, Grafana and the collector run alongside and are part of the
  idle footprint.
- An idle kind cluster (`vitalmesh-control-plane`, from the Kubernetes
  tests) was stopped before the run; it costs about a fifth of a core when
  running and would otherwise be in the tails.

## Results

Run of 2026-09-16, 23:12 UTC, commit `4140694` with a modified working
tree, from an empty database (`RESET=1`), load for 120 s. The full report
is `tests/load/results/baseline-20260916T231202Z.md` on the machine that
produced it; what follows is its content, condensed.

### 1. Environment and idle footprint

Windows 11, Docker Desktop 26.1.4 (8 CPUs, 3.8 GiB), compose 2.27, k6
1.4.0, both services at version `dev` from the working tree. At rest,
`docker stats`: gateway about 70 MiB, PostgreSQL 31 MiB, processor 4 MiB,
Redis 8 MiB, all near 0% CPU.

### 2. Sign-in cost

Five sequential logins from the host, argon2id `m=65536,t=3,p=1` (the
defaults): 0.32, 0.23, 0.23, 0.28, 0.21 s. Under the mixed load the
server-side histogram for `POST /auth/login` (the k6 setup's 27 logins
and the load accounts) reads p50 196 ms, p95 775 ms, p99 955 ms.

### 3. Mixed load, client side (k6, 120 s)

7,862 requests at 61.7 req/s; 76,961 readings at 604 readings/s; 241
jobs at 1.9 jobs/s; error rate 0.000%; every check passed; no dropped
iterations; 153 of the rate-limit probe's 451 requests refused with 429.

| Operation | Requests | req/s | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| POST /measurements (one reading) | 2401 | 18.9 | 6.0 | 10.1 | 15.2 | 140.1 |
| POST /measurements/batch (100 readings) | 601 | 4.7 | 42.6 | 61.8 | 93.3 | 185.0 |
| POST /processing/jobs (60 readings, end to end) | 241 | 1.9 | 17.3 | 30.0 | 44.3 | 163.2 |
| GET …/processing-results (after a job) | 241 | 1.9 | 2.6 | 4.3 | 6.0 | 10.9 |
| GET /patients/{id} | 1201 | 9.4 | 1.6 | 2.7 | 4.1 | 11.1 |
| GET /patients/{id}/measurements | 1201 | 9.4 | 2.6 | 4.4 | 5.3 | 43.3 |
| GET …/processing-results (reader) | 1201 | 9.4 | 2.1 | 3.7 | 5.9 | 18.3 |
| GET /patients/{id} (rate-limit probe) | 451 | 3.5 | 1.3 | 2.6 | 3.4 | 9.3 |

Milliseconds. Processing latency of the 60-reading jobs (the job's own
timestamps): p50 4.0, p95 7.0, p99 11.0, max 19.0 ms.

CPU and memory during the run (`docker stats` every 2 s, 49 samples):

| Container | CPU average | CPU max | Memory max |
|---|---|---|---|
| api-gateway | 14% | 100% | see below |
| postgres | 17% | 68% | 100 MiB |
| processor | 0% | 4% | 3 MiB |
| redis | 2% | 4% | 8 MiB |

`docker stats` on Docker Desktop reports the gateway's memory
inconsistently (samples of 137 MiB before the load and 22 MiB during it
for a process whose own resident set never fell); the gateway's memory is
taken from Prometheus instead: resident set max 164 MiB, Go heap in use
max 127 MiB, 50 goroutines max, 4.7 GC cycles a second, 0.15 cores
averaged over the window.

### 4. Server side, over the same window (Prometheus)

HTTP, from routing to last byte (the smallest histogram bucket is 5 ms,
so sub-5 ms values are interpolated and read as 2.5 ms; the client table
above is the finer instrument for reads):

| Route | per s | p50 | p95 | p99 |
|---|---|---|---|---|
| POST /api/v1/measurements | 10.1 | 6.1 | 9.8 | 21.2 |
| POST /api/v1/measurements/batch | 3.5 | 38.6 | 76.7 | 96.3 |
| POST /api/v1/processing/jobs | 1.0 | 17.8 | 24.9 | 46.4 |
| POST /api/v1/auth/login | 0.1 | 196.4 | 775.0 | 955.0 |
| GET /api/v1/patients/{id} | 6.7 | 2.5 | 4.8 | 5.0 |
| GET /api/v1/patients/{id}/measurements | 5.0 | 2.5 | 4.8 | 5.0 |
| GET /api/v1/patients/{id}/processing-results | 5.9 | 2.5 | 4.8 | 5.5 |

Application operations (after decoding and authorisation):
`measurement.create` p50 3.5 ms, `measurement.batch` 38.4 ms,
`processing.create` 17.7 ms, `patient.get`, `measurement.list` and
`processing.results` at the 2.5 ms floor.

Database statements from the gateway, one histogram per statement kind,
round trip included:

| Statement | per s | p50 | p95 | p99 |
|---|---|---|---|---|
| begin | 17.2 | 0.5 | 1.0 | 1.0 |
| insert (one pipelined batch counts once) | 23.2 | 0.6 | 1.8 | 3.2 |
| select | 38.7 | 0.5 | 1.2 | 2.3 |
| update | 3.0 | 0.8 | 4.6 | 5.0 |
| commit | 17.2 | 3.0 | 7.3 | 15.3 |

Processor job histogram: p50 5.0 ms, p95 9.5 ms, p99 9.9 ms, active
jobs never above one at a time. Patient cache: 1,480 hits, 18 misses.

### 5. Batch ingestion by batch size (one client, sequential)

| Batch size | min | median | max | readings/s at the median |
|---|---|---|---|---|
| 100 | 52.6 | 58.2 | 88.3 | 1,718 |
| 500 | 171.4 | 174.4 | 189.0 | 2,867 |
| 1000 | 317.4 | 341.7 | 385.3 | 2,926 |

Milliseconds. Between 500 and 1000 readings the cost is 0.33 ms per
reading; the fixed part of a batch request is about 25 ms.

### 6. Processing by job size (one client, sequential, three each)

| Readings | end to end median | processor median | processor min | readings/s (processor) | result rows |
|---|---|---|---|---|---|
| 60 | 23.3 | 4.9 | 4.4 | 12,163 | 3 |
| 600 | 28.1 | 6.8 | 6.3 | 88,757 | 14 |
| 6,000 | 120.7 | 25.9 | 21.4 | 231,392 | 122 |
| 60,000 | 1,175.9 | 470.7 | 209.0 | 127,458 | 1,202 |

Milliseconds. The 60,000-reading jobs varied most between their three
runs (processor 209 to 471 ms; the run before this one measured 202 ms
median), which is the host, not the input: the input is identical.

### 7. Database, on the data the run left

| Table | rows | size | seq scans | index scans |
|---|---|---|---|---|
| measurements | 144,961 | 47 MB | 3 | 1,454 |
| audit_logs | 145,337 | 46 MB | 2 | 0 |
| processing_results | 4,509 | 2.7 MB | 3 | 1,453 |
| processing_jobs | 255 | 272 kB | 3 | 7,370 |
| patients | 58 | 64 kB | 148,396 | 7,819 |

Every measurement has exactly one `MEASUREMENT_CREATED` audit row
(144,961 of each). The largest index is the uniqueness key on
`(patient, type, recorded_at, source)`, 18 MB, which no query reads: it
exists to refuse duplicates. Plans, all with warm buffers:

- Listing a patient's readings (first page, and a keyset page deep into
  the data): index scan on `(patient_id, recorded_at)` plus an incremental
  sort for the `id` tiebreak, 15 buffers, 0.27 ms.
- What a job reads (all 60,000 readings in range, ordered): bitmap index
  scan, then a sort that spills to disk (`external merge, Disk: 5880kB`)
  because the default `work_mem` of 4 MB is smaller than the row set,
  71 ms.
- A patient's results, first page: index scan, 0.15 ms. The audit trail
  of one resource: backward index scan, 0.14 ms.

### 8. The Rust engine alone (Criterion, `cargo bench`, release build)

| Benchmark | Input | Time | Throughput |
|---|---|---|---|
| statistics (full descriptive statistics of one window) | 100,000 values | 7.7 ms | 13.0 M values/s |
| aggregate (windowing) | 100,000 readings | 47.5 ms | 2.1 M/s |
| rolling deviation, window 5 / 60 / 600 | 10,000 readings | 0.95 / 4.1 / 34.7 ms | 10.5 M / 2.5 M / 288 k per s |
| stages: parse timestamps | 100,000 | 6.0 ms | 16.7 M/s |
| stages: analyse, no rules | 100,000 | 53.7 ms | 1.86 M/s |
| stages: analyse, with rules (threshold, z-score, rolling 30) | 100,000 | 218.5 ms | 458 k/s |
| stages: serialise the outcome | 100,000 | 74.0 ms | 1.35 M/s |
| pipeline, one 1h window | 1,000 / 10,000 / 100,000 | 0.81 / 7.1 / 79.9 ms | 1.2 to 1.4 M readings/s |
| pipeline, every window | 1,000 / 10,000 / 100,000 | 3.2 / 25.9 / 260.3 ms | 308 to 386 k readings/s |

Criterion's mean of each measurement; the ranges it reports are within
about 10% except where the machine was noisy (the 1,000-reading cases).

## Where the time goes, from the measurements

Each statement below is read off the tables above; where two instruments
disagree, both are cited.

1. **A single reading costs a commit.** One `POST /measurements` is
   6 ms at the client and 3.5 ms in the handler, and the database
   histogram puts the commit alone at p50 3.0 ms, p99 15 ms, against
   0.5 ms for the insert itself. A reading is a transaction of four
   statements (begin, insert the reading, insert its audit row, commit),
   and the commit's fsync is most of it. This is the floor for
   single-reading ingestion on this disk, and it is why batching is
   worth 15× per reading (section 5).

2. **Batch ingestion is bounded by per-row statements and the audit
   trail, not by the request.** Between 500 and 1000 readings a batch
   costs 0.33 ms per reading, on top of about 25 ms per request. Each
   reading is two pipelined `INSERT … RETURNING` statements (the reading,
   its audit row), so a 1000-reading batch is 2,000 statements in one
   round trip and one commit. The audit table holds exactly one row per
   reading and is as large as the readings table (46 MB against 47 MB):
   every reading is written twice and stored twice. That is the design's
   price for an append-only trail per reading, and it is the largest
   single cost in ingestion; the measurement does not say it should
   change, it says what it costs.

3. **Sign-in is the most expensive request, by thirty times.** argon2id
   at 64 MiB and three passes costs about 200 ms of CPU per login here
   (0.21 to 0.32 s from the host, p50 196 ms server side, p99 955 ms
   under load), and `PASSWORD_HASH_MAX_CONCURRENT=4` bounds how many run
   at once. Tokens live 15 minutes, so a client that signs in once and
   keeps its token never notices; a client that signs in per request
   would be limited to a few logins a second per core, before rate
   limiting. The cost is the security parameter working as intended;
   it is recorded here so that a future "logins are slow" is not a
   surprise.

4. **The Rust engine is not where a job's time goes.** For a 60-reading
   job the processor's share is 5 of 17 ms end to end; for 60,000
   readings it is 209 to 471 of 1,176 ms. In process (Criterion) the
   pipeline handles 1.2 M readings a second on one window and 385 k a
   second on every window; over HTTP it handled 127 k to 297 k a second.
   The rest of a large job is the gateway: reading 60,000 rows with a
   sort that spills to disk (71 ms, section 7), encoding them to JSON for
   the processor and decoding the outcome, writing 1,202 result rows as
   individual statements, and the garbage those allocations make (heap
   peaks of 127 MiB and 4.7 GC cycles a second during the load, for a
   process idling at 20 MiB). For jobs of a few thousand readings and
   more, the gateway's serialisation and result writes are the
   bottleneck, and the processor is a fifth of the time.

5. **Inside the engine, the rules are the cost.** Analysing 100,000
   readings takes 54 ms with no rules and 219 ms with the three rule
   kinds configured; the rolling-deviation rule scales with its window
   (10 M readings a second at window 5, 288 k at window 600). The
   statistics kernel itself runs at 13 M values a second and the time
   stamp parse at 17 M. Anyone tuning the engine should start at the
   rule evaluation, and nothing else in it is worth touching first.

6. **Reads are cheap and cached.** Patient, listing and results reads
   are 1.6 to 2.6 ms at the client, at the histogram's floor at the
   server, all index scans of under 0.3 ms with warm buffers, and the
   patient cache hit 1,480 times for 18 misses. The 148,396 sequential
   scans on `patients` are the existence check every write makes,
   planned as a scan because the table has 58 rows; at 0.5 ms a select it
   is not a cost now, and the planner will switch to the index when the
   table justifies it.

7. **Nothing saturates at the measured rates, by design.** Over the
   load window the gateway averaged 0.15 cores and PostgreSQL 0.17, the
   processor rounded to zero, and every container stayed well under
   200 MiB. Peaks of 100% (one core) on the gateway and 68% on PostgreSQL
   are single samples. The rates are set under the per-account rate limit
   (LOAD_TESTING.md), which refuses traffic long before any of this is
   busy; the baseline measures cost per operation, not capacity.

8. **Two measurement artefacts worth knowing.** On a Windows host,
   `localhost` resolves to `::1` first and the fallback costs about
   200 ms per connection (0.21 s connect against 0.8 ms to `127.0.0.1`);
   a first version of this baseline attributed it to the gateway, and
   every client-side number here is over `127.0.0.1`. And `docker stats`
   on Docker Desktop reports the gateway's memory in a way its own
   resident set does not support; Prometheus is the instrument for it.

No correctness issue was found: zero errors and every check held across
the runs, the job read's disk sort is a cost and not a fault, and no
change was made to any service.

## What this baseline does not say

- Nothing about capacity: no measurement here pushed anything to its
  limit, by design. The per-account rate limit would refuse traffic before
  the server did.
- Nothing about a deployment: no TLS, no ingress, no network between the
  load generator and the gateway, no managed database, one replica.
- Nothing about long runs: two minutes of load against a database that
  starts empty. Retention, vacuum, index bloat and cache eviction are
  hours-and-gigabytes questions.
- Tails on this machine are dominated by the host (Docker Desktop's
  virtual machine and whatever else the laptop does); medians are the
  robust figure.

## Reproducing

```bash
make up
RESET=1 make perf-baseline                       # the report, stats and k6 output in tests/load/results/
cd services/processor && cargo bench --locked    # the engine alone; needs the Rust toolchain
```

Run it twice and compare medians; keep the report with its environment
block. To compare a change, run the baseline on the same machine before
and after, from an empty database both times (`RESET=1`), and read
sections 4 to 7 side by side.
