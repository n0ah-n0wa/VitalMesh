# Processor

The Rust service that performs VitalMesh's data processing (SPECIFICATIONS.md section 7). This document describes how the crate is organised and what the foundation provides. No processing algorithms exist yet; the engine executes whatever work it is given within the configured limits.

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
  telemetry.rs        structured JSON logging with the repository's field schema
  requestid.rs        request and correlation id validation and generation
  transport/          axum router, middleware, health handlers, error envelope
  lifecycle.rs        serve until shutdown, then drain and cancel
tests/lifecycle.rs    real-socket start/serve/shutdown test
```

## Rules

- Dependencies point inward: `transport` and `lifecycle` use `engine`, `state` and `error`; `domain`, `error` and `config` depend on nothing else in the crate.
- Every failure crossing a layer is an `error::Error`. The transport maps its kind to a status and never exposes the cause.
- One `Arc<AppState>` is shared with handlers. Inside it, the engine's running-job map is behind a `std::sync::Mutex` held only for map operations; the readiness flag is an atomic.
- Handlers never spawn tasks. Work runs through `Engine::run`, which admits at most `MAX_CONCURRENT_JOBS` jobs and rejects the rest immediately.
- No `unwrap`/`expect` on production paths. A poisoned lock is recovered because the map is never left inconsistent.

## Configuration

Values come from the environment; empty values count as unset. Start-up exits 2 listing every invalid value.

| Variable | Default | Purpose |
|---|---|---|
| `ENVIRONMENT` | `local` | `local`, `test`, `staging`, `production` |
| `HTTP_ADDR` | `0.0.0.0:8081` | listen address |
| `HTTP_REQUEST_TIMEOUT` | `30s` | bound on handling one request; exceeding it returns 504 |
| `HTTP_MAX_BODY_BYTES` | `1048576` | largest accepted request body |
| `LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `MAX_CONCURRENT_JOBS` | `4` | jobs admitted at once; further jobs are rejected with 503 and `Retry-After` |
| `MAX_BATCH_SIZE` | `1000` | measurements read per page (consumed by the engine once it reads data) |
| `PROCESSING_TIMEOUT` | `5m` | bound on one job |
| `SHUTDOWN_TIMEOUT` | `30s` | how long running jobs may finish after shutdown starts before being cancelled |

Durations are an integer with a unit: `250ms`, `30s`, `5m`, `1h`.

## Endpoints

| Route | Purpose |
|---|---|
| `GET /health` | liveness; never consults dependencies |
| `GET /ready` | `200 ready` once the listener is up, `503 not_ready` before that and from the moment shutdown begins |
| `GET /internal/v1/health` | the versioned health check the gateway calls; same body as `/health` |

Unknown routes and wrong methods return the error envelope:

```json
{"error":{"code":"NOT_FOUND","message":"The requested resource does not exist.","request_id":"..."}}
```

Error kinds map to statuses: invalid 400, validation 422, not found 404, conflict 409, overloaded/cancelled/unavailable 503 (overloaded adds `Retry-After: 1`), timeout 504, internal 500.

## Request identification

Clients may send `X-Request-ID` (per hop) and `X-Correlation-ID` (end to end), 1–128 characters of `[A-Za-z0-9._-]`. Invalid or missing request ids are replaced by a generated one; a missing correlation id defaults to the request id. Both are echoed in the response, included in every error envelope, and attached as top-level fields to every log record emitted while handling the request.

## Logs

One JSON object per line with `timestamp`, `level`, `service`, `version`, `environment`, `message`, any event fields, and the fields of enclosing spans (`request_id`, `correlation_id`, `method`, `path` inside a request). The access log record is `message: "request"` with `status` and `duration_ms`.

## Shutdown

On SIGTERM or SIGINT: readiness flips to `not_ready`, the listener stops accepting, in-flight requests finish, running jobs get up to `SHUTDOWN_TIMEOUT` to complete, remaining jobs are cancelled, and the process exits 0. Runtime failures exit 1.
