# Resilience: retries, timeouts, leases

Every place the system retries, waits or gives up, classified the way
SPECIFICATIONS.md sections 37 and 93 require, with what bounds it and what
proves it. This is the record of the distributed resilience review; the
outages themselves are observed in [FAILURE_MODES.md](FAILURE_MODES.md).

The rule (section 93): retry temporary network errors, temporary downstream
availability failures and transient infrastructure failures; never retry
validation, authentication, authorization, invalid data or contract
violations; use bounded exponential backoff; never create uncontrolled
duplicate writes (section 37).

## Every retry in the system

| Caller → callee | What is retried | Bound | Backoff | Per-attempt timeout | Duplicate safety |
|---|---|---|---|---|---|
| Gateway → processor, `POST /internal/v1/process` | classified retryable failures only (table below) | `PROCESSOR_MAX_ATTEMPTS` (3, hard ceiling 5), and never past the request's own deadline | exponential from `PROCESSOR_BACKOFF` (100 ms), doubling, capped at `PROCESSOR_MAX_BACKOFF` (2 s), with equal jitter (half kept, half random); a `Retry-After` from the processor is honoured as given, capped | `PROCESSOR_TIMEOUT` (5 s), derived from the request context so the caller's deadline always wins; the remaining budget is sent as `X-Request-Timeout-Ms` so the processor stops when the gateway does | the request carries the job id the gateway created; the processor refuses a duplicate still running (409) and keeps no result of a finished one; results are written once, with the COMPLETED transition, in one transaction |
| Gateway → processor, `GET /internal/v1/health` | nothing | 1 | — | `PROCESSOR_TIMEOUT` | read-only |
| Gateway → Redis (cache, rate limit, idempotency lock) | nothing in the client (`MaxRetries = -1`) | 1 | — | `REDIS_TIMEOUT` (250 ms) per command, `REDIS_DIAL_TIMEOUT` (2 s) | every use degrades instead: cache miss, per-replica rate limit, lock skipped (PostgreSQL still serialises the idempotency claim) |
| Gateway → PostgreSQL | nothing automatically; `TRANSACTION_CONFLICT` (serialization failure, deadlock) and `DATABASE_UNAVAILABLE` are returned as retryable 503s for the client to repeat | — | — | every statement runs under the request's deadline (`HTTP_REQUEST_TIMEOUT`); `DATABASE_CONNECT_TIMEOUT` (5 s) bounds a dial; pool acquisition waits only as long as the caller's context | a 5xx leaves no idempotency record, so a client repeat under the same `Idempotency-Key` runs once more and no more |
| Gateway, OTLP span export | the SDK's default bounded retry | ~1 minute of elapsed retry | SDK exponential | `OTEL_EXPORTER_OTLP_TIMEOUT` (10 s) | off the request path; spans are dropped, never requests |
| Processor | nothing outbound; it calls no one but its OTLP exporter (`OTEL_EXPORTER_OTLP_TIMEOUT`, 10 s) | — | — | — | — |
| Client → gateway (documented in [API.md](API.md)) | 429 and `409 IDEMPOTENCY_IN_PROGRESS` after `Retry-After`; 5xx with the same `Idempotency-Key` | the client's | the client's | `HTTP_REQUEST_TIMEOUT` (10 s) | `Idempotency-Key` on every write: a replay returns the stored response, a concurrent replay is refused, a 5xx leaves no record |
| Kubernetes | the migration Job (`backoffLimit` 6, `activeDeadlineSeconds` 900); probes (`failureThreshold` 3, startup 30) | as listed | Kubernetes' own | as listed | migrations are versioned and idempotent |

Startup retries nothing: the gateway connects to PostgreSQL lazily and
starts with it down (readiness reports the outage), and never calls the
processor's health endpoint to decide whether to start.

## Classification of the processor call

The gateway decides whether a failure is worth repeating from the status
and the error code, as the contract classifies them
(`contracts/internal-api/processor-v1.json`, `x-retryable` and the body's
`retryable`). A test reads the contract and checks every declared code
against the gateway's table, so the two services cannot drift apart on
this (`internal/infra/processorclient/client_contract_test.go`); the
processor's own contract test proves it emits those codes with those
classifications.

| Answer | Section 93 category | Retried | Gateway code |
|---|---|---|---|
| connection refused, reset, DNS failure, EOF | temporary network error | yes | `PROCESSOR_UNAVAILABLE` |
| the attempt's own deadline | temporary downstream (slow) | yes | `PROCESSING_TIMEOUT` |
| `503 PROCESSOR_OVERLOADED` (with `Retry-After`) | temporary downstream availability | yes | `PROCESSOR_UNAVAILABLE` |
| `503 PROCESSOR_SHUTTING_DOWN` | temporary downstream availability | yes | `PROCESSOR_UNAVAILABLE` |
| `504 PROCESSING_TIMEOUT` | temporary downstream (slow) | yes | `PROCESSING_TIMEOUT` |
| `500 INTERNAL_ERROR` | transient infrastructure (as the processor itself classifies it) | yes | `PROCESSOR_PROTOCOL_ERROR` |
| `409 JOB_ALREADY_RUNNING` | temporary downstream: this gateway's own earlier attempt is still running and ends on its own | yes | `PROCESSOR_BUSY` |
| `503 PROCESSING_CANCELLED` | not a failure to repeat: the work was stopped on request | no | `PROCESSOR_UNAVAILABLE` |
| `422 NO_VALID_MEASUREMENTS`, `422 JOB_TOO_LARGE`, `422 UNSUPPORTED_ALGORITHM_VERSION` | validation / invalid data | no | `PROCESSING_NO_MEASUREMENTS`, `PROCESSING_JOB_TOO_LARGE`, `PROCESSING_REJECTED` |
| `401 UNAUTHENTICATED` | authentication | no | `PROCESSOR_PROTOCOL_ERROR` |
| `400`, `413`, `415`, and any status the contract does not name (`429`, `502`, …) | contract violation | no | `PROCESSOR_PROTOCOL_ERROR` |
| an unreadable or mismatched 200 body | contract violation | no | `PROCESSOR_PROTOCOL_ERROR` |
| the caller's context cancelled | the caller gave up | no | `PROCESSING_TIMEOUT` |

Authorization does not arise between the two services: the processor has
one internal credential and one role. A `502` is deliberately not retried:
nothing in the deployed path emits it (the gateway addresses the
processor's ClusterIP Service directly, with no proxy between them), so one
would mean something is in the path that the architecture does not have,
and it is reported loudly as a protocol error rather than absorbed.

The processor's admission control is the other half of this. It admits at
most `MAX_CONCURRENT_JOBS` (4) jobs and never queues: a job over capacity
is refused at once with `503` and `Retry-After: 1`, so a busy processor
costs the gateway a millisecond, not a slot. The work is cancelled
cooperatively at the smaller of `PROCESSING_TIMEOUT` and the gateway's
remaining budget, checked at every pipeline stage and every few windows, so
a timed-out attempt stops computing rather than finishing for nobody; the
computation runs on a blocking thread, off the async runtime that answers
health probes.

## Timeouts, and the order they must keep

| Bound | Default | Where |
|---|---|---|
| `HTTP_READ_HEADER_TIMEOUT` / `HTTP_READ_TIMEOUT` / `HTTP_WRITE_TIMEOUT` / `HTTP_IDLE_TIMEOUT` | 5 s / 10 s / 15 s / 60 s | gateway server |
| `HTTP_REQUEST_TIMEOUT` | 10 s | gateway, every handler; every database statement, Redis command and processor attempt inherits it |
| `HTTP_SHUTDOWN_TIMEOUT` | 10 s | gateway drain on SIGTERM |
| `PROCESSOR_TIMEOUT` × `PROCESSOR_MAX_ATTEMPTS` | 5 s × 3 | one attempt; the budget, which the request deadline cuts short |
| `PROCESSING_FAILURE_RECORD_TIMEOUT` | 5 s | the FAILED write, on its own deadline because the request's has usually passed |
| `PROCESSING_JOB_LEASE` / `PROCESSING_LEASE_SWEEP_INTERVAL` | 15 m / 1 m | how long a dispatch may hold a job; how often expired leases are failed |
| `DATABASE_CONNECT_TIMEOUT` | 5 s | one dial |
| `REDIS_DIAL_TIMEOUT` / `REDIS_TIMEOUT` / `REDIS_RECOVERY_INTERVAL` | 2 s / 250 ms / 5 s | one dial; one command; how long Redis is left alone after a failure |
| `READINESS_TIMEOUT` | 2 s | the whole readiness evaluation |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | 10 s | one span export, both services |
| processor `HTTP_REQUEST_TIMEOUT` / `PROCESSING_TIMEOUT` / `SHUTDOWN_TIMEOUT` | 30 s / 300 s / 30 s | one request; one job (the gateway's budget makes it smaller); drain |

Configuration validation refuses an order that would turn a clear failure
into a mystery: `PROCESSOR_TIMEOUT` < `HTTP_REQUEST_TIMEOUT` <
`HTTP_WRITE_TIMEOUT`; `REDIS_TIMEOUT` < `HTTP_REQUEST_TIMEOUT`;
`HTTP_SHUTDOWN_TIMEOUT` ≥ `HTTP_REQUEST_TIMEOUT` (or every request in
flight at a deployment is cut off); `PROCESSOR_MAX_BACKOFF` ≥
`PROCESSOR_BACKOFF`; 1 ≤ `PROCESSOR_MAX_ATTEMPTS` ≤ 5;
`PROCESSING_JOB_LEASE` ≥ 2 × (`HTTP_REQUEST_TIMEOUT` +
`PROCESSING_FAILURE_RECORD_TIMEOUT`), so the sweep can never fail a job
that is still legitimately being worked on. The one bound that crosses the
two services, the gateway's attempt against the processor's request
timeout, is kept by the budget header rather than by validation: the
processor's effective bound is always the smaller of the two.

The retry budget (15 s at the defaults) is deliberately larger than the
request timeout (10 s). The client sees the request's bound, never the
budget's, because the client checks the deadline before every sleep and
never sleeps into it; in practice a frozen processor gets two attempts and
the caller gets `504` at 10 s, as the `bounded retries` stack case
measures.

## Duplicate safety

Retries must never create uncontrolled duplicate writes (section 37). The
chain, from the outside in:

1. **Client → gateway.** Every write takes an `Idempotency-Key`. The claim
   is a unique insert in PostgreSQL (`ON CONFLICT DO NOTHING`), so two
   identical requests serialise there whether or not Redis is up; Redis
   only adds a fast in-progress refusal. A completed request replays its
   stored response; a different body under the same key is refused; a
   5xx or a panic deletes the claim so a legitimate retry can run.
2. **Gateway → processor.** A job exists (PENDING) before anything is
   dispatched and is PROCESSING, with its attempt counted, before the call.
   Every attempt carries the same job id; the processor refuses a
   duplicate still running and keeps no result of a finished one, so a
   repeat recomputes rather than duplicates. Results and the COMPLETED
   transition are one transaction, and the trigger
   `processing_jobs_enforce_transition` refuses a second completion.
3. **Gateway → database.** No transaction is open across the processor
   call. Serialization failures and deadlocks are not retried inside the
   gateway; they surface as `503 TRANSACTION_CONFLICT`, which the client
   repeats under its idempotency key.

## Circuit protection, where justified

Section 37 asks for circuit breaking *or equivalent protection where
justified*. Each dependency was assessed on what a failure costs and what
already bounds it.

**Redis has a breaker.** After a failure the client refuses every call
for `REDIS_RECOVERY_INTERVAL` (5 s) without touching the network, then
tries once; a caller's own cancellation is never counted against Redis.
An outage therefore costs one 250 ms timeout per replica per 5 s, and
everything Redis backs degrades rather than stalls.

**The processor has equivalent protection, and a client-side breaker is
not justified.** What a failing processor costs the gateway is bounded on
every axis: at most 3 attempts inside a 10 s request, no database
connection or transaction held across the call, no readiness dependency
(so a dead processor never takes a gateway out of rotation or restarts
it), and admission control on the processor itself, which refuses excess
work in a millisecond. A processor that is *down* fails fast (connection
refused); one that is *frozen* costs a request its 10 s and nothing else,
and job creation is an operator action under rate limits, not a hot path.
A breaker would add per-replica state and a new failure mode (a tripped
breaker refusing work the processor has recovered for, until a half-open
probe) to save at most a few seconds per request during a black-hole
outage. Revisit if job creation becomes high-volume or automated, or if
anything is ever held across the call.

**PostgreSQL is protected by readiness.** An unreachable database fails
`/ready` in about a second, the pod leaves the Service, and callers stop
arriving; requests already inside fail predictably with
`503 DATABASE_UNAVAILABLE` under the request deadline. The pool bounds
connections (`DATABASE_MAX_CONNS`) and an acquire waits only as long as
the caller's context.

## Job leases: a crash cannot strand a job

Graceful shutdown drains a dispatch, but a crash, an OOM kill or a lost
node cannot, and the job it was running would stay PROCESSING for ever,
neither terminal (section 92) nor a failure record (section 94). Every
dispatch therefore holds the job under a lease, recorded in
`lease_expires_at`, and every gateway replica sweeps expired leases: a
PROCESSING job whose lease has expired is FAILED with
`PROCESSING_INTERRUPTED`, keeping its attempt count and timestamps; a
PENDING job older than a lease was created by a request that died before
dispatching it and is started and failed the way an abandoned dispatch is.
The sweep locks the rows it takes and skips rows another replica holds, so
replicas share the work, and it never touches a job whose lease is valid.

## What the review found and changed

- **A job whose results could not be stored was left PROCESSING.** If the
  processor finished but the transaction writing the results failed (a
  request deadline that had just passed, a database blip), the client got
  the store's error and the job stayed in flight for ever. It is now
  failed with `PROCESSING_RESULTS_NOT_STORED`, on the failure record's own
  deadline.
- **A job the gateway died under was left PROCESSING.** Nothing reclaimed
  expired leases; the column recorded the intent. The lease sweep above
  now does, and the `gateway killed mid-job` stack case proves it.
- **The two services disagreed on `409 JOB_ALREADY_RUNNING`.** The
  contract and the processor classified it non-retryable; the gateway
  retried it, with sound reasoning (its own earlier attempt is still
  running and ends on its own). The contract (1.1.2) and the processor now
  say retryable too, and a test holds the gateway's table to the contract
  so the next disagreement cannot land silently. The gateway's unused
  `retryable` body field was removed: the status and the code decide, and
  the contract test is what keeps that honest.
- **Backoff was exponential but not jittered.** With two or more replicas
  failing against the same processor at the same instant, every retry fell
  on the same instant too. The delay now carries equal jitter, still never
  above the exponential value, so every bound and the deadline check hold
  unchanged; a `Retry-After` the processor asks for is still honoured as
  given.
- **A shutdown grace period shorter than the request timeout was
  accepted.** It is now refused at startup.

Everything else was as section 93 requires and stays as it was. In
particular the Redis client's single attempt, the absence of automatic
transaction retries, and the decision not to retry `502` were each
re-examined and kept, for the reasons given above.

## How this is verified

- Unit: the processor client's attempt bound, backoff, cap, jitter,
  `Retry-After`, deadline check and classification of every contract
  failure; the sweeper's batching, bound and shutdown; the processing
  service's failure recording, including the results-not-stored path.
- Contract: the gateway's classification against `processor-v1.json`;
  the processor's codes, kinds and `retryable` flags against the same
  document and over a real socket.
- Integration (PostgreSQL): the lease sweep against the trigger-enforced
  state machine; pool exhaustion.
- Stack (`make stack-test`): every failure in FAILURE_MODES.md, including
  transient recovery, bounded retries with no duplicate, severed database
  connections, processor and gateway restarts, and a gateway killed
  mid-job.
