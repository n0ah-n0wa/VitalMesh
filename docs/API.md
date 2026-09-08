# API conventions

This document describes the conventions every VitalMesh public endpoint follows. The endpoint catalogue itself lives in the OpenAPI contract under `contracts/openapi/` (not yet written; see `IMPLEMENTATION_PLAN.md`, Phase 1). Nothing here is medical: all data is synthetic and all results are technical data-processing outputs (SPECIFICATIONS.md sections 1 and 3).

## Versioning

All public endpoints live under `/api/v1`. Breaking changes are published under a new prefix (`/api/v2`); the previous version keeps working until it is retired explicitly. Health endpoints (`/health`, `/ready`) are unversioned because they serve the platform, not clients.

## Content

- Requests with a body must send `Content-Type: application/json` (a `charset` parameter is accepted). Anything else is rejected with `415`.
- Bodies must contain exactly one JSON value matching the documented schema. Unknown fields are rejected, so typos never pass silently.
- Responses are always JSON (`Content-Type: application/json`) with an explicit `Content-Length`. `204 No Content` responses have no body.
- The maximum request body size is 1 MiB by default (configurable per deployment). Larger requests are rejected with `413` before the body is read when the length is declared, and as soon as the limit is crossed otherwise.

## Errors

Every failed request returns the same envelope:

```json
{
  "error": {
    "code": "VALIDATION_FAILED",
    "message": "The request is invalid.",
    "request_id": "DBYEUHL7YNZT4MHMR6CQZI55CT",
    "details": [
      {"field": "name", "message": "is required"},
      {"field": "size", "message": "must be between 1 and 10"}
    ]
  }
}
```

- `code` is a stable, machine-readable identifier. Clients should branch on it, not on `message`.
- `message` is human-readable and safe to display. It never contains stack traces, SQL, hostnames, or other implementation details.
- `request_id` identifies the request for support and log correlation. It equals the `X-Request-ID` response header.
- `details` is present only when specific input fields are at fault.

Codes defined by the platform (feature endpoints add their own):

| Code | Status | When |
|---|---|---|
| `INVALID_JSON` | 400 | malformed JSON, wrong value type, unknown field, or more than one JSON value |
| `EMPTY_BODY` | 400 | a body was required but not sent |
| `VALIDATION_FAILED` | 422 | well-formed input that violates a rule; see `details` |
| `INVALID_CURSOR` | 422 | the pagination cursor was not produced by this API |
| `NOT_FOUND` | 404 | no such route or resource |
| `METHOD_NOT_ALLOWED` | 405 | the route exists but not for that method; `Allow` lists the valid ones |
| `REQUEST_BODY_TOO_LARGE` | 413 | body exceeds the size limit |
| `UNSUPPORTED_MEDIA_TYPE` | 415 | `Content-Type` is not `application/json` |
| `REQUEST_TIMEOUT` | 504 | the request exceeded the server's processing bound |
| `REQUEST_CANCELLED` | 503 | the request was cancelled before completion |
| `INTERNAL_ERROR` | 500 | an unexpected failure; the cause is logged server-side under `request_id` |
| `AUTHENTICATION_REQUIRED` | 401 | the route needs an access token and none was sent |
| `INVALID_TOKEN` | 401 | the access token is malformed, tampered with, signed with an unknown key, or its subject no longer exists |
| `TOKEN_EXPIRED` | 401 | the access token is past its expiry; log in again |
| `INVALID_CREDENTIALS` | 401 | login with an unknown email or wrong password (deliberately indistinguishable) |
| `ACCOUNT_DISABLED` | 403 | the credentials or token are valid but the account is disabled |
| `PERMISSION_DENIED` | 403 | the token is valid but the account's role does not permit the operation |
| `INVALID_IDEMPOTENCY_KEY` | 422 | the `Idempotency-Key` header is not 1–255 characters of `[A-Za-z0-9._:-]` |
| `IDEMPOTENCY_KEY_REUSED` | 422 | the key was already used by this account for a different request |
| `IDEMPOTENCY_IN_PROGRESS` | 409 | the first request under this key is still running; `Retry-After` says when to retry |

## Authentication

The API uses JWT bearer tokens (SPECIFICATIONS.md section 9.1). Obtain one by logging in:

```http
POST /api/v1/auth/login
Content-Type: application/json

{"email": "admin@example.com", "password": "..."}
```

```json
{
  "access_token": "<opaque JWT>",
  "token_type": "Bearer",
  "expires_in": 900,
  "expires_at": "2026-09-06T12:15:00Z",
  "user": {"id": "…", "email": "admin@example.com", "role": "ADMIN", "status": "ACTIVE", "created_at": "…"}
}
```

- Email matching is case-insensitive. Both fields are required; the password may be up to 1024 bytes.
- Present the token on every other `/api/v1` request as `Authorization: Bearer <access_token>`. `GET /api/v1/auth/me` returns the calling account and is the simplest way to check a token.
- Tokens are signed (HS256), carry `sub` (user id), `role`, `iat`, `exp` and a unique `jti`, and expire after 15 minutes by default. There are no refresh tokens: log in again. Clients must treat the token as opaque.
- Failures are explicit (see the codes above) and every `401` carries a `WWW-Authenticate: Bearer` challenge. Login failures never say whether the email exists.
- Accounts are created by an operator with the gateway's command line (`api-gateway users create`); there is no self-registration and no default account.
- Every successful login is recorded in the audit log under the request's `request_id`. Credentials and tokens are never written to logs.

## Authorization

Every account has exactly one role: `ADMIN`, `OPERATOR` or `USER` (SPECIFICATIONS.md section 9.1). The role is carried in the access token and decides which operations the account may perform. Decisions are made by the server on every request from the verified token alone; nothing a client sends besides the token (headers, body fields, query parameters) can widen them.

| Capability | Operations | ADMIN | OPERATOR | USER |
|---|---|---|---|---|
| Manage users and roles | `POST /users`, `GET /users`, `GET /users/{user_id}`, `PATCH /users/{user_id}/role` | yes | no | no |
| Create and delete patients | `POST /patients`, `DELETE /patients/{patient_id}` | yes | yes | no |
| Read patients | `GET /patients`, `GET /patients/{patient_id}` | yes | yes | yes |
| Create and delete measurements | `POST /measurements`, `POST /measurements/batch`, `DELETE /measurements/{measurement_id}` | yes | yes | no |
| Read measurements | `GET /measurements/{measurement_id}`, `GET /patients/{patient_id}/measurements` | yes | yes | yes |
| Create and cancel processing jobs | `POST /processing/jobs`, `POST /processing/jobs/{job_id}/cancel` | yes | yes | no |
| Read jobs and results | `GET /processing/jobs/{job_id}`, `GET /patients/{patient_id}/processing-results` | yes | yes | yes |
| Own session | `GET /auth/me` | yes | yes | yes |
| Log in | `POST /auth/login` | anyone | anyone | anyone |

Roles nest: every operation an `OPERATOR` may perform is available to an `ADMIN`, and every `USER` operation to an `OPERATOR`. Operations are listed here before they are implemented; an unimplemented operation answers `404` for everyone.

Outcomes, in the order they are checked:

1. No token, or a token that cannot be verified: `401` with `AUTHENTICATION_REQUIRED`, `INVALID_TOKEN` or `TOKEN_EXPIRED` and a `WWW-Authenticate` challenge, whatever role the token claims.
2. A verified token whose role lacks the capability: `403 PERMISSION_DENIED`. The response does not say which permission was missing; the server logs the account, role and permission.
3. Otherwise the operation runs. Rules about specific resources (for example an account acting on itself) are enforced by the operation itself and documented with it.

A role change takes effect for tokens issued after it; tokens live 15 minutes by default.

## Patients

Patients are synthetic entities (SPECIFICATIONS.md section 11); the API stores and returns identifiers and demographics and applies no clinical logic. All four operations require a token; the Authorization matrix above says which roles may write.

| Operation | Success | Notes |
|---|---|---|
| `POST /api/v1/patients` | `201` with the patient and a `Location` header | OPERATOR or ADMIN |
| `GET /api/v1/patients/{patient_id}` | `200` with the patient | any role |
| `GET /api/v1/patients` | `200` with a page (see Pagination) in creation order | any role; deleted patients are never listed |
| `DELETE /api/v1/patients/{patient_id}` | `204` | OPERATOR or ADMIN; soft delete |

Request body of `POST`:

```json
{"external_reference": "synthetic-0001", "date_of_birth": "1984-02-29", "sex": "FEMALE"}
```

- `external_reference`: required, 1–128 characters after trimming, no control characters, unique across patients.
- `date_of_birth`: required, a calendar date `YYYY-MM-DD` between `1900-01-01` and today (UTC).
- `sex`: required, one of `FEMALE`, `MALE`, `OTHER`, `UNKNOWN`.

Patient representation:

```json
{
  "id": "…",
  "external_reference": "synthetic-0001",
  "date_of_birth": "1984-02-29",
  "sex": "FEMALE",
  "status": "ACTIVE",
  "created_at": "2026-09-06T12:00:00Z",
  "updated_at": "2026-09-06T12:00:00Z"
}
```

`status` is `ACTIVE`, `INACTIVE` or `DELETED`. Deletion is a soft delete: the record keeps its identity so that measurements, jobs and results stay valid, and `updated_at` moves. After deletion the patient answers `404` to OPERATOR and USER and is returned with `"status":"DELETED"` to ADMIN; deleting it again is `409`.

| Code | Status | When |
|---|---|---|
| `PATIENT_NOT_FOUND` | 404 | no patient has that id, the id is not a UUID, or the patient is deleted and the caller is not ADMIN |
| `PATIENT_ALREADY_EXISTS` | 409 | another patient has the same `external_reference` |
| `PATIENT_ALREADY_DELETED` | 409 | the patient was deleted before |

Every creation and deletion writes an audit record (`PATIENT_CREATED`, `PATIENT_DELETED`) with the acting account and the request's `request_id`, in the same transaction as the change: if the record cannot be written the change is not made.

## Measurements

Measurements are synthetic readings attached to a patient (SPECIFICATIONS.md section 12). The API validates and stores them; it applies no clinical interpretation.

| Operation | Success | Notes |
|---|---|---|
| `POST /api/v1/measurements` | `201` with the reading and a `Location` header | OPERATOR or ADMIN; accepts `Idempotency-Key` |
| `POST /api/v1/measurements/batch` | `201` with `{"items":[…]}` in input order | OPERATOR or ADMIN; all readings stored or none; accepts `Idempotency-Key` |
| `GET /api/v1/measurements/{measurement_id}` | `200` with the reading | any role |
| `GET /api/v1/patients/{patient_id}/measurements` | `200` with a page in recording order | any role; filters `type`, `from`, `to` |
| `DELETE /api/v1/measurements/{measurement_id}` | `204` | OPERATOR or ADMIN; hard delete, recorded in the audit log |

Request body of `POST /api/v1/measurements` (a batch wraps the same objects in `{"items":[…]}`):

```json
{
  "patient_id": "…",
  "type": "HEART_RATE",
  "value": 72,
  "unit": "bpm",
  "recorded_at": "2026-09-06T11:59:00Z",
  "source": "synthetic-monitor",
  "metadata": {"lead": "II"}
}
```

Validation is strict; every failing field is reported in one `422 MEASUREMENT_VALIDATION_FAILED` response, batch items as `items[i].field`:

- `patient_id`: required UUID of an existing, non-deleted patient.
- `type` and `unit`: `type` is one of the table below and `unit` must be exactly that type's canonical unit. No unit conversion is performed.
- `value`: required finite number within the type's technical range, inclusive. The ranges reject values that cannot be a reading at all; they are not medical thresholds.
- `recorded_at`: required RFC 3339 timestamp with a UTC offset, not before `1900-01-01T00:00:00Z` and not more than 5 minutes (configurable) ahead of the server clock. Stored and returned in UTC.
- `source`: required, 1–64 characters after trimming, no control characters. The same reading (patient, type, `recorded_at`, source) cannot be stored twice: `409 MEASUREMENT_ALREADY_EXISTS`, naming the batch item where applicable.
- `metadata`: optional JSON object (absent or `null` means `{}`), at most 2048 bytes as compact JSON (configurable, hard limit 4096) and at most 8 levels deep.
- Batches: 1 to 1000 items (configurable). Two items describing the same reading are rejected (`items[j]` "duplicates items[i]"). A batch is stored in one transaction with one audit record per reading; if any item is rejected, none is stored.

| Type | Unit | Range |
|---|---|---|
| `HEART_RATE` | `bpm` | 0 – 300 |
| `BLOOD_PRESSURE_SYSTOLIC` | `mmHg` | 0 – 300 |
| `BLOOD_PRESSURE_DIASTOLIC` | `mmHg` | 0 – 200 |
| `SPO2` | `%` | 0 – 100 |
| `BODY_TEMPERATURE` | `C` | 20 – 45 |
| `BLOOD_GLUCOSE` | `mg/dL` | 0 – 1000 |
| `RESPIRATORY_RATE` | `breaths/min` | 0 – 100 |

Listing `GET /api/v1/patients/{patient_id}/measurements` accepts `type` (one of the table), `from` (inclusive) and `to` (exclusive) as RFC 3339 timestamps, plus `limit` and `cursor`. Readings are ordered by `recorded_at` then id, so pages are stable under concurrent inserts. A deleted patient answers `404 PATIENT_NOT_FOUND` except to ADMIN, and its readings follow the same rule on `GET` and `DELETE /api/v1/measurements/{measurement_id}`: `404 MEASUREMENT_NOT_FOUND` for OPERATOR and USER, visible to ADMIN.

`recorded_at` is stored with microsecond precision; finer digits are dropped on input, and two readings that differ only below a microsecond are the same reading.

| Code | Status | When |
|---|---|---|
| `MEASUREMENT_VALIDATION_FAILED` | 422 | one or more fields violate the rules above; see `details` |
| `MEASUREMENT_ALREADY_EXISTS` | 409 | the same patient, type, `recorded_at` and source is already stored |
| `MEASUREMENT_NOT_FOUND` | 404 | no reading has that id, or the id is not a UUID |
| `PATIENT_NOT_FOUND` | 404 | listing readings of an unknown or invisible patient |

Every stored or deleted reading writes an audit record (`MEASUREMENT_CREATED`, `MEASUREMENT_DELETED`) in the same transaction as the change, with the acting account, the request's `request_id`, and the patient and type in its metadata. Request logs never include reading values or metadata.

## Processing

`POST /api/v1/processing/jobs` creates a job, dispatches it to the processing service and answers when it has finished, so the response carries the job in its terminal state. `GET /api/v1/processing/jobs/{job_id}` reads a job back and `GET /api/v1/patients/{patient_id}/processing-results` lists a patient's results, paginated as above.

The request selects what to process:

```json
{
  "patient_id": "…",
  "measurement_types": ["HEART_RATE"],
  "windows": ["1m", "1h"],
  "percentiles": [50, 95],
  "from": "2026-09-06T00:00:00Z",
  "to":   "2026-09-07T00:00:00Z"
}
```

- `measurement_types` and `windows` are required and must be non-empty; `windows` come from `1m`, `5m`, `15m`, `1h`, `6h`, `24h`, `7d`. Repeated entries are rejected, and order does not matter: the lists are normalised, so two requests differing only in order are the same request.
- `percentiles` are ranks between 1 and 99, at most 20 of them, and may be omitted.
- `from` and `to` bound which readings are processed, half-open and both optional. A range holding no readings is `422 PROCESSING_NO_MEASUREMENTS`; a range holding more than the configured ceiling is `422 PROCESSING_JOB_TOO_LARGE`, which names the ceiling and asks you to narrow the range.

**Job lifecycle.** A job is `PENDING` when created, `PROCESSING` while the processing service holds it, and then `COMPLETED`, `FAILED` or `CANCELLED` (SPECIFICATIONS.md section 92). The response to a successful create is `201` with a `COMPLETED` job and a `Location` header. A `COMPLETED` job always has its results: they are written in the same transaction as the transition, so a job never reports success with results missing.

**Failure.** A job that could not be processed is kept, `FAILED`, with `error_code`, `error_message`, `attempt_count` and the timestamps, so a failure never disappears (section 94). What the client receives depends on why:

| Outcome | Response | Job |
|---|---|---|
| processed | `201` | `COMPLETED` |
| nothing to process, or too much | `422` | `FAILED` |
| the processing service refused the data | `422` | `FAILED` |
| the processing service is unavailable | `503` | `FAILED` |
| the processing service was too slow | `504` | `FAILED` |
| the processing service is still working on an earlier attempt | `503 PROCESSOR_BUSY` | `FAILED` |
| not every measurement could be processed | `503 PROCESSING_INCOMPLETE` | `FAILED` |
| the two services disagree | `503 PROCESSOR_PROTOCOL_ERROR` | `FAILED` |

`PROCESSING_INCOMPLETE` means the processing service would not process every measurement in the range. Rather than report statistics that silently cover less data than was asked for, the job fails and stores nothing; narrowing `from` and `to` to a range the service accepts is the way through.

A `5xx` here is transient and worth retrying. Because a `5xx` leaves no idempotency record, the same `Idempotency-Key` can be used for the retry; it creates a new job, and the failed one stays on record. The processing service's own error messages are never repeated to you: they are written for that service's operators and can name internal detail.

**Timeouts and retries.** The call to the processing service is bounded per attempt and is retried a bounded number of times for failures worth repeating, all inside this request's own deadline. You never wait longer than the request timeout below.

## Status codes

| Status | Meaning |
|---|---|
| 200 | request succeeded, body returned |
| 201 | resource created |
| 202 | request accepted for asynchronous processing |
| 204 | request succeeded, nothing to return |
| 400 | request could not be read |
| 401 | authentication missing or invalid |
| 403 | authenticated but not permitted |
| 404 | route or resource does not exist |
| 409 | conflicts with existing state |
| 413 | body too large |
| 415 | unsupported content type |
| 422 | input understood but invalid |
| 429 | rate limit exceeded |
| 500 | unexpected server failure |
| 503 | a required dependency is unavailable |
| 504 | processing timed out |

## Pagination

Collection endpoints are cursor-paginated:

```json
{"items": [], "next_cursor": "eyJjcmVhdGVkX2F0IjoiLi4uIn0", "has_more": true}
```

- `limit` (query) selects the page size: default 50, maximum 200. Values outside `1..200` are rejected with `422`.
- `cursor` (query) continues from a previous page. Cursors are opaque, URL-safe and at most 512 characters; only values returned in `next_cursor` are valid. Tampered cursors are rejected with `INVALID_CURSOR`.
- `next_cursor` is `null` and `has_more` is `false` on the last page. `items` is always an array, never `null`.
- Ordering is by a stable keyset, so concurrent inserts never cause skipped or duplicated items.

## Request identification

Clients may send `X-Request-ID` (1–128 characters of `[A-Za-z0-9._-]`). The server keeps a valid value and generates one otherwise. The effective identifier is returned in the `X-Request-ID` response header and inside every error envelope, and appears in server logs.

Clients may also send `X-Correlation-ID`, in the same shape, to tie one client request to everything it causes across services (SPECIFICATIONS.md section 85). A request that sends none starts one, taking the request id. Both are echoed, both are logged, and both are propagated to the processing service and stored with any job the request creates, so a result can always be traced back to the call that asked for it. A malformed value is replaced rather than rejected: an unusable identifier must not turn valid work into a failure.

## Response headers

Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` and `Cache-Control: no-store`. HSTS is applied by the TLS terminator in deployed environments.

## Idempotency

Write operations that could create duplicate state accept an `Idempotency-Key` header (SPECIFICATIONS.md section 24): `POST /api/v1/patients`, `POST /api/v1/measurements`, `POST /api/v1/measurements/batch` and `POST /api/v1/processing/jobs`. The header is optional; without it every request runs.

- The key is 1–255 characters of `[A-Za-z0-9._:-]`, chosen by the client (a UUID is a good choice), and is scoped to the calling account and the operation.
- The first request under a key runs normally and its response (status and body) is stored for 24 hours by default.
- A repeat with the same body returns the stored response with the header `Idempotency-Replayed: true`. The replay is the same JSON document; byte-for-byte equality is not promised.
- A repeat with a different body is refused with `422 IDEMPOTENCY_KEY_REUSED`; the original state is untouched.
- A repeat while the first request is still running is refused with `409 IDEMPOTENCY_IN_PROGRESS` and `Retry-After`.
- Responses with status `4xx` are stored and replayed like successes: they are the final answer to that request. A `5xx` leaves no record, so the client may retry with the same key.
- Only the status and body are stored. A replay carries the current request's `X-Request-ID` header while an error body still names the original request's `request_id`, and response headers such as `Location` are not repeated.

## Timeouts

The server bounds each request's processing time (10 s by default). When the bound is exceeded the client receives `504 REQUEST_TIMEOUT` and any partial output from the handler is discarded. Idempotent requests may be retried; non-idempotent requests should be retried only with the same `Idempotency-Key` (mechanism to be specified with the write endpoints).
