# API Gateway

The Go service that forms VitalMesh's public boundary (SPECIFICATIONS.md section 6.1). This document describes how the code is organised and the rules that keep it that way.

## Layout

```text
cmd/api-gateway/            entry point: load config, build logger, run the app
internal/
  app/                      composition root: wires packages together, runs the lifecycle
  config/                   typed configuration loaded from the environment
  domain/                   vocabulary shared by every feature (error model; entities later)
  health/                   application service: readiness evaluation over Checker ports
  httpapi/                  HTTP transport: router, route groups, route table, server lifecycle
    handler/                HTTP handlers (HTTP <-> application translation only)
    middleware/             request ID, security headers, logging, timeout, recovery, body limit
    model/                  request/response wire models (error envelope, pages, health)
    request/                JSON body decoding and pagination parameter parsing
    respond/                JSON and error-envelope writers, domain error -> status mapping
  pagination/               cursor primitives and page-size bounds
  validate/                 input validation accumulator producing domain validation errors
  observability/logging/    structured logging setup
  requestid/                request identifier generation, validation and context carriage
  buildinfo/                version injected at link time
```

## Layers and dependency rules

| Layer | Packages | May import |
|---|---|---|
| Transport | `httpapi` and its subpackages | application services, `domain`, `config`, `requestid` |
| Application | `health`, and one package per feature as they arrive | `domain`, ports it declares itself |
| Domain | `domain` | nothing inside the service |
| Infrastructure | `observability/logging`; later `infra/postgres`, `infra/redis`, `infra/processor` | the application ports it implements, `domain`, `config` |
| Composition | `app`, `cmd/api-gateway` | everything |

- Dependencies point inward: transport -> application -> domain. Infrastructure implements application interfaces and is only referenced by `app`.
- Interfaces are declared by the package that consumes them, next to the consumer. `health.Checker` is the first example; repository interfaces will live in the feature package that uses them (for instance `internal/patient/repository.go`), and PostgreSQL implementations in `internal/infra/postgres`.
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
| `HTTP_WRITE_TIMEOUT` | `10s` | |
| `HTTP_IDLE_TIMEOUT` | `60s` | |
| `HTTP_SHUTDOWN_TIMEOUT` | `10s` | grace period for in-flight requests on SIGTERM/SIGINT; remaining connections are closed when it elapses and the process exits 1 |
| `HTTP_REQUEST_TIMEOUT` | `10s` | handler execution bound; must be shorter than `HTTP_WRITE_TIMEOUT` |
| `HTTP_MAX_BODY_BYTES` | `1048576` | largest accepted request body |
| `READINESS_TIMEOUT` | `2s` | bound for the whole `/ready` evaluation |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

## Endpoints

| Route | Purpose |
|---|---|
| `GET /health` | liveness; never consults dependencies |
| `GET /ready` | readiness; `200` when every registered check passes, otherwise `503` with `"status":"not_ready"`. Check failure causes are logged, not returned. |

Unknown paths return `404` and wrong methods `405` (with `Allow`), both as the standard error envelope:

```json
{"error":{"code":"NOT_FOUND","message":"The requested resource does not exist.","request_id":"..."}}
```

## API conventions

The client-facing conventions are specified in [docs/API.md](../../docs/API.md). How they are implemented here:

- **Versioning.** Feature routes register on `rt.Group(httpapi.APIv1)`; the prefix constant is the only place the version string appears.
- **Middleware chain** (`httpapi.Wrap`, outermost first): request ID, security headers, request logging, request timeout, panic recovery, body limit. The timeout buffers the handler's output so a late handler cannot corrupt the 504 response.
- **Input.** Handlers call `request.DecodeJSON`, which requires `Content-Type: application/json`, rejects unknown fields and trailing values, and classifies every decoder failure into a client-safe `*domain.Error`. `request.Pagination` parses `limit` and `cursor`.
- **Validation.** `validate.Validator` accumulates field errors; `Err()` yields one `VALIDATION_FAILED` error carrying all of them as `details`.
- **Output.** `respond.JSON` encodes before writing and sets `Content-Length`; `respond.Error` maps `domain.Kind` to a status, includes `details`, treats context deadline and cancellation explicitly, and turns anything unclassified into a logged generic 500.
- **Pagination.** `model.NewPage` builds the `items`/`next_cursor`/`has_more` envelope; `pagination.EncodeCursor`/`DecodeCursor` give repositories opaque URL-safe keyset cursors.

## Request logs

One JSON record per request: `timestamp`, `level`, `service`, `version`, `environment`, `request_id`, `message` (`"request"`), `method`, `path`, `status`, `bytes`, `duration_ms`. Clients may supply `X-Request-ID` (1–128 characters of `[A-Za-z0-9._-]`); other values are replaced. The effective ID is echoed in the response header.
