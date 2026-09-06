# API Gateway

The Go service that forms VitalMesh's public boundary (SPECIFICATIONS.md section 6.1). This document describes how the code is organised and the rules that keep it that way.

## Layout

```text
cmd/api-gateway/            entry point: load config, build logger, run the app
internal/
  app/                      composition root: wires packages together, runs the lifecycle
  auth/                     application service: password hashing, access tokens, login, principal
  authz/                    authorization policy (role -> permissions) and the route -> permission table
  config/                   typed configuration loaded from the environment
  domain/                   vocabulary shared by every feature (error model, entities)
  health/                   application service: readiness evaluation over Checker ports
  httpapi/                  HTTP transport: router, route groups, route table, server lifecycle
    handler/                HTTP handlers (HTTP <-> application translation only)
    middleware/             request ID, security headers, logging, timeout, recovery, body limit, authentication, authorization
    model/                  request/response wire models (error envelope, pages, health, login, user)
    request/                JSON body decoding and pagination parameter parsing
    respond/                JSON and error-envelope writers, domain error -> status mapping
  pagination/               cursor primitives and page-size bounds
  validate/                 input validation accumulator producing domain validation errors
  infra/postgres/           PostgreSQL: pool, migrator, transactions, repositories, error mapping
    postgrestest/           per-test database helper for integration tests
  observability/logging/    structured logging setup
  requestid/                request identifier generation, validation and context carriage
  buildinfo/                version injected at link time
migrations/                 embedded SQL migrations (see docs/DATABASE.md)
```

## Layers and dependency rules

| Layer | Packages | May import |
|---|---|---|
| Transport | `httpapi` and its subpackages | application services, `domain`, `config`, `requestid` |
| Application | `health`, `auth`, `authz`, and one package per feature as they arrive | `domain`, `config`, `validate`, `requestid`, `auth` (for `authz`), ports it declares itself |
| Domain | `domain` | nothing inside the service |
| Infrastructure | `observability/logging`, `infra/postgres`; later `infra/redis`, `infra/processor` | the application ports it implements, `domain`, `config` |
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

## Request logs

One JSON record per request: `timestamp`, `level`, `service`, `version`, `environment`, `request_id`, `message` (`"request"`), `method`, `path`, `status`, `bytes`, `duration_ms`. Clients may supply `X-Request-ID` (1–128 characters of `[A-Za-z0-9._-]`); other values are replaced. The effective ID is echoed in the response header.
