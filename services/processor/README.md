# Processor

The Rust service that performs VitalMesh's data processing (SPECIFICATIONS.md section 7). This document describes how the crate is organised, what the foundation provides, and the numerical rules of the statistical engine. The execution engine runs whatever work it is given within the configured limits; the statistical engine in `stats` supplies the computations.

## Layout

```text
src/
  main.rs             entry point: config, logging, signal handling, listener, run
  lib.rs              module map and service constants
  config.rs           typed configuration from the environment, all errors reported at once
  error.rs            classified error type (kind, stable code, client-safe message, hidden cause)
  domain/             validated models: ids, finite values, UTC timestamps, measurement types and
                      units, measurements and batches, jobs and their state machine, results,
                      anomalies and severity, algorithm versions (see domain/mod.rs)
  concurrency.rs      bounded admission limiter (semaphore, never waits)
  engine.rs           bounded, time-limited, cancellable job execution and drain
  state.rs            application state shared with handlers; readiness flag
  stats/              statistical engine: time ordering, descriptive statistics, rolling mean and
                      moving standard deviation, epoch-aligned tumbling windows, aggregation
  jobs.rs             bounded registry of the jobs this instance is running or ran
  telemetry.rs        structured JSON logging with the repository's field schema
  metrics.rs          Prometheus metrics and the exposition served at /metrics
  tracing_otel.rs     W3C trace context propagation and OpenTelemetry export
  redact.rs           removes credentials, personal facts and payloads from every log record
  requestid.rs        request and correlation id validation and generation
  transport/          axum router, middleware, health handlers, error envelope
  lifecycle.rs        serve until shutdown, then drain and cancel
tests/lifecycle.rs    real-socket start/serve/shutdown test
tests/internal_api.rs integration tests of the internal API against the real service
tests/contract.rs     conformance of this service with the internal API contract
benches/stats.rs      criterion benchmarks of the statistical engine and its aggregation steps
benches/pipeline.rs   criterion benchmarks of the pipeline end to end and of its stages
```

## Rules

- Dependencies point inward: `transport` and `lifecycle` use `engine`, `state` and `error`; `domain`, `error` and `config` depend on nothing else in the crate.
- Every failure crossing a layer is an `error::Error`. The transport maps its kind to a status and never exposes the cause.
- One `Arc<AppState>` is shared with handlers. Inside it, the engine's running-job map is behind a `std::sync::Mutex` held only for map operations; the readiness flag is an atomic.
- Handlers never spawn tasks. Work runs through `Engine::run`, which admits at most `MAX_CONCURRENT_JOBS` jobs and rejects the rest immediately.
- No `unwrap`/`expect` on production paths. A poisoned lock is recovered because the map is never left inconsistent.

## Statistical engine

`stats` implements SPECIFICATIONS.md sections 14 and 15 over the domain types: `aggregate` time-orders measurements and produces one `Statistics` (count, min, max, mean, median, sample variance, standard deviation, requested percentiles) per measurement type, window and epoch-aligned window start; `Rolling` yields a rolling mean and moving standard deviation over the last `k` samples. Every numerical rule is stated in the module documentation and tested:

| Quantity | Rule |
|---|---|
| Sums | Neumaier compensated summation in input order |
| Mean | compensated sum ÷ count, clamped to `[min, max]` |
| Variance | sample variance (`n − 1`), two-pass with compensated summation; `0` for one value; clamped at `0` |
| Standard deviation | `sqrt(variance)` |
| Median | middle value, or `lo + (hi − lo) / 2` of the two middle values |
| Percentiles | nearest rank `ceil(p/100 · n)` on the sorted sample, so always an observed value |
| Windows | tumbling, aligned to the Unix epoch in UTC (`1m`, `5m`, `15m`, `1h`, `6h`, `24h`, `7d`); windows without data produce nothing |
| Rolling | last `k` samples; the first `k − 1` points use what is available |
| Empty / single samples | empty yields no statistics; one value has min = max = mean = median = value and zero spread |
| Determinism | fixed sequential operation order, total-order sorting, no float hashing; equal inputs give bit-identical outputs |
| Memory | the input plus one sorted copy of the current window (aggregation) or `k` values (rolling); nothing grows with time |

The rolling window recomputes from its `k` values at every step (`O(k)` per point, no sorting, no allocation). That is deliberate: it is exact and free of the cancellation an incremental removal introduces; `benches/stats.rs` measures it so a faster scheme can be justified with numbers. The mean and variance of any sample, rolling or not, come from one routine (`stats::spread`), so a rolling baseline and a full description of the same values agree bit for bit. Benchmarks run with `cargo bench --bench stats`; their figures are hardware-specific and are not performance claims.

## Anomaly detection

`anomaly` implements SPECIFICATIONS.md section 16 on top of the statistical engine. It is technical anomaly detection over synthetic data: severities INFO, WARNING and CRITICAL classify how far a value departs from a rule's expectation and carry no medical meaning.

- **Rules** come from a JSON `RuleSet` (`anomaly::RuleSet::from_json`) of three kinds, each scoped to measurement types and windows (empty scope means all) and each carrying severity tiers; the highest tier a value exceeds wins, a value that exceeds none is not anomalous, and comparisons are strict as the specification writes them (a value on a bound is not anomalous):
  - `THRESHOLD`: per-tier `{"lower", "upper"}` bounds; outer tiers must contain inner ones.
  - `Z_SCORE`: per-tier positive, increasing z thresholds over the window's own statistics; needs `min_count` values (default 5) and non-zero spread, so a constant window flags nothing.
  - `ROLLING_DEVIATION`: per-tier multipliers of the rolling standard deviation over the `window_size` values *preceding* the value (the value never shapes its own baseline); needs `min_count` predecessors.
- **Results** are the domain `Anomaly`: measurement type, window, metric, the measured value, the bound it crossed in the measurement's unit (`mean ± t·std` for statistical rules), severity, `detected_at` equal to the measurement's own timestamp, and `algorithm_version`. Detection runs per requested window, so one value can be reported under several windows, each against that window's statistics.
- **Versioning.** `anomaly::ALGORITHM_VERSION` (`1.0.0`) is stamped on every anomaly. Any change that can alter which anomalies appear or their values bumps the major number; results are never compared across versions.
- **Determinism and robustness.** Evaluation order is fixed (time, then rule declaration), inputs are finite, computed bounds are checked for finiteness and an overflow is an error rather than a result. Rule configurations are validated on load: unknown fields, empty tiers, non-nested threshold tiers, non-increasing or non-positive statistical tiers and out-of-range sample counts are rejected with the rule's name.
- **Tests.** Unit tests per rule kind and `tests/anomaly_fixtures.rs`, a golden harness over `tests/fixtures/anomaly/*.json` whose expected anomalies were computed by hand from samples chosen to have exact standard deviations; the harness also asserts order independence and the presence of every required field.

## Processing pipeline

`pipeline` composes the stages of SPECIFICATIONS.md section 14 into one deterministic core and one bounded executor:

| Stage | Where | Behaviour |
|---|---|---|
| Raw measurements | `pipeline::RawReading` | plain strings and numbers, so a malformed reading is reported on its own |
| Validation | `pipeline::stages` | identifier, type, unit, finite value, range, RFC 3339 timestamp, and timestamp bounds relative to the job's `requested_at` (so validation is reproducible); each rejection carries the input index and a stable `RejectionCode` |
| Normalization | `pipeline::stages` | UTC and `-0.0` normalisation happen on construction; duplicate identifiers are rejected in every occurrence; readings of unrequested types are skipped and counted |
| Time ordering, window aggregation, statistics, anomaly detection | `stats`, `anomaly::run_with` | as documented above, with a checkpoint after every window |
| Result generation | `domain::ProcessingResult` | one validated result per type, window and window start, stamped with the job id, `anomaly::ALGORITHM_VERSION` and the service version |

- **Bounds.** `MAX_JOB_MEASUREMENTS` refuses oversized jobs before any parsing (`JOB_TOO_LARGE`); memory is proportional to that bound. A job must request the engine's algorithm version (`UNSUPPORTED_ALGORITHM_VERSION` otherwise); a job with no processable reading fails as a whole (`NO_VALID_MEASUREMENTS`).
- **Execution.** `Processor::process` runs the core under `Engine::run`: at most `MAX_CONCURRENT_JOBS` jobs, each as exactly one task on tokio's blocking pool, bounded by `PROCESSING_TIMEOUT`, cancellable per job and on shutdown. The core receives a `Control` and checks it at every stage boundary and every 64 windows, and the control is cancelled whenever the engine abandons the job, so a timed-out or cancelled job releases its thread at the next checkpoint instead of running on in the background.
- **Errors.** Every failure is an `error::Error` with a kind (`Validation`, `Overloaded`, `Timeout`, `Cancelled`, `Unavailable`, `Internal`) and a stable code; numerical or consistency failures inside the engine become `Internal` with the cause attached, never exposed.
- **Tests and benchmarks.** `tests/pipeline.rs` covers complete flows: results with statistics, anomalies and versions, partial failures by index, whole-job failures, determinism across input orders, cooperative stop between stages, admission limits, prompt cancellation, the time bound and shutdown. `benches/pipeline.rs` measures the core end to end over 1,000 to 100,000 readings with all windows and with one, and its `stages` group isolates parsing, analysis with and without rules, and serialisation (see [Performance characteristics](#performance-characteristics)).

## Performance characteristics

Measured with the criterion benches on one development machine (`cargo bench --bench pipeline` and `cargo bench --bench stats`), on a job of 100,000 readings of two types at 10-second intervals, two percentiles, all seven windows and three rules (threshold, z-score, rolling deviation over 30 values). The figures describe the shape of the cost, not a target; they move with hardware, and the `stages` and `aggregate_steps` groups exist so that a regression can be attributed to one step.

| Step | Cost at 100,000 readings | Complexity |
|---|---|---|
| Timestamp parsing (`stages/parse_timestamps`) | 4.6 ms | `O(N)` |
| Validation, normalisation, result generation (`stages/pipeline_no_rules` − `stages/analyse_no_rules`) | 35 ms | `O(N)`; duplicates by hashing |
| Time ordering (`aggregate_steps/time_order_reversed`) | 4.4 ms | `O(N log N)` |
| Splitting into windows (`aggregate_steps/tumbling_all_windows`, one type) | 5.8 ms | `O(W · N)`, one timestamp constructed per window |
| Statistics per window (the rest of `stages/analyse_no_rules`) | 40 ms | `O(W · N log n)` for windows of `n` values: one sorted copy per window |
| Anomaly detection (`stages/analyse` − `stages/analyse_no_rules`) | 133 ms | threshold and z-score `O(W · N)`; rolling deviation `O(W · N · k)` |
| Whole core (`pipeline/all_windows`) | 222 ms, 450 k readings/s | `O(W · N · (log N + k))` |
| Whole core, one window (`pipeline/one_hour`) | 66 ms, 1.5 M readings/s | |
| JSON of the outcome (`stages/serialize_outcome`, 42,906 results, 15.3 MB) | 60 ms, 255 MB/s | `O(output)`; not on a hot path until a transport carries results |

Where the time goes and what was changed after measuring it:

- **Allocation per reading.** A `RawReading` owns four strings, as the wire shape requires. Validation consumes the readings, so an accepted identifier keeps its allocation (a copy is taken only for a reading about to be rejected, so that the rejection can echo it), and normalisation moves measurements instead of cloning them; duplicate detection hashes borrowed identifiers into one map and one bitmap. The remaining per-reading allocations are the identifier from deserialisation and the `Measurement` vectors; nothing is copied more than once. Together these took the validation, normalisation and result generation share from 60 ms to 35 ms.
- **Allocation per window.** One sorted copy of the window's values (needed for the median and the nearest-rank percentiles), one percentile map, one `Statistics`, and, for each result, the job id and service version strings the domain result owns. Detection allocates only when a window has anomalies. The value scratch buffer is reused across windows.
- **Rolling deviation** was the one super-linear step: it copied, sorted and allocated its baseline for every value to obtain a mean and a standard deviation that need no order statistics. It now uses `stats::spread` over its ring buffer: the same summation, clamp and Bessel correction, so the golden fixtures are unchanged, and the pipeline as a whole runs at 2.2× its previous throughput (480 ms to 222 ms at 100,000 readings; `aggregate/100000` 85 ms to 45 ms). The shared routine has a fixed cost per call that the special-cased originals did not: `rolling/5` is about 12% slower per point (70 ns against 63 ns) while `rolling/60` and `rolling/600` are 20% faster, and `statistics/*` is 2–5% slower because it no longer reads the extremes off the sorted copy. That is accepted for one code path.
- **Windowing** compared a freshly constructed `window_start` timestamp for every item and every window. It now constructs one per window and compares instants, with a test that the grouping is identical for every window, across the epoch and at the unrepresentable floor of year 0000 (`stages/analyse_no_rules`: 90 ms to 53 ms).
- **Iterators and copies.** Windows borrow the time-ordered input (`tumbling` yields slices), rolling statistics are a lazy iterator holding `k` values, sums take iterators, and the per-type view is a vector of references built once per type. Values are copied once per window into the scratch buffer that is then sorted.
- **Tasks and synchronisation.** One `spawn_blocking` task per admitted job and none otherwise; admission is a `try_acquire` on a semaphore that never queues, and the running-job map is behind a mutex taken twice per job. Inside the core there is no shared state: the `Pipeline` is read through an `Arc`, and `Control::check` is one atomic load and one clock read, taken at every stage boundary and every 64 windows (under 0.1% of the run). After the engine abandons a job, its blocking thread stops at the next checkpoint, so the overshoot is bounded by the work between two checkpoints: the validation stage, or 64 windows.
- **Memory.** Everything is proportional to `MAX_JOB_MEASUREMENTS`: the readings, one `Measurement` per accepted reading (moved, not duplicated), a bitmap and a hash map for duplicate detection, one reference per measurement of the current type, the scratch and sorted copies of the current window, and the results (at most `types × windows × N`). The rolling baseline holds `k` values. Nothing grows with the number of windows or with time.
- **Serialisation** runs at serde_json's usual rate for this shape; each timestamp is formatted into a fresh string, which is a small share of the total and left alone until a transport makes the outcome a hot path.
- **Not changed, on purpose.** The sorted copy per window stays: the median and percentiles need order statistics, and a selection algorithm would save little for the small windows that dominate. The rolling recomputation stays `O(k)`: an incremental update would change rounding and therefore the algorithm version. Statistics use `f64` sequential operations only; there is no SIMD or parallel reduction because the sum order defines the result.

## Configuration

Values come from the environment; empty values count as unset. Start-up exits 2 listing every invalid value.

| Variable | Default | Purpose |
|---|---|---|
| `ENVIRONMENT` | `local` | `local`, `test`, `staging`, `production` |
| `HTTP_ADDR` | `0.0.0.0:8081` | listen address |
| `HTTP_REQUEST_TIMEOUT` | `30s` | bound on handling one request; exceeding it returns 504 |
| `HTTP_MAX_BODY_BYTES` | `16777216` | largest accepted request body; must hold `MAX_JOB_MEASUREMENTS` readings, which start-up checks |
| `INTERNAL_TOKEN` | none | shared secret the gateway presents as `Authorization: Bearer`; at least 16 characters, required in staging and production, never logged |
| `LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `MAX_CONCURRENT_JOBS` | `4` | jobs admitted at once; further jobs are rejected with 503 and `Retry-After` |
| `MAX_BATCH_SIZE` | `1000` | measurements read per page (consumed by the engine once it reads data) |
| `MAX_JOB_MEASUREMENTS` | `100000` | readings one job may carry; larger jobs are refused before parsing |
| `MAX_MEASUREMENT_AGE` | `9600h` (400 days) | oldest `recorded_at` accepted, relative to the job's request time |
| `MAX_FUTURE_SKEW` | `5m` | furthest `recorded_at` ahead of the job's request time |
| `PROCESSING_TIMEOUT` | `5m` | bound on one job |
| `JOB_RETENTION` | `15m` | how long a finished job stays observable through the jobs endpoint |
| `ANOMALY_RULES` | empty | the JSON rule set every job is evaluated against; an empty set flags nothing |
| `SHUTDOWN_TIMEOUT` | `30s` | how long running jobs may finish after shutdown starts before being cancelled |

Durations are an integer with a unit: `250ms`, `30s`, `5m`, `1h`.

## Endpoints

The three `/internal/v1` routes are the internal API contract
([`contracts/internal-api/processor-v1.json`](../../contracts/internal-api/processor-v1.json), SPECIFICATIONS.md section 8), which is the authority on their request and response shapes. `/health` and `/ready` are the unversioned platform probes and are deliberately outside the contract: they serve Kubernetes, not the gateway.

| Route | Purpose |
|---|---|
| `GET /health` | liveness; never consults dependencies |
| `GET /ready` | `200 ready` once the listener is up, `503 not_ready` before that and from the moment shutdown begins |
| `GET /internal/v1/health` | versions and the limits in force, so the gateway can check compatibility and size its requests and timeouts; no credential needed |
| `POST /internal/v1/process` | runs one job's measurements through the pipeline and returns its results |
| `GET /internal/v1/jobs/{job_id}` | this instance's view of a job |

- **Authentication.** The two working endpoints require `Authorization: Bearer <INTERNAL_TOKEN>`, compared in constant time. The token authenticates the gateway to the processor inside the cluster; it is not a user credential and carries no identity, because the gateway has already authenticated and authorised the caller. It is required in staging and production, where start-up fails without it, and optional for a developer, where its absence is logged and the layer is not installed. A rejection says only that authentication is required: distinguishing absent from wrong helps nobody but an attacker.
- **Strict parsing.** The body must be `application/json` and must match the request type exactly; unknown fields, wrong types and every domain rule are rejected. Anything the request could not be understood as is `400 INVALID_REQUEST`; only a well-formed request that breaks a rule is `422`. Serde's message quotes the offending input, so it stays in the log and never reaches the client.
- **Partial failures.** A reading the pipeline rejects is reported in `rejected` by its input index with a stable code, and the rest of the job is processed. Only a job in which nothing survives fails as a whole.
- **Timeouts.** `X-Request-Timeout-Ms` lets a client declare what is left of its own deadline; the job is bounded by the smaller of that and `PROCESSING_TIMEOUT`, so work nobody is waiting for stops at the next checkpoint. A client's socket deadline must exceed the value it sends, or it will abandon the call before the `504` arrives. An unusable value is rejected rather than ignored.
- **Concurrency.** At most `MAX_CONCURRENT_JOBS` run at once and nothing queues: a request arriving at capacity is refused immediately with `503 PROCESSOR_OVERLOADED` and `Retry-After`. A second request for a job id already running is refused with `409`, which never disturbs the attempt in flight.
- **Jobs are observed, not owned.** `GET /internal/v1/jobs/{job_id}` answers from a bounded in-memory registry: running jobs, plus jobs that finished within `JOB_RETENTION` and at most the newest 1024 of them. A `404 JOB_NOT_FOUND` means this instance is not running the job and no longer remembers it, never that the job does not exist; the gateway's row is the system of record. Of the five statuses in the job state machine only `PROCESSING`, `COMPLETED`, `FAILED` and `CANCELLED` appear here, because `PENDING` exists before dispatch.

Every failure uses one envelope, on every route:

```json
{"error":{"code":"NOT_FOUND","message":"The requested resource does not exist.",
          "request_id":"...","correlation_id":"...","retryable":false}}
```

`code` is stable and is what a client branches on; `retryable` states whether repeating the identical request could succeed (SPECIFICATIONS.md section 93), and a failure that has something specific to add carries it in `details`, such as the limit and the count for `JOB_TOO_LARGE`. Error kinds map to statuses: invalid 400, unauthenticated 401, not found 404, method not allowed 405, conflict 409, unsupported media 415, validation 422, overloaded/cancelled/unavailable 503 (overloaded adds `Retry-After: 1`), timeout 504, internal 500. A cancellation is a 503 that is *not* retryable: the work stopped because someone asked it to.

## Observability

Every request logs one structured record with the request id, correlation id, method, path, status and duration. A job adds a `process` span carrying its id, algorithm version and reading count, and one record on completion with the status, duration, and the counts of accepted, skipped and rejected readings, results and anomalies. Nothing from a payload is logged: not a value, not an identifier of a measurement, and never the internal token. Prometheus metrics and OpenTelemetry export arrive with the observability phase; the log fields above are the ones those exporters will read.

## Metrics

`GET /metrics` serves the Prometheus exposition, unversioned and needing no credential, like the probes: it serves the platform's scraper. Everything is prefixed `vitalmesh_processor_`.

| Metric | Type | Labels |
|---|---|---|
| `http_requests_total` | counter | method, route, status |
| `http_errors_total` | counter | method, route, class |
| `http_request_duration_seconds` | histogram | method, route |
| `jobs_total` | counter | outcome (completed, failed, cancelled, refused) |
| `job_duration_seconds` | histogram | outcome |
| `active_jobs`, `job_capacity`, `job_slots_available` | gauge | none |

The route label is the pattern axum matched, never the request path, so asking about a thousand jobs is one series and no job id reaches the exposition. The method label keeps only routable methods and folds anything else into `OTHER`, because the method is the one label a client controls and HTTP permits any token as one. Job outcomes are published at zero from start-up, so an alert on failures works before the first failure. The saturation gauges read the engine at scrape time rather than being copied on a timer, so they cannot drift from what admission sees.

**On queue depth.** The specification asks for it; this service has none. Admission is a semaphore that never waits, and work that cannot get a permit is refused at once, so a depth gauge would read zero for ever. Saturation answers the same question: `job_slots_available` shows how close to full the service is, and `jobs_total{outcome="refused"}` shows how often it was.

## Tracing

This service is the far end of a trace that starts in a client and passes through the gateway. The gateway injects W3C `traceparent`; this service extracts it and makes its request span a child of the gateway's, so one trace covers both. The trace id is recorded on the span, so it appears in every log line of that request and a log and a span can be joined without the log pipeline knowing about OpenTelemetry.

`OTEL_EXPORTER_OTLP_ENDPOINT` names an OTLP/HTTP collector; `OTEL_EXPORTER_OTLP_TIMEOUT` bounds one export attempt. With no endpoint the service still reads and honours incoming trace context and still logs the trace id; it simply exports nothing of its own. Export is batched onto its own task, so a collector that is slow, refused or absent costs a request nothing, and a malformed `traceparent` is ignored rather than trusted or refused.

A span carries the route pattern rather than the path, because a path carries a job id and a span leaves this process for a backend that is not the record's custodian. Readings never appear: what a job records is its id, its algorithm version and counts.

That is enforced at the exit rather than trusted at each call site. Log records are redacted by the JSON formatter, but the OpenTelemetry layer reads span fields directly and would export them untouched, so the OTLP exporter is wrapped in `RedactingExporter`, which applies the same `redact.rs` rules to every span attribute and every event before anything leaves the process. Probe requests (`/health`, `/ready`, `/metrics`) run under a span named `probe`, which the exporter filter drops: a liveness probe every few seconds per replica would bury the traces that matter, and probes stay visible in metrics and logs.

## Request identification

Clients may send `X-Request-ID` (per hop) and `X-Correlation-ID` (end to end), 1–128 characters of `[A-Za-z0-9._-]`. Invalid or missing request ids are replaced by a generated one; a missing correlation id defaults to the request id. Both are echoed in the response, included in every error envelope, and attached as top-level fields to every log record emitted while handling the request.

## Logs

One JSON object per line with `timestamp`, `level`, `service`, `version`, `environment`, `message`, any event fields, and the fields of enclosing spans (`request_id`, `correlation_id`, `method`, `path` inside a request). The access log record is `message: "request"` with `status` and `duration_ms`. This service has no trace of its own to name; the gateway propagates `X-Request-ID` and `X-Correlation-ID`, and a `trace_id` joins the schema when the observability phase adds trace propagation.

Every record passes through `redact.rs` as the formatter writes it, so the guarantee does not depend on remembering it at each call site, and span fields are covered as well as event fields. Three rules apply: a field named as a credential or a personal fact (`token`, `secret`, `password`, `authorization`, `email`, `date_of_birth`, `external_reference`, `body`, `payload`, or a name ending in `_token`, `_secret`, `_password`, `_hash`) is replaced with `[redacted]`; a JSON Web Token is removed wherever it appears in a string, recognised by the `eyJ` prefix rather than by the field it arrived in; and any string is bounded at 512 bytes so a payload logged by mistake is truncated rather than stored whole. Identifiers and counts are deliberately kept, because they are what makes a record useful during an incident: `job_id`, `request_id`, `token_id`, `readings`, `accepted`.

## Shutdown

On SIGTERM or SIGINT: readiness flips to `not_ready`, the listener stops accepting, in-flight requests finish, running jobs get up to `SHUTDOWN_TIMEOUT` to complete, remaining jobs are cancelled, and the process exits 0. Runtime failures exit 1.
