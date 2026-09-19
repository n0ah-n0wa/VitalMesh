# Public API contract

`vitalmesh-public-v1.json` is the machine-readable description of the API the Go gateway serves to clients: the operations under `/api/v1` plus the unversioned platform probes (SPECIFICATIONS.md sections 9–13 and 24–28). It is an OpenAPI 3.0.3 document and it is the authority on the wire format. Where this README and the document disagree, the document wins.

`docs/API.md` is the prose reference for the same surface. It explains conventions in more depth and is the better place to start reading; this document is the better place to generate a client from.

| File | What it is |
|---|---|
| `vitalmesh-public-v1.json` | The contract. OpenAPI 3.0.3, JSON. |
| `vitalmesh-public-v1.lock.json` | The reviewed fingerprint of the contract's interface surface. |

## Why JSON rather than YAML

The same reason as the internal contract: JSON and YAML are both valid OpenAPI serialisations that every OpenAPI tool reads, and JSON is what CI can check today without adding a parser to a service that does not otherwise want one. The gate below runs with the Go toolchain and nothing else. Redocly, Spectral, `oasdiff` and `oapi-codegen` all consume this file unchanged when that tooling arrives.

## What it describes

Sixteen operations: fourteen under `/api/v1` and the two unversioned probes.

| Operation | Purpose |
|---|---|
| `GET /health` | Liveness, and which service, version and environment answered. |
| `GET /ready` | Readiness, with one entry per dependency. |
| `POST /api/v1/auth/login` | Exchange credentials for a bearer token. |
| `GET /api/v1/auth/me` | The account behind the token. |
| `POST /api/v1/patients` | Create a synthetic patient. |
| `GET /api/v1/patients` | List patients, paginated. |
| `GET /api/v1/patients/{patient_id}` | Read one patient. |
| `DELETE /api/v1/patients/{patient_id}` | Soft-delete a patient. |
| `GET /api/v1/patients/{patient_id}/measurements` | List a patient's readings, filtered and paginated. |
| `GET /api/v1/patients/{patient_id}/processing-results` | List a patient's processing results. |
| `POST /api/v1/measurements` | Store one reading. |
| `POST /api/v1/measurements/batch` | Store up to 1000 readings in one transaction. |
| `GET /api/v1/measurements/{measurement_id}` | Read one reading. |
| `DELETE /api/v1/measurements/{measurement_id}` | Delete a reading. |
| `POST /api/v1/processing/jobs` | Create and run a processing job. |
| `GET /api/v1/processing/jobs/{job_id}` | Read a processing job. |

### What it deliberately does not describe

**User management and job cancellation.** `POST /users`, `GET /users`, `GET /users/{user_id}`, `PATCH /users/{user_id}/role` and `POST /processing/jobs/{job_id}/cancel` have authorization rules in `internal/authz/routes.go` and no handler, so no route is registered and each answers `404` (OPEN_QUESTIONS OQ-04 and OQ-06). A contract that described them would promise endpoints that do not answer. `TestUnservedAuthorizationRulesAreAbsentFromTheContract` states the current set, so serving one becomes a visible decision rather than a silent divergence.

**`/metrics`.** It is a Prometheus exposition rather than JSON, it serves the platform's scraper rather than clients, and deployments restrict it at the network.

## The gate

`make contracts-check` runs it, and CI runs it in the `contracts` job. Three kinds of check, in two packages:

`services/api-gateway/internal/contract` checks the document against itself and against the gateway's own source:

- every operation is named, described and accepts the correlation headers, and declares a success and a 500;
- every failure that returns the error envelope declares its `x-error-codes` and its `x-retryable` classification;
- **every published error code exists in the gateway's non-test Go source**, so a code that is renamed in the implementation and not in the contract fails;
- every example validates against the schema it is published against;
- every `$ref` resolves, every component is referenced, and every schema and property carries a description;
- authentication is declared and applied by default, and exactly `health`, `readiness` and `login` may opt out;
- `Idempotency-Key` is declared on exactly the four write operations that can create duplicate state.

`services/api-gateway/internal/httpapi` checks the document against the router:

- **the contract's operations and the routes `NewHandler` mounts are the same set.** The served set is derived from `authz.Routes` and `Handlers.operations()`, the same tables the router is built from, so an endpoint added, removed or left unimplemented cannot silently disagree with what clients are told.

And the fingerprint lock makes an interface change impossible to land silently, exactly as for the internal contract.

## Versioning

The rules are the public half of the internal contract's rules.

**Backward-compatible**, no version change required: adding an operation, adding an optional request field, adding a response field, adding an enum value a client is not required to understand, relaxing a constraint, or rewording any description. The fingerprint still moves, so the lock is still re-recorded and the change is still reviewed.

**Breaking**, requires a new major version and a new path prefix (`/api/v2`): removing an operation or a field, making an optional field required, narrowing a type or a constraint, removing an enum value, changing a field's meaning, or changing which status code an outcome produces. The previous version keeps working until it is retired explicitly.

`info.version` is the contract's own semantic version and the lock file records it. It tracks the document, not the gateway build.

## Changing the contract

1. Edit `vitalmesh-public-v1.json`.
2. If the change adds or removes an operation, update `publicOperations` in `internal/contract/public_api_test.go` and the handler that serves it, so both sides of the agreement move together.
3. Run `make contracts-check`. It fails until the lock is re-recorded.
4. Review the change against the rules above.
5. Run `make contracts-lock` and commit the updated lock with the contract.
