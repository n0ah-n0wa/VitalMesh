# Internal API contract

`processor-v1.json` is the contract between the Go API gateway and the Rust processor: the HTTP/JSON API the processor serves under `/internal/v1` and the gateway consumes (SPECIFICATIONS.md section 8). It is an OpenAPI 3.0.3 document and it is the authority on the wire format. Where this README and the document disagree, the document wins.

| File | What it is |
|---|---|
| `processor-v1.json` | The contract. OpenAPI 3.0.3, JSON. |
| `processor-v1.lock.json` | The reviewed fingerprint of the contract's interface surface. |

## Why JSON rather than YAML

JSON and YAML are both valid OpenAPI serialisations and every OpenAPI tool reads both. JSON is what CI can check today: the pipeline installs Go and Rust and nothing else, and both languages parse JSON with no dependency, so the gate below runs with no new tooling in either service. A YAML document would need a YAML parser added to a service that does not otherwise want one. When contract tooling arrives in a later phase (Redocly or Spectral for linting, `oasdiff` for breaking-change analysis, `oapi-codegen` for the Go client), all of it consumes this file unchanged.

## Endpoints

| Operation | Purpose |
|---|---|
| `POST /internal/v1/process` | Run one job's measurements through the pipeline and return the results. |
| `GET /internal/v1/jobs/{job_id}` | Report this processor's view of a job. |
| `GET /internal/v1/health` | Report liveness, versions and the limits in force. |

Cancellation is deliberately not in v1. The processor supports it internally, but no endpoint exposes it yet; adding `POST /internal/v1/jobs/{job_id}/cancel` later is a backward-compatible addition under the rules below.

## Ownership

The gateway owns PostgreSQL and is the system of record for patients, measurements, jobs and results (SPECIFICATIONS.md section 6.1). The processor holds no database connection (section 7). Two consequences run through the whole contract:

- **Every measurement arrives in the request body.** `POST /process` carries the readings; the processor never fetches them. This is why the body is bounded by `MAX_JOB_MEASUREMENTS` and `HTTP_MAX_BODY_BYTES` and why oversized jobs are refused before parsing (section 89).
- **`GET /jobs/{job_id}` is an observation, not a record.** It answers from a bounded in-memory registry covering running jobs and jobs that finished within `JOB_RETENTION`. A `404` means "this processor is not running it and no longer remembers it", never "it does not exist". The gateway's row is authoritative.

## Processing status

The job state machine of SPECIFICATIONS.md section 92 is owned by the gateway:

```text
PENDING → PROCESSING          PROCESSING → COMPLETED
PENDING → CANCELLED           PROCESSING → FAILED
                              PROCESSING → CANCELLED
```

The processor reports only `PROCESSING`, `COMPLETED`, `FAILED` and `CANCELLED`; `PENDING` exists before dispatch and is never observable through this API. The gateway derives the transition to persist from the response to `POST /process`:

| Result of `POST /process` | Job status to persist |
|---|---|
| `200` | `COMPLETED`, with the returned results |
| `422` | `FAILED`, with the returned code (not retryable) |
| `409` | unchanged; another attempt is already running |
| `503` `PROCESSOR_OVERLOADED` or `PROCESSOR_SHUTTING_DOWN` | stays `PENDING`, redispatch later |
| `503` `PROCESSING_CANCELLED` | `CANCELLED`, and not redispatched |
| `504` | `FAILED` after the retry budget is spent, otherwise redispatch |
| `500` | `FAILED` after the retry budget is spent |

Each failure body carries a `retryable` flag that is authoritative for that failure. The response-level `x-retryable` classifies the status code as a whole and can be broader: `503` covers both a transient refusal and a cancellation, and only the former may be redispatched.

A `FAILED` job keeps its diagnostic metadata (section 94). The processor supplies `error.code`, the safe message, its service and algorithm versions and the request id; the gateway adds the attempt count and its own timestamps.

## Timeout semantics

Four bounds apply to one dispatch, and they must be ordered so that a failure is observed rather than guessed:

1. The gateway's HTTP client deadline for the call.
2. `X-Request-Timeout-Ms`, the budget the gateway declares to the processor.
3. `PROCESSING_TIMEOUT`, the processor's own per-job bound.
4. `HTTP_REQUEST_TIMEOUT`, the processor's per-request bound.

The processor bounds the job by the smaller of (2) and (3). The gateway must set (1) strictly greater than the value it sends in (2), otherwise it abandons the call before the `504` arrives and cannot tell a timeout from a network failure. The processor advertises (3) in the health response so the gateway can size (1) and (2) against the peer it actually has.

When the bound is exceeded the processor answers `504 PROCESSING_TIMEOUT` and stops the work at its next checkpoint. No partial results are returned: a job either produces its complete outcome or none of it.

## Request and correlation ids

Both are carried in headers, both directions, and both are validated as 1 to 128 characters from `[A-Za-z0-9._-]` before being echoed or logged (SPECIFICATIONS.md section 85).

| Header | Meaning | If missing or malformed |
|---|---|---|
| `X-Request-ID` | This hop | A new id is generated |
| `X-Correlation-ID` | The client request across every service | Defaults to the request id |
| `traceparent`, `tracestate` | W3C trace context | Ignored; a new trace starts |

A malformed value is replaced, never rejected: an unusable id must not turn a valid piece of work into a failure. The id in force is echoed on the response and appears in `error.request_id`, so a failure can always be traced back.

## Versioning

The contract version is `info.version`, and the major number also appears in the path prefix.

**Backward compatible** (minor or patch bump, same prefix): adding an endpoint; adding an optional request field; adding a response field; adding a value to an enum the client only reads; relaxing a constraint; adding an error code to a response that already exists; any change to prose.

**Breaking** (new major version and a new `/internal/v2` prefix): removing or renaming an endpoint or a field; making an optional request field required; narrowing a type or a constraint; removing an enum value the server may still send; changing the meaning of an existing field; changing a status code for an existing outcome.

These rules bind clients as well as the server. A client must ignore response fields it does not know, or the compatible additions above would break it; the gateway's client does exactly that at runtime, and catches drift at build time with the contract tests instead.

Adding a value to an enum is compatible only in the direction the value travels. A new `MeasurementType` that the gateway may send is breaking for the processor until the processor implements it; a new `Severity` the processor may send requires every client to tolerate unknown values. Clients of this contract must ignore response fields they do not know and must treat an unrecognised enum value in a response as a value they cannot interpret, not as a parse failure.

`algorithm_version` is versioned separately from the contract: it describes the computation, not the wire format (SPECIFICATIONS.md section 95). A job is refused with `422 UNSUPPORTED_ALGORITHM_VERSION` unless it requests exactly the version the processor implements, and every result records both the algorithm version and the service build.

## How CI checks this

Four gates run in `make verify`, so the contract cannot drift from either side unnoticed. `make contracts-check` runs them on their own.

**1. The document is well formed** — `services/api-gateway/internal/contract`. It parses, declares exactly the three agreed operations with unique operation ids, every operation documents itself and accepts the correlation headers, every response describes itself and echoes them, every failure uses the shared error envelope and declares both the codes it can return (`x-error-codes`) and whether it is worth retrying (`x-retryable`), every `$ref` resolves, every component schema is used and documented, and the health endpoint is reachable without a credential.

**2. Every published example satisfies its own schema** — same package. Examples are how a hand-written contract usually starts lying, so each one is checked against the schema it is published under by a JSON Schema subset checker. The checker refuses any keyword it does not implement rather than skipping it, so it cannot report success over a constraint it never applied, and its own rejection behaviour is unit-tested.

**3. The server agrees with the document** — `services/processor/tests/contract.rs`. Every enumeration in the contract is compared against the wire values the Rust domain types actually serialise, the advertised limits are compared against the service's configuration defaults, the advertised algorithm version is compared against the one this build implements, every promised error code is one something in the service emits, and the request and result examples are deserialised into the real domain types, which apply every invariant on the way in.

**4. An interface change cannot land silently** — the lock file, checked in gate 1. `processor-v1.lock.json` records a digest of everything a client can observe: paths, operations, parameters, schemas, security, status codes. Prose is excluded, so rewording a description does not touch it. Any change to the shape of a request or a response does, and the test then fails with instructions.

That satisfies SPECIFICATIONS.md section 49 to the extent a digest can: it proves the interface changed and forces a reviewer to look, but it does not itself judge whether the change was compatible. That judgement is the reviewer's, against the rules above. When `oasdiff` is available in CI it replaces the lock with real breaking-change analysis.

After a reviewed change:

```sh
make contracts-lock   # re-record the fingerprint
```

## Implementation status

- **The processor serves all three endpoints.** Authentication, strict parsing, the body limit, the timeout budget, admission control, the job registry, cancellation and the error envelope are implemented and covered by `services/processor/tests/internal_api.rs`, which drives the production router over a real socket. `services/processor/tests/contract.rs` additionally asserts that every route in this document is served, that nothing outside it is, and that the version the service reports is this document's own.
- **The gateway has no client yet.** Nothing in the Go service calls these endpoints; that arrives with the vertical slice, along with the live contract test that runs a real client against a real server.
- **Not yet built:** cancellation (deliberately out of v1, see above), and Prometheus metrics for the processing set, which belong to the observability phase.

## Version history

| Version | Change |
|---|---|
| 1.1.1 | Raised the advertised `max_request_bytes` to match the default the processor now uses. A job at the advertised `max_job_measurements` weighs about 12 MB, which the previous 1 MiB default could not carry, so jobs well inside the ceiling were refused for their size. The processor now refuses to start if the two numbers contradict each other. Document only: no request or response shape changed. |
| 1.1.0 | Declared `WWW-Authenticate` on `401`, as RFC 7235 requires. Removed `VALIDATION_FAILED` from the `422` on `POST /process`: the service never emits it there, because a body it cannot understand is a `400` and every `422` has a named cause. Documented the `details` that `JOB_TOO_LARGE` carries. |
| 1.0.0 | First draft. Never served by any build, so 1.1.0 amends it rather than superseding it. |
