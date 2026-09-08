# API Gateway

The Go service that forms VitalMesh's public boundary (SPECIFICATIONS.md section 6.1). This document describes how the code is organised and the rules that keep it that way.

## Layout

```text
cmd/api-gateway/            entry point: load config, build logger, run the app
internal/
  app/                      composition root: wires packages together, runs the lifecycle
  auth/                     application service: password hashing, access tokens, login, principal
  authz/                    authorization policy (role -> permissions) and the route -> permission table
  patient/                  application service: Patient API use cases over a Store port
    patienttest/            in-memory Store for service and API tests
  measurement/              application service: Measurement API, type catalogue, strict validation, batches
    measurementtest/        in-memory Store for service and API tests
  idempotency/              Idempotency-Key rules (key shape, request fingerprint) and the Store port
    idempotencytest/        in-memory Store for middleware and API tests
  config/                   typed configuration loaded from the environment
  domain/                   vocabulary shared by every feature (error model, entities)
  health/                   application service: readiness evaluation over Checker ports
  httpapi/                  HTTP transport: router, route groups, route table, server lifecycle
    handler/                HTTP handlers (HTTP <-> application translation only)
    middleware/             request ID, security headers, logging, metrics, timeout, recovery, body limit, authentication, authorization, idempotency
    model/                  request/response wire models (error envelope, pages, health, login, user, patient, measurement)
    request/                JSON body decoding and pagination parameter parsing
    respond/                JSON and error-envelope writers, domain error -> status mapping
  pagination/               cursor primitives and page-size bounds
  validate/                 input validation accumulator producing domain validation errors
  infra/postgres/           PostgreSQL: pool, migrator, transactions, repositories, error mapping, patient store
    postgrestest/           per-test database helper for integration tests
  observability/logging/    structured logging setup (request_id and trace_id from the context)
  observability/metrics/    metrics port (Recorder) with a no-op default; bounded labels only
  observability/tracing/    tracing port (Tracer, Span) with a no-op default; trace id carriage
  requestid/                request identifier generation, validation and context carriage
  buildinfo/                version injected at link time
migrations/                 embedded SQL migrations (see docs/DATABASE.md)
```

## Layers and dependency rules

| Layer | Packages | May import |
|---|---|---|
| Transport | `httpapi` and its subpackages | application services, `domain`, `config`, `requestid` |
| Application | `health`, `auth`, `authz`, `patient`, and one package per feature as they arrive | `domain`, `config`, `validate`, `requestid`, `pagination`, `auth` (for the principal), the `observability` ports, ports it declares itself |
| Domain | `domain` | nothing inside the service |
| Infrastructure | `observability/logging`, `infra/postgres`; later `infra/redis`, `infra/processor` | the application ports it implements (and their input types), `domain`, `config` |
| Composition | `app`, `cmd/api-gateway` | everything |

- Dependencies point inward: transport -> application -> domain. Infrastructure implements application interfaces and is only referenced by `app`.
- Interfaces are declared by the package that consumes them, next to the consumer. `health.Checker` and `auth.UserStore`/`auth.Auditor` are the examples so far; the PostgreSQL repositories in `internal/infra/postgres` satisfy them (through a small adapter in `app` where the shapes differ).
- Handlers contain no business rules. They decode input, call an application service, and encode the result through `respond`.
- Every error crossing a layer boundary is a `*domain.Error` or wraps one. `respond.Error` maps the kind to a status code; anything unclassified becomes a generic 500 and is logged with its cause.
- Wire models in `httpapi/model` are the only types serialised to clients. They will be generated from the OpenAPI contract once it exists.
- Loggers are passed explicitly. Request-scoped fields come from the context: any `*Context` logging call inside a request automatically carries `request_id`.

## Configuration

All values are read from the environment. Empty values count as unset. Start-up fails with exit code 2 listing every invalid value.

| Variable | Default | Purpose |
|---|---|---|
| `ENVIRONMENT` | `local` | one of `local`, `test`, `staging`, `production` |
| `HTTP_ADDR` | `:8080` | listen address |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | |
| `HTTP_READ_TIMEOUT` | `10s` | |
| `HTTP_WRITE_TIMEOUT` | `15s` | |
| `HTTP_IDLE_TIMEOUT` | `60s` | |
| `HTTP_SHUTDOWN_TIMEOUT` | `10s` | grace period for in-flight requests on SIGTERM/SIGINT; remaining connections are closed when it elapses and the process exits 1 |
| `HTTP_REQUEST_TIMEOUT` | `10s` | handler execution bound; must be shorter than `HTTP_WRITE_TIMEOUT` |
| `HTTP_MAX_BODY_BYTES` | `1048576` | largest accepted request body |
| `DATABASE_URL` | (required) | PostgreSQL connection URL, e.g. `postgres://user:pass@host:5432/db?sslmode=disable`. In `staging` and `production` the `sslmode` must be `require`, `verify-ca` or `verify-full`; start-up refuses plaintext and opportunistic modes. |
| `DATABASE_MAX_CONNS` | `10` | connection pool size |
| `DATABASE_CONNECT_TIMEOUT` | `5s` | per-connection dial timeout |
| `JWT_SECRET` | (required) | HMAC key for access tokens; at least 32 bytes. Never commit it (SPECIFICATIONS.md section 31). |
| `JWT_KEY_ID` | `1` | `kid` written into issued tokens; 1–64 characters of `[A-Za-z0-9._-]` |
| `JWT_PREVIOUS_SECRETS` | (none) | retired keys still accepted for verification, as `kid=secret,kid=secret`; lets `JWT_SECRET` rotate without invalidating live tokens |
| `JWT_ISSUER` | `vitalmesh` | `iss` claim written and required |
| `JWT_TTL` | `15m` | access token lifetime; at most `24h` |
| `JWT_CLOCK_SKEW` | `30s` | tolerance on `iat`/`exp`; at most `5m` |
| `PASSWORD_HASH_MEMORY_KIB` | `65536` | Argon2id memory cost for new hashes (8192–1048576) |
| `PASSWORD_HASH_TIME` | `3` | Argon2id iterations (1–100) |
| `PASSWORD_HASH_PARALLELISM` | `1` | Argon2id lanes (1–64) |
| `PASSWORD_HASH_MAX_CONCURRENT` | `4` | hash computations allowed at once (1–1024); bounds login memory to this × `PASSWORD_HASH_MEMORY_KIB`. Requests beyond it wait until their request timeout. |
| `MEASUREMENT_MAX_BATCH_SIZE` | `1000` | readings per batch request (1–10000) |
| `MEASUREMENT_MAX_METADATA_BYTES` | `2048` | reading metadata as compact JSON (1–4096; the schema caps the stored form at 4096) |
| `MEASUREMENT_MAX_FUTURE_SKEW` | `5m` | how far ahead of the server clock `recorded_at` may be (at most `1h`) |
| `IDEMPOTENCY_TTL` | `24h` | how long an `Idempotency-Key` stays replayable (`1m`–`168h`) |
| `IDEMPOTENCY_RETENTION_INTERVAL` | `1h` | how often expired idempotency records are swept |
| `PROCESSOR_URL` | `http://127.0.0.1:8081` | the processing service, scheme and host only |
| `PROCESSOR_TOKEN` | none | shared secret presented to the processor as `Authorization: Bearer`; required in staging and production, never logged |
| `PROCESSOR_TIMEOUT` | `5s` | bound on one call to the processor; must be shorter than `HTTP_REQUEST_TIMEOUT` |
| `PROCESSOR_MAX_ATTEMPTS` | `3` | attempts per dispatch, including the first (1–5) |
| `PROCESSOR_BACKOFF` | `100ms` | delay before the second attempt, doubling thereafter |
| `PROCESSOR_MAX_BACKOFF` | `2s` | cap on that delay |
| `PROCESSING_MAX_JOB_MEASUREMENTS` | `100000` | readings one job may carry; must not exceed the processor's own bound |
| `PROCESSING_ALGORITHM_VERSION` | `1.0.0` | the algorithm version jobs ask for; the processor refuses any other |
| `PROCESSING_FAILURE_RECORD_TIMEOUT` | `5s` | bound on the write that records why a job failed |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none | OTLP/HTTP collector; empty propagates trace context but exports nothing |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | `10s` | bound on one export attempt |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | permits plain HTTP to the collector; refused outside local development |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | fraction of traces this service starts that are recorded; a decision made upstream is always honoured |
| `REDIS_URL` | none | `redis://` or `rediss://`; empty disables Redis and every feature that uses it degrades |
| `REDIS_TIMEOUT` | `250ms` | bound on one Redis command; must be shorter than `HTTP_REQUEST_TIMEOUT` |
| `REDIS_DIAL_TIMEOUT` | `2s` | bound on establishing a connection |
| `REDIS_POOL_SIZE` | `10` | connections held |
| `REDIS_NAMESPACE` | `vitalmesh` | prefix on every key, so environments can share a server |
| `REDIS_RECOVERY_INTERVAL` | `5s` | how long calls fail locally after a failure before Redis is tried again |
| `RATE_LIMIT_ENABLED` | `true` | rate limiting works without Redis, using a per-replica counter |
| `RATE_LIMIT_WINDOW` | `1m` | the period each limit applies to (`1s`–`1h`) |
| `RATE_LIMIT_ANONYMOUS` | `60` | requests per window for an unauthenticated caller |
| `RATE_LIMIT_AUTHENTICATED` | `300` | requests per window for a signed-in caller |
| `RATE_LIMIT_ADMIN` | `1000` | requests per window for an ADMIN |
| `TRUSTED_PROXY_HOPS` | `0` | how far back through `X-Forwarded-For` the client address may be taken (at most 8) |
| `CACHE_ENABLED` | `true` | short-lived caching; without Redis every read goes to the database |
| `CACHE_PATIENT_TTL` | `30s` | how long a patient record may be served from the cache (at most 5m) |
| `READINESS_TIMEOUT` | `2s` | bound for the whole `/ready` evaluation |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

## Endpoints

| Route | Purpose |
|---|---|
| `GET /health` | liveness; never consults dependencies |
| `GET /ready` | readiness; `200` when every registered check passes (currently `postgres`), otherwise `503` with `"status":"not_ready"`. Check failure causes are logged, not returned. |
| `POST /api/v1/auth/login` | exchanges `{"email","password"}` for an access token (see [docs/API.md](../../docs/API.md#authentication)) |
| `GET /api/v1/auth/me` | the account behind the presented token; requires `Authorization: Bearer` |
| `POST /api/v1/patients`, `GET /api/v1/patients`, `GET /api/v1/patients/{patient_id}`, `DELETE /api/v1/patients/{patient_id}` | the Patient API (see [docs/API.md](../../docs/API.md#patients)); soft delete, audit record in the same transaction |
| `POST /api/v1/measurements`, `POST /api/v1/measurements/batch`, `GET /api/v1/measurements/{measurement_id}`, `GET /api/v1/patients/{patient_id}/measurements`, `DELETE /api/v1/measurements/{measurement_id}` | the Measurement API (see [docs/API.md](../../docs/API.md#measurements)); strict validation against the type catalogue, all-or-nothing batches, one audit record per reading |

| `POST /api/v1/processing/jobs`, `GET /api/v1/processing/jobs/{job_id}`, `GET /api/v1/patients/{patient_id}/processing-results` | the Processing API (see [docs/API.md](../../docs/API.md#processing)); the job is created, dispatched to the Rust processor and answered in one request |

Write operations that could duplicate state (`POST /patients`, `POST /measurements`, `POST /measurements/batch`, `POST /processing/jobs`) accept an `Idempotency-Key`; see [docs/API.md](../../docs/API.md#idempotency).

The binary also carries the schema, `api-gateway migrate up|down|version|force` (see [docs/DATABASE.md](../../docs/DATABASE.md)), and creates accounts: `api-gateway users create <email> <ADMIN|OPERATOR|USER>` reads the password (at least 12 characters) from standard input, hashes it with the configured parameters, inserts the user and a `USER_CREATED` audit entry in one transaction. It needs `DATABASE_URL` and honours `PASSWORD_HASH_*`. This is how the first administrator is bootstrapped; there is never a default account.

## Authentication

- **Passwords** are stored as Argon2id PHC strings (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`) with a random 16-byte salt and a 32-byte key. `auth.Hasher` verifies hashes produced with any parameters (bounded, so a corrupt hash cannot demand unbounded work) and reports when one predates the configured parameters; a successful login re-hashes such a password transparently. Malformed stored hashes are an internal error, never a silent failure. Because Argon2id is memory-hard, the hasher runs at most `PASSWORD_HASH_MAX_CONCURRENT` computations at once; further logins wait for a slot until their request deadline, so a flood of anonymous login attempts cannot exhaust memory.
- **Secrets in configuration** (`config.Secret`) print as `[redacted]` through `fmt`, `slog` and `encoding/json`, so a dumped configuration never contains key material.
- **Access tokens** are HS256 JSON Web Tokens produced by `auth.Tokens`, implemented on the standard library (`crypto/hmac`) so that the accepted algorithm is fixed by construction: the header must say `HS256`, `alg: none` and every other algorithm are rejected before any key is consulted, and the signature is compared in constant time. Claims: `iss`, `sub` (user id), `role`, `iat`, `exp`, `jti` (random UUID). The `kid` header selects the verification key among `JWT_SECRET` and `JWT_PREVIOUS_SECRETS`; an unknown `kid` is rejected.
- **Login** (`auth.Service.Login`) looks the account up by normalised email, verifies the password, refuses disabled accounts, issues a token and appends a `LOGIN` audit entry in the request's id. An unknown email still runs one Argon2id verification against a throwaway hash so that timing does not reveal whether an account exists. The audit write is part of the operation: if it fails the login fails.
- **Middleware.** `middleware.Authenticate` reads `Authorization: Bearer <token>`, verifies it and stores an `auth.Principal` (user id, role, token id, expiry) in the request context. `middleware.Require(policy, permission)` then checks the principal's role against the policy.
- **Errors** are explicit and never reveal internals: `AUTHENTICATION_REQUIRED`, `INVALID_TOKEN`, `TOKEN_EXPIRED`, `INVALID_CREDENTIALS` (401), `ACCOUNT_DISABLED` and `PERMISSION_DENIED` (403). Every 401 carries a `WWW-Authenticate: Bearer` challenge; `respond` adds the default one so no code path can omit it.
- **Known limits, by design of this phase:** a token stays valid until it expires (15 minutes by default) even if the account is disabled or its role changes in between (`/auth/me` re-checks the account; the Redis denylist phase closes the gap for other routes), and there is no per-account lockout or request rate limit yet (the Redis rate-limiting phase adds them; until then the hashing bound above is the only brake on password guessing, so deploy behind an edge rate limiter).
- **Never logged:** passwords, hashes, tokens and emails. Failure records carry a `reason` and, where known, the `user_id`; success records carry `user_id` and `token_id`; authorization denials carry `user_id`, `role` and `permission`.
- **Not yet:** logout (needs the Redis token denylist) and the user management handlers; their routes and permissions are already declared.

## Authorization

Authorization is data, enforced by the route table, so it can be read and tested without running anything:

- **Policy** (`authz.Default`): a role -> permissions table. Permissions name operation classes (`users:manage`, `patients:read`, `patients:write`, `measurements:read`, `measurements:write`, `jobs:read`, `jobs:write`, `results:read`). ADMIN holds all of them, OPERATOR everything except `users:manage`, USER the four read permissions. Two reserved permissions describe routes that need no grant: `public` (no token) and `authenticated` (any known role). Unknown roles hold nothing, not even `authenticated`.
- **Route table** (`authz.Routes`): every public API operation of SPECIFICATIONS.md sections 10–13 plus authentication and user management, each with the permission it requires. This is the one place that says who may call what; the client-facing matrix in [docs/API.md](../../docs/API.md#authorization) is derived from it and a test pins the two together cell by cell.
- **Mounting** (`httpapi.Mount`): the HTTP route table is built by walking `authz.Routes` and registering the operations that have handlers, each behind `Authenticate` and `Require` unless the rule says `public`. A handler for an operation the table does not list is a start-up error, so no endpoint can be served without an authorization decision, and operations without handlers are simply not registered (404) rather than stubbed.
- **Decisions are server-side only.** The role comes from the verified token's claims; request headers, body fields and query parameters play no part. `Require` without a principal fails closed with 401 even if `Authenticate` were missing from the chain.
- **Resource-level rules** (an account acting on its own record, a patient's ownership) do not exist in the specification's data model; when one is needed it belongs in the handler or service of that operation, with its own tests, not in the policy.
- **Tests** exercise every role against every listed operation through the real router and middleware (`internal/httpapi/routes_test.go`), the policy cell by cell against the documented matrix (`internal/authz/policy_test.go`), the middleware in isolation, and escalation attempts: rewritten role claims, tokens minted with a rogue key, `alg: none`, unknown roles, expired ADMIN tokens and role-asserting headers.

Unknown paths return `404` and wrong methods `405` (with `Allow`), both as the standard error envelope:

```json
{"error":{"code":"NOT_FOUND","message":"The requested resource does not exist.","request_id":"..."}}
```

## API conventions

The client-facing conventions are specified in [docs/API.md](../../docs/API.md). How they are implemented here:

- **Versioning.** Feature routes are mounted on `rt.Group(httpapi.APIv1)` from `authz.Routes`; the prefix constant is the only place the version string appears. A feature adds its handlers to `Handlers.operations` and, if it introduces an operation, a rule to `authz.Routes`.
- **Middleware chain** (`httpapi.Wrap`, outermost first): request ID, security headers, request logging, request timeout, panic recovery, body limit. The timeout buffers the handler's output so a late handler cannot corrupt the 504 response. Per-route middleware (authentication, authorization) is attached with `Group.With` and therefore never runs for 404/405 responses.
- **Input.** Handlers call `request.DecodeJSON`, which requires `Content-Type: application/json`, rejects unknown fields and trailing values, and classifies every decoder failure into a client-safe `*domain.Error`. `request.Pagination` parses `limit` and `cursor`.
- **Validation.** `validate.Validator` accumulates field errors; `Err()` yields one `VALIDATION_FAILED` error carrying all of them as `details`.
- **Output.** `respond.JSON` encodes before writing and sets `Content-Length`; `respond.Error` maps `domain.Kind` to a status, includes `details`, treats context deadline and cancellation explicitly, and turns anything unclassified into a logged generic 500.
- **Pagination.** `model.NewPage` builds the `items`/`next_cursor`/`has_more` envelope; `pagination.EncodeCursor`/`DecodeCursor` give repositories opaque URL-safe keyset cursors.

## Feature services

A feature is an application package (`patient` is the template) with a `Service` whose methods are the API's use cases, a `Store` interface it declares for persistence, validation through `validate`, client-safe errors with the feature's own codes, and audit events passed to the store so that the PostgreSQL implementation writes them in the same transaction as the state change. The handler in `httpapi/handler` only decodes, calls the service and encodes; the store in `infra/postgres` only composes repositories inside `WithTx`. Tests: service tests over `patienttest.MemoryStore`, API tests through `httpapi.NewHandler` with the same memory store, store tests against PostgreSQL, and one end-to-end test in `app` with real login.

## Measurements and idempotency

- **Type catalogue** (`measurement.Catalog`) mirrors the `measurement_types` table: canonical unit and technical range per type. The service validates against the catalogue so that every failing field is reported at once with the `MEASUREMENT_VALIDATION_FAILED` code of section 25; the database trigger enforces the same rules independently, and an integration test keeps the two identical.
- **Batches** are validated item by item (`items[i].field`), checked for duplicates within the batch and for patient existence per distinct patient, then stored in one transaction with one `MEASUREMENT_CREATED` record per reading. A reading the database still rejects (a duplicate of a stored one, say) is reported by index through `postgres.BatchItemError` and nothing from the batch is kept. Batch sizes are reported to the metrics port.
- **Idempotency** (`middleware.Idempotency`) implements section 24 over the `idempotency_keys` table (OQ-10): the key is scoped to `(account, method, path)`, the request fingerprint is a SHA-256 of method, path and body bytes, the first response is stored (jsonb) and replayed, mismatched bodies and in-flight duplicates are refused, `5xx` outcomes release the key, and expired records are replaced lazily. Which operations take part is declared once in `httpapi.idempotentOperations`; the middleware runs inside authentication and authorization so that a key can never act for another account. The retention job that purges expired rows belongs to the retention phase; `Idempotency.DeleteExpired` is ready for it.

## Processing and the Rust processor

`processing` is the feature service; `infra/processorclient` is the client for the internal contract (`contracts/internal-api/processor-v1.json`), and `infra/postgres.JobStore` is its persistence.

- **Transaction boundaries.** Three short transactions bracket one long external call, and none is open while that call is in flight: the job is created `PENDING` with its audit record; it is moved to `PROCESSING`, counting the attempt; then the readings are fetched and the processor is called with no transaction held; then the results and the transition to `COMPLETED` are written together, or the failure is recorded. A unit test drives the service with a store that fails the test if it is touched while the processor call is running, so the rule is enforced rather than merely intended.
- **Lifecycle.** Every transition is a compare-and-set on the current status and a database trigger rejects anything outside the state machine of section 92, so two writers cannot both move one job. A `COMPLETED` job always has its results, because they are inserted in the same transaction as the transition.
- **Timeout and retries.** The client bounds each attempt by `PROCESSOR_TIMEOUT`, retries up to `PROCESSOR_MAX_ATTEMPTS` with exponential backoff capped at `PROCESSOR_MAX_BACKOFF`, honours a `Retry-After` the processor sends, and stops early when the caller's deadline leaves no room for another attempt. It also tells the processor what is left of the attempt (`X-Request-Timeout-Ms`) so work nobody is waiting for stops there too.
- **Retry classification** (section 93). Unavailable, overloaded, shutting down, timed out and internal failures are retried; a refusal of the data, a cancellation, a credential the processor will not take, and anything this gateway cannot parse are not. The classification is on the error and is reported to clients as `retryable` semantics through the status code.
- **Safe failure.** A processor that is down, slow or incoherent produces a `5xx` with a stable code and a job row that survives, `FAILED`, with its diagnosis and attempt count. The processor's own messages are never repeated to a client. Because a `5xx` leaves no idempotency record, the same key can be reused to retry. The rest of the API is unaffected: the processor is not part of readiness, so one service being down does not take the gateway out of rotation.
- **Bounds.** The readings for a job are read with a limit one above `PROCESSING_MAX_JOB_MEASUREMENTS`, so an oversized range is refused before anything is sent rather than by the processor. The gateway's ceiling must not exceed the processor's, which the processor advertises on its health endpoint and an end-to-end test compares. The client bounds the response it will read.
- **Whole answers only.** The processor reports how many readings it accepted, skipped and rejected. Every reading the gateway sends came from its own database, was validated against the same catalogue and is of a requested type, so all of them should be accepted; if the counts do not add up, the statistics cover less data than the job asked for. The job then fails with `PROCESSING_INCOMPLETE` and stores nothing, rather than presenting a partial answer as a whole one.
- **Snapshot reads.** A job asks for several measurement types and each is a separate query, so the reads run in one read-only repeatable-read transaction. Without it a write landing between two queries would put the job's types at different instants. The snapshot takes no locks and holds no external call.
- **Forward compatibility.** The client ignores response fields it does not know, because adding one is a compatible change under the contract's versioning rules. Drift is caught at build time by the contract tests, which decode the document strictly, rather than at runtime where it would break a release the processor was entitled to make.
- **Tests.** Service tests with a recording store and a scripted processor; client tests over `httptest` covering every status the contract defines, both timeouts, the retry budget and every malformed answer; contract tests validating the dispatch the gateway builds against the OpenAPI document and decoding the document's own outcome; integration tests against PostgreSQL asserting the rows, the results, the audit record and the idempotency behaviour; and `tests/e2e`, which runs the real processor binary and the wired gateway together.

## Redis

`infra/redisclient` is the adapter; `ratelimit` and `cache` are the features built on it. Redis holds three things, and none of them is a record of truth (SPECIFICATIONS.md section 23): rate-limit counters, short-lived cache entries and idempotency locks. Everything the gateway needs to be correct is in PostgreSQL, so losing Redis costs precision and latency, never data.

- **Lifecycle.** The client dials lazily, so the gateway starts while Redis is down and picks it up when it returns; only an address that cannot be parsed stops start-up, because that is a mistake rather than an outage. Connections are closed on shutdown, before the database, because nothing depends on them.
- **Timeouts.** Every call is bounded by the client's own per-command timeout and by the caller's context, whichever is shorter, and configuration refuses a Redis timeout that is not shorter than the request timeout. A slow Redis therefore cannot be what makes a request slow.
- **Degradation.** Every operation reports unavailability rather than failing a request. After a failure the client stops dialling for `REDIS_RECOVERY_INTERVAL` and answers locally, so an outage costs one timeout rather than one per request, and it logs the transition in each direction once rather than flooding. Redis is deliberately **not** a readiness check: its outage degrades three features and stops none, so failing readiness would take a working gateway out of rotation (section 90, OQ-27).
- **Rate limiting** (section 29) is a fixed-window counter maintained by a Lua script, so replicas share one budget and concurrent requests cannot both read the same pre-increment value. Limits are per role and come from configuration. An authenticated request counts against its account, an anonymous one against its address, and `X-Forwarded-For` is trusted only as far as `TRUSTED_PROXY_HOPS`, so a client cannot choose its own budget. Every response carries `RateLimit-Limit`, `RateLimit-Remaining` and `RateLimit-Reset`; a refusal is `429` with `Retry-After`. Without Redis the limit still applies, from a per-replica counter: weaker, because N replicas then allow N budgets, but far better than failing open or turning a Redis outage into an API outage.
- **Caching** (section 84) serves `GET /patients/{patient_id}`. What is cached is the record, never the answer: the deleted-patient visibility rule is applied to whatever was read, so two callers with different roles cannot be served each other's answer. Writes invalidate the entry explicitly, on a context detached from the request so that a client hanging up cannot leave a stale entry; the time to live is the second line of defence and is capped at five minutes. Nothing that makes a decision reads through the cache: the patient check behind a measurement write goes to the database, and a test asserts it.
- **Idempotency locks** (section 24, OQ-10) are the ephemeral coordination of section 23. A `SET NX PX` lock lets a concurrent replay be refused without a database round trip; the unique constraint on `(account, method, path, key)` is what actually serialises replays. A lock that cannot be consulted is skipped, and an integration test runs the whole idempotency contract three times, with Redis, with Redis unreachable and with none configured, to show the outcomes are identical.
- **Tests.** Everything above is tested against a real Redis (`make dev-redis`, `TEST_REDIS_URL`), with each test in its own key namespace, plus a failure path for each feature against an address nothing listens on.

## Observability hooks

- **Metrics** (`observability/metrics.Recorder`): `middleware.Metrics` records every request as method, matched route pattern, status and duration; services record each operation with a closed outcome vocabulary (`ok`, `invalid`, `not_found`, `conflict`, `denied`, `error`, `cancelled`). Route patterns come from the router, never from request paths, so labels stay bounded and identifiers never become labels (SPECIFICATIONS.md section 41). `Noop` is wired until the observability phase binds Prometheus.
- **Metrics endpoint**: `GET /metrics` serves the Prometheus exposition, unversioned and unauthenticated like the health probes, because it serves the platform's scraper rather than clients. It carries no identifiers by construction; deployments restrict it at the network.
- **Tracing** (`observability/tracing`): OpenTelemetry, with W3C trace context in and out. A request's trace covers the gateway's own span, every PostgreSQL statement, every Redis command, each service operation, and the call to the processor, which continues the same trace on its side.

## Request logs

One JSON record per request: `timestamp`, `level`, `service`, `version`, `environment`, `request_id`, `trace_id` (when a tracer set one), `message` (`"request"`), `method`, `path`, `route` (the matched pattern, or `unmatched`), `status`, `bytes`, `duration_ms`. Clients may supply `X-Request-ID` (1–128 characters of `[A-Za-z0-9._-]`); other values are replaced. The effective ID is echoed in the response header. Services add their own records (`patient created`, `patient deleted`) carrying `request_id`, the resource id and the acting `user_id`; never payload contents.

## Tracing

One trace covers client, gateway, processor and the dependencies each touches. W3C `traceparent` arrives on the request and is read before the span opens, so a trace a client started continues here rather than restarting; the same header is injected into the call to the processor.

**Propagation does not depend on having a backend.** A real tracer is built whether or not a collector is configured, so trace ids exist, reach the logs as `trace_id`, and are injected into outgoing calls even with `OTEL_EXPORTER_OTLP_ENDPOINT` unset. Without an endpoint the spans are recorded and dropped rather than exported. Making propagation conditional on a backend would be exactly backwards.

**A telemetry backend is never on the critical path.** Spans go to a batch processor and are exported on their own goroutine, so a collector that is slow, refused or absent costs a request nothing. An outage is one log line, not one per attempt, and shutdown is bounded so a collector that has gone away cannot hold up exit.

**What a span may carry.** The method, the matched route pattern, the status, resource identifiers, counts and durations. Never a path, a query string, a header or a body. The attribute conversion enforces the last part: a value that is not a string, number, boolean, duration or `Stringer` is recorded as its type rather than its contents, so a struct holding readings cannot be serialised into a span by being passed as an interface. Every string attribute and every recorded error then passes through the same rules the logger applies, from the shared `observability/redact` package: a name that is a credential or a personal fact is refused, a token is removed by shape wherever it appears, and any value is bounded in length. Logs and spans are two exits from the process and cannot drift apart in what they refuse to carry (SPECIFICATIONS.md sections 40 and 41).

**The call to the processor is a span of its own.** Each attempt opens a client span carrying the method, the processor's host and the status, and records its error, so a retried call shows in the trace as what it was: several attempts with their own outcomes, rather than one long gap between the gateway's span and the processor's. The processor's server span nests under the attempt that made the call.

**Probes are not traced.** `/health`, `/ready` and `/metrics` are polled by the platform every few seconds per replica; traced, they would outnumber real traffic in any trace store and bury the traces that matter. They stay visible in metrics and logs.

## What the metrics publish

Everything is prefixed `vitalmesh_` and registered when the recorder is built, so two components cannot publish the same series under different meanings.

| Metric | Type | Labels |
|---|---|---|
| `http_requests_total` | counter | method, route, status |
| `http_errors_total` | counter | method, route, class (client or server) |
| `http_request_duration_seconds` | histogram | method, route |
| `operation_total` | counter | operation, outcome |
| `operation_duration_seconds` | histogram | operation |
| `operation_in_flight` | gauge | operation |
| `database_query_duration_seconds` | histogram | statement, outcome |
| `database_connections_*` | gauge | none |
| `redis_command_duration_seconds` | histogram | command, outcome |
| `batch_size` | histogram | operation |
| `cache_reads_total` | counter | operation, result |
| `rate_limit_decisions_total` | counter | decision, backend |

Processing jobs, their duration and their failures are the `processing.*` series of the operation family, so a rate of jobs and a rate of failures come from one query. Active jobs is `operation_in_flight{operation="processing.job"}`, moved around the call to the processor so that it counts work actually running.

**Cardinality is a property of the port, not of its callers.** Every label comes from a closed vocabulary: routes are the patterns the router registered, statement verbs are normalised to eight keywords, and outcomes, decisions and backends are fixed sets. No method on the recorder takes a value that varies per request, so a user id, patient id, request id or trace id cannot become a label even by mistake, because there is no parameter to put one in (SPECIFICATIONS.md section 41).

The one label a client controls is the request method: HTTP permits any token as a method, so recording it verbatim would let a caller create a series per request. The recorder keeps only the methods the router can route and folds everything else into `OTHER`. The smoke test sends invented methods at both services and fails if either creates a series.

Series whose labels form a small closed set are published at zero from start-up, so an alert works from the first scrape rather than from the first occurrence. Where the label space is open, such as route by method by status, the series appears with the first request.

## What never reaches a log

Call sites pass identifiers rather than values, and `observability/logging` enforces that rather than trusting it (SPECIFICATIONS.md section 41). Every attribute passes through one hook on its way to the handler, so there is no path into the output that skips the rules, including attributes a logger was derived with and attributes nested in a group.

- **By name.** An attribute called `password`, `secret`, `token`, `authorization`, `cookie`, `signature`, `email`, `date_of_birth`, `phone`, `external_reference`, `body` or `payload`, or one ending in `_token`, `_secret`, `_password`, `_hash`, `_email` or `_signature`, is replaced with `[redacted]`. The list names credentials and facts about people, not their neighbours: `token_id` identifies a token and survives, as do `key`, `count` and every `*_id`.
- **By shape.** A JSON Web Token is removed wherever it appears, including inside a message, an error, a URL or an `Authorization` header echoed into one. Detection anchors on the `eyJ` prefix every JWT header carries, so a hostname or a version string cannot trip it.
- **By size.** Any single string is bounded at 512 bytes and says how much it dropped, so a request body or a batch of readings logged by mistake is truncated rather than stored whole.

What stays is what an incident needs: opaque identifiers (`user_id`, `patient_id`, `measurement_id`, `job_id`, `request_id`), counts, statuses and durations. Redaction costs nothing operationally: a failed login still logs `login failed` with its reason and the `user_id`, just never the address or the password.

The processor applies the same three rules in `redact.rs`, at the point its JSON formatter writes a record, so span fields are covered as well as event fields.
