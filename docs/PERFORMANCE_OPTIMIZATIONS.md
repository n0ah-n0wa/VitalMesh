# Performance optimizations

What was changed after the [performance baseline](PERFORMANCE_BASELINE.md),
why the measurements pointed there, what each change measured before and
after, how it was verified, and what was considered and rejected because
the numbers did not justify it. Nothing here was done on intuition: every
change starts from a figure in the baseline and ends with the same figure
re-measured.

The measurements are from the same laptop as the baseline (Windows 11,
Docker Desktop, 8 virtual CPUs, 3.8 GiB for containers), with the quick
form of the baseline script (`SKIP_LOAD=1 RESET=1 make perf-baseline`:
the single-client sections from an empty database) for the sweeps, Go
benchmarks against the real database for the pieces, and the full baseline
with the mixed load for the last comparison. Run-to-run noise on this
machine is visible in every table and is the reason medians and minima are
quoted together.

## Where the baseline pointed

From [the baseline's findings](PERFORMANCE_BASELINE.md#where-the-time-goes-from-the-measurements):

- A 60,000-reading job spent 1,176 ms end to end, of which the processor
  took 200 to 470 ms; the rest was the gateway. Its read of the readings
  sorted 60,000 rows on disk (71 ms), and its results were 1,202 separate
  insert statements.
- Batch ingestion cost 0.33 ms per reading between 500 and 1,000 readings,
  as two pipelined statements per reading (the reading and its audit row),
  each running the row-level validation trigger.
- The Rust engine handled 127,000 to 297,000 readings a second over HTTP
  and 385,000 to 1.2 million in process. Reads were cheap and cached, and
  nothing saturated.

So the order of work was the gateway's database paths first, the engine
last, which is the reverse of the intuition and the point of measuring.

## 1. The job's read: the index's own order instead of a sort

**Baseline.** `EXPLAIN (ANALYZE, BUFFERS)` of the read behind processing,
60,000 readings of one type in a range: bitmap index scan, then `Sort
Method: external merge, Disk: 5880kB`, 71 ms. The sort was there because
the read shared the listing's `ORDER BY recorded_at, id`, and the index
carries no `id`.

**Change.** A dedicated read for jobs, `ListForJob`, ordered by
`recorded_at` alone, which the `(patient_id, type, recorded_at)` prefix of
the uniqueness index yields directly. The identifier tiebreak was never
needed there: the processor orders by time and identifier itself before it
computes anything (`stats::order::time_order`), so the set of readings the
job sees and the order it computes in are unchanged.

**After.** The same plan: an index scan, no sort, 27 to 33 ms. No new
index, so no write cost was added.

## 2. One statement per batch instead of one per row

**Baseline.** A 1,000-reading batch was 2,000 pipelined
`INSERT … RETURNING` statements (readings and audit rows) in one round
trip; a job's 1,202 results were 1,202 statements. One client, sequential:

| Batch size | before, median (ms) | readings/s |
|---|---|---|
| 100 | 52.3 | 1,914 |
| 500 | 198.7 | 2,516 |
| 1000 | 494.0 | 2,024 |

**Change.** `Measurements.CreateBatch`, `Audit.AppendBatch` and
`Results.CreateBatch` each send the whole batch as one
`INSERT … SELECT … FROM unnest($1::uuid[], …)` statement, with the rows as
parallel arrays, and put the returned rows back into input order by each
row's own key (unique within a batch), since `RETURNING` follows insertion
order in practice but not by contract. The API's promise to name the
reading that a batch was refused for is kept: a refused batch is rolled
back and stored again one statement per reading (`CreateBatchEach`), which
stops at the offending reading and names its index. Only a failing batch
pays for that second pass.

**After** (with change 1; the job column is the whole path):

| Batch size | after, median (ms) | readings/s | before → after |
|---|---|---|---|
| 100 | 36.5 | 2,741 | 1.4× |
| 500 | 127.1 | 3,932 | 1.6× |
| 1000 | 280.8 | 3,561 | 1.8× |

| Job (readings) | before, end to end (ms) | after (ms) |
|---|---|---|
| 60 | 28.8 | 22.9 |
| 6,000 | 94.9 | 83.9 |
| 60,000 | 1,742 | 615 |

**Verified.** The PostgreSQL integration tests for atomic batches, item
attribution (`ItemError{Index: 2}` for a duplicate in position 2, a unit
mismatch in position 1), input order of the returned rows, and results
batches all pass unchanged; a new test covers the audit batch's nullable
columns (a `SYSTEM` entry with no actor and no resource).

## 3. The validation trigger, per statement instead of per row

**Baseline.** `EXPLAIN (ANALYZE)` of a 1,000-row insert into
`measurements`, rolled back:

```
Trigger for constraint measurements_patient_id_fkey: time=28.1 calls=1000
Trigger for constraint measurements_type_fkey:       time=27.4 calls=1000
Trigger measurements_validate:                       time=34.9 calls=1000
Execution Time: 121.6 ms
```

Without the row trigger the same insert took 72 ms; the same check as one
set-based query over the 1,000 new rows took 0.66 ms.

**Change.** Migration 000011 replaces the row-level `BEFORE` trigger with
statement-level `AFTER` triggers over the transition table (one for
`INSERT`, one for `UPDATE`; PostgreSQL allows one event per transition
table trigger). The function joins the statement's rows to the catalogue
once and raises for the first offender with the same error codes and the
same constraint names as before, so the API's error mapping is untouched.
The per-reading fallback path still fires the trigger once per reading, so
attribution is untouched too. One observable difference at the database
level: a non-finite value now fails the table's own `CHECK`
(`measurements_value_finite_check`) before the trigger runs, which is the
more precise constraint; the API refuses such a value before the database
sees it.

**After.** The measurement insert of a 1,000-reading batch drops from
about 122 ms to about 72 ms at the database, and the batch sweep (with
changes 1, 2 and 4):

| Batch size | before (ms) | after (ms) | readings/s | before → after |
|---|---|---|---|---|
| 100 | 52.3 | 28.0 | 3,567 | 1.9× |
| 500 | 198.7 | 94.7 | 5,280 | 2.1× |
| 1000 | 494.0 | 211.5 | 4,727 | 2.3× |

**Verified.** The schema tests for every constraint and trigger pass; the
two cases for `NaN` and `Infinity` now expect the finite check, with the
reason in the test. Migration down restores the row-level form and the
migration tests run it.

## 4. The job's readings scanned once, into the shape that is sent

**Baseline.** A Go benchmark against the real database of the store's
read of 60,000 readings (`BenchmarkReadingsForJob60k`): 200 to 300 ms,
103 MB and 900,000 allocations, for a query that executes in 27 ms. The
profile put two thirds of the allocation in the growth of a slice of the
full domain row (nine columns including the `jsonb` metadata, scanned by
reflection) and its copy into a second slice, followed by a third pass in
the processing service converting every row into the wire struct (a
further 15 to 27 ms and 120,000 allocations, `BenchmarkConvertReadings60k`).

**Change.** `ListForJob` selects only the five columns the processor
receives and scans them by position straight into `processing.Reading`,
the wire struct; `ReadingsForJob` returns that slice (a single-type job's
page is returned as is, several types are joined once at exact size) and
the service sends it without conversion.

**After.** `BenchmarkReadingsForJob60k`: 85 to 100 ms, 44 MB, 540,000
allocations; the conversion pass is gone. The 60,000-reading job end to
end, from an empty database:

| Readings | before (ms) | after 1 and 2 (ms) | after all (ms) | before → after |
|---|---|---|---|---|
| 60 | 28.8 | 22.9 | 33.2 (min 23) | noise level |
| 600 | 30.9 | 28.4 | 28.3 | noise level |
| 6,000 | 94.9 | 83.9 | 72.6 | 1.3× |
| 60,000 | 1,742 | 615 | 557 | 3.1× |

The processor's own time on the 60,000-reading job was 327 to 398 ms
before and 156 to 248 ms after across runs; that column is the host's
noise on the day, not a change to the engine, and is why the gateway's
share is read as end to end minus processor: about 1,344 ms before,
about 310 ms after.

**Verified.** The processing service's unit tests (with the fake store
returning wire readings), the PostgreSQL integration tests, the in-process
end-to-end suite and the containerized suite all pass; the contract tests
confirm the wire struct is still the contract's.

## Considered and rejected

Each of these was measured and left alone, with the number that decided it.

- **The rolling-deviation rule in O(n) instead of O(n × window).**
  `rolling/600` takes 34.7 ms per 10,000 readings, so a sliding-window
  implementation would be a real algorithmic win at large windows. Two
  measurements argued against it now: at the window the engine is
  configured and benchmarked with (30), the rule costs about 4 ms per
  10,000 readings, under a fifth of the rule evaluation and about 2% of the
  whole-pipeline time; and the anomaly fixtures assert bit-identical
  results, which a running-sum variance does not reproduce for arbitrary
  values. A change to the numerics is an algorithm-version change, not an
  optimization, and the measured benefit at the configured window does not
  justify one.
- **The engine's outcome serialisation** (`serialize_outcome`, 74 ms per
  100,000-reading synthetic outcome) and **timestamp parsing** (6 ms):
  after changes 1 to 4 the engine's whole share of a large job is 150 to
  250 ms of 557, and the serialised outcome of a real job is about 1,200
  small rows. Nothing to gain that the measurements would show.
- **A hand-written JSON encoder for the request to the processor.**
  `BenchmarkEncodeRequest60k`: 26 ms and 8 MB for 60,000 readings with
  `encoding/json`. At most a few tens of milliseconds on the largest jobs,
  and only there; not worth a second encoder to maintain.
- **The results insert's per-row costs.** `EXPLAIN (ANALYZE)` of 1,200
  result rows: three foreign-key checks (34 to 36 ms each) and the
  `processing_results_set_patient` row trigger (40 ms), 150 µs a row in
  all, about 185 ms of a 60,000-reading job. The trigger copies the
  patient from the job so that the application can never supply the wrong
  one; replacing it with an application-supplied value verified by a
  composite foreign key trades the trigger's lookup for another
  referential check of the same cost. Left as designed.
- **Sign-in.** 0.2 to 0.3 s a login is the argon2 parameters (64 MiB,
  three passes) doing their job. Not a performance problem to fix.
- **A new index for the job read.** Considered before change 1; the
  ordering change made it unnecessary and saved the write cost every
  reading would have paid.

## Mixed load, before and after

The k6 standard profile for 120 s from an empty database, client side,
the baseline run against a run after all four changes:

| Operation, client side (ms) | baseline p50 / p95 | after, run 1 p50 / p95 | after, run 2 p50 / p95 |
|---|---|---|---|
| POST /measurements (one reading) | 6.0 / 10.1 | 7.0 / 38.1 | 6.1 / 10.9 |
| POST /measurements/batch (100 readings) | 42.6 / 61.8 | 26.8 / 153.9 | 23.1 / 50.2 |
| POST /processing/jobs (60 readings, end to end) | 17.3 / 30.0 | 19.5 / 93.7 | 17.7 / 30.5 |
| GET …/processing-results (after a job) | 2.6 / 4.3 | 3.0 / 13.6 | 2.6 / 4.6 |
| GET /patients/{id} | 1.6 / 2.7 | 1.8 / 4.6 | 1.6 / 2.7 |
| GET /patients/{id}/measurements | 2.6 / 4.4 | 3.0 / 12.9 | 2.2 / 3.9 |
| GET …/processing-results (reader) | 2.1 / 3.7 | 2.5 / 9.9 | 2.0 / 3.6 |

Throughput was the profile's fixed rates in every run (about 7,860 requests,
77,000 readings, 241 jobs, zero errors). The first run after the changes
had PostgreSQL and the gateway each peaking at a full core in `docker
stats` and every p95 inflated; the second, minutes later on the same
machine, matches the baseline on everything the changes do not touch. The
100-reading batch's median fell from 42.6 to 23.1 ms (1.8×); the gateway's
heap peak over the window fell from 127 to 113 MiB and its GC rate from 4.7
to 3.1 cycles a second.

The mixed load is dominated by single-reading writes, reads and small jobs,
none of which the changes target beyond the trigger; the batch and job
rows are where the difference is.

## Correctness

Every change was followed by the suites it touches and then by the whole
set: `make verify` from a clean checkout (format, lint with every build
tag, unit, contract, PostgreSQL and Redis integration, in-process end to
end, coverage floors, build), the containerized end-to-end suite with its
failure cases, the image build, verification and scans. No correctness
issue was found in the baseline and none was introduced: the results of
a job are the same rows, the batch error attribution is the same index,
the constraint names are the same names.

## Reproducing a comparison

```bash
make up
SKIP_LOAD=1 RESET=1 make perf-baseline     # before: sections 2, 5, 6, 7 in about 90 s
# change something, rebuild: docker compose up -d --build api-gateway
SKIP_LOAD=1 RESET=1 make perf-baseline     # after
cd services/api-gateway
go test -tags integration -run xxx -bench 'Job60k|Batch1200' -benchmem ./internal/infra/postgres/
go test -run xxx -bench 'Readings60k|EncodeRequest60k' -benchmem ./internal/processing/
```

Compare medians and minima; keep the report with its environment block.
