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

## Response headers

Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` and `Cache-Control: no-store`. HSTS is applied by the TLS terminator in deployed environments.

## Timeouts

The server bounds each request's processing time (10 s by default). When the bound is exceeded the client receives `504 REQUEST_TIMEOUT` and any partial output from the handler is discarded. Idempotent requests may be retried; non-idempotent requests should be retried only with the same `Idempotency-Key` (mechanism to be specified with the write endpoints).
