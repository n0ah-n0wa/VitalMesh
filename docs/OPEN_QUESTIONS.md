# VitalMesh — Open Questions, Ambiguities and Contradictions

**Status:** Temporary planning document (created during the Phase 0 reconnaissance pass).
**Purpose:** Record every point where `SPECIFICATIONS.md` is silent, ambiguous, or internally inconsistent, together with the options and a recommended default. Nothing in this file is implemented. No assumption listed here has been applied silently: each item must be resolved into an Architecture Decision Record under `docs/adr/` (or, where the specification itself must change, into a deliberate specification amendment per §114) before the phase it affects begins.
**Lifecycle:** Once every item is `DECIDED` or `SPEC-CHANGE`, this file is deleted and the ADR index becomes the record.

Companion document: [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md).

## Legend

| Field | Meaning |
|---|---|
| **Impact** | `Blocking` = the affected phase cannot start until decided. `Shaping` = work can begin on the recommended default, but the decision changes design. `Minor` = documentation-level clarification. |
| **Status** | `OPEN`, `DECIDED (ADR-nnnn)`, `SPEC-CHANGE (PR #)` |
| **§** | Section numbers in `SPECIFICATIONS.md` |

## Index

| ID | Topic | Impact | Blocks phase | Status |
|---|---|---|---|---|
| OQ-01 | Processing data flow: does Rust read PostgreSQL directly? | Blocking | 1 | OPEN |
| OQ-02 | Job dispatch and queueing model (push vs pull, where the queue lives) | Blocking | 1 | OPEN |
| OQ-03 | Write ownership of shared tables between Go and Rust | Blocking | 1 | OPEN |
| OQ-04 | Authentication endpoints, users API and JWT details are unspecified | Blocking | 1 | OPEN |
| OQ-05 | RBAC permission matrix is unspecified | Blocking | 1 | OPEN |
| OQ-06 | Job cancellation has no public endpoint | Blocking | 1 | OPEN |
| OQ-07 | Job request parameters and result schema are unspecified | Blocking | 1 | OPEN |
| OQ-08 | Measurement units, canonical units and numeric ranges | Blocking | 1 | OPEN |
| OQ-09 | Patient status values, update endpoint, soft vs hard delete | Shaping | 1 | OPEN |
| OQ-10 | Idempotency storage: PostgreSQL table vs Redis metadata | Shaping | 3 | OPEN |
| OQ-11 | `/metrics` listed on the public API surface | Shaping | 2 | OPEN |
| OQ-12 | Rate limiting: anonymous identity, algorithm, Redis-down behaviour | Shaping | 3 | OPEN |
| OQ-13 | Go → Rust internal API authentication is unspecified | Shaping | 5 | OPEN |
| OQ-14 | Readiness semantics per dependency | Shaping | 2 | OPEN |
| OQ-15 | Partial failure reporting vs job status set (no PARTIAL state) | Shaping | 4 | OPEN |
| OQ-16 | Percentiles, window sizes and bounded memory | Shaping | 4 | OPEN |
| OQ-17 | Kustomize vs Helm, and the automatic rollback mechanism | Shaping | 8 | OPEN |
| OQ-18 | Ingress, TLS and DNS: is a domain available? | Shaping | 9 | OPEN |
| OQ-19 | Observability stack choice (local and in-cluster) | Shaping | 6 | OPEN |
| OQ-20 | Migration tool and migration runner | Shaping | 1 | OPEN |
| OQ-21 | Windows developer hosts: `make`, missing toolchains, CRLF | Shaping | 0 | OPEN |
| OQ-22 | Developer CLI and dataset generator: language and location | Shaping | 3 | OPEN |
| OQ-23 | Retention job execution model | Shaping | 7 | OPEN |
| OQ-24 | Coverage thresholds and performance targets have no numbers | Shaping | 2 | OPEN |
| OQ-25 | Custom-metric autoscaling needs an adapter; min replicas vs cost | Minor | 8 | OPEN |
| OQ-26 | Diagram says HTTP/gRPC, text says HTTP + JSON | Minor | 1 | OPEN |
| OQ-27 | Rust health paths: `/health` vs `/internal/v1/health` | Minor | 4 | OPEN |
| OQ-28 | Request ID vs correlation ID vs W3C trace context conventions | Shaping | 2 | OPEN |
| OQ-29 | Measurement type storage: enum, CHECK or lookup table | Shaping | 1 | OPEN |
| OQ-30 | Semantic duplicate measurements and uniqueness | Shaping | 1 | OPEN |
| OQ-31 | Window alignment and the meaning of DST tests under UTC | Shaping | 4 | OPEN |
| OQ-32 | Backup restore test requires real AWS spend | Minor | 12 | OPEN |
| OQ-33 | Terraform bootstrap and who applies infrastructure | Shaping | 9 | OPEN |
| OQ-34 | Missing list/filter endpoints (jobs list, measurement filters) | Minor | 1 | OPEN |
| OQ-35 | Batch ingestion semantics: atomic or per-item | Blocking | 1 | OPEN |
| OQ-36 | Logout with stateless JWT requires a denylist | Shaping | 2 | OPEN |
| OQ-37 | Production secret delivery into Kubernetes | Shaping | 10 | OPEN |
| OQ-38 | Benchmark memory/CPU reporting tooling | Minor | 4 | OPEN |

---

## A. Processing architecture

### OQ-01 — Processing data flow: does Rust read PostgreSQL directly?

**§:** 5, 6.1, 7, 8, 17, 19, 40, 48, 58, 83

**Ambiguity.** §6.1 assigns "database access" to the Go gateway and §7 does not list database access for Rust. Yet §40 requires trace propagation `Go Gateway → Rust Processor → Database / Redis`, §58 draws both services connected to RDS and ElastiCache, and §48 shows `Rust processing → Persist results` without naming the writer. §17 requires bounded memory and streaming, while §8 requires bounded request sizes on the internal HTTP API. These constraints pull in opposite directions if measurements travel inside the HTTP request body.

**Options.**

| | A. Payload-in-request (Rust has no database) | B. Job-reference model (Rust reads/writes PostgreSQL) |
|---|---|---|
| Flow | Go loads measurements, POSTs them to Rust, Rust returns results synchronously or via polling, Go persists results | Go creates the job row, POSTs `{job_id, parameters}` to Rust, Rust streams measurements from PostgreSQL in keyset pages, computes, writes results and terminal status |
| Bounded memory (§17) | Hard: whole dataset crosses one HTTP request or Go must chunk and Rust must merge chunks deterministically | Natural: page-by-page reads; per-window state only |
| Rust statelessness (§83) | Fully stateless | Stateless per job; state lives in PostgreSQL |
| Job durability on Rust restart (§38) | In-memory only; lost unless Go re-dispatches | Job row survives; Go reconciler re-dispatches |
| Matches §40/§58 diagrams | No | Yes |
| Coupling | Rust decoupled from schema | Rust depends on schema owned by Go migrations (see OQ-03) |
| Contract complexity | Large payload schemas | Small request schema; results schema still shared |

**Recommended default:** **Option B.** It is the only reading that satisfies §17, §40, §58 and §83 simultaneously. Mitigate the coupling with a schema-compatibility test in the Rust test suite and a single migration owner (OQ-03).

**Consequences if decided otherwise:** internal contract, Rust crate layout (no `sqlx`), Kubernetes NetworkPolicies (Rust would not need RDS egress) and the Phase 4/5 boundaries all change. Must be decided before Phase 1.

### OQ-02 — Job dispatch and queueing model

**§:** 5 (Redis "Job metadata"), 8, 17, 18, 38, 41 ("processing queue depth"), 91

**Ambiguity.** §8 mandates a push endpoint `POST /internal/v1/process`, §91 requires "bounded queues or equivalent flow control" and forbids unbounded in-memory queues, §38 requires in-flight work to be cancelled or requeued on shutdown, and §8 forbids depending on a message broker. Where the queue lives and who retries is not stated.

**Options.**
1. **Push + bounded admission + reconciler.** Go POSTs to Rust immediately after creating the job. Rust holds a bounded admission gate (`MAX_CONCURRENT_JOBS` semaphore plus a small bounded wait queue) and answers `429`/`503` with `Retry-After` when full. Go leaves the job `PENDING` and a Go-side reconciler (idempotent across replicas via `SELECT ... FOR UPDATE SKIP LOCKED`) re-dispatches `PENDING` jobs and jobs whose `PROCESSING` lease expired.
2. **Pull (PostgreSQL as queue).** Rust workers poll `processing_jobs` with `SKIP LOCKED`. Simple and durable, but contradicts the mandated push endpoint and makes `POST /internal/v1/process` redundant.
3. **Redis list/stream as queue.** Contradicts §23 ("Redis must never become the system of record") for job state unless mirrored in PostgreSQL, and edges toward broker dependence.

**Recommended default:** **Option 1.** It honours the mandated endpoint, keeps PostgreSQL as the durable job record, provides the "reject / defer / explicitly queue" behaviours of §91, and gives a real "processing queue depth" metric (Rust wait-queue length plus `PENDING` count in Go). The dispatcher is hidden behind a `JobDispatcher` interface so an asynchronous transport can be added later (§8).

### OQ-03 — Write ownership of shared tables

**§:** 19, 22, 64, 68, 92

**Ambiguity.** If OQ-01 resolves to Option B, two services write the same database. The specification places migrations only under `services/api-gateway/migrations/`.

**Recommended default.**
- Migrations have exactly one owner: the Go gateway repository path. Rust never runs migrations.
- Go owns all writes to `users`, `patients`, `measurements`, `audit_logs`, `idempotency_keys`, and creates `processing_jobs` rows (`PENDING`) and requests cancellation.
- Rust owns writes to `processing_results` and the transitions `PENDING → PROCESSING`, `PROCESSING → COMPLETED | FAILED | CANCELLED`, each performed as a compare-and-set `UPDATE ... WHERE status = $expected` (§92).
- Rust connects with a dedicated least-privilege database role (`processor`) granted `SELECT` on `measurements`, `patients`, `processing_jobs` and `INSERT` on `processing_results`, `UPDATE` on `processing_jobs` only.
- A Rust integration test asserts the columns it depends on exist with the expected types (schema-compatibility contract), and CI runs it against the migrated database.

### OQ-15 — Partial failure reporting vs job status set

**§:** 13, 17, 92, 94

**Ambiguity.** §17 requires "partial failure reporting" but the status set has no `PARTIAL` state and §92 lists no transition to one.

**Recommended default:** a job is `COMPLETED` when at least one result was produced; per-window or per-type problems are recorded in a `warnings` array on the job (JSONB, bounded size) and in results metadata. A job is `FAILED` only when no result can be produced. Never add a status outside §13 without a specification amendment.

### OQ-16 — Percentiles, window sizes and bounded memory

**§:** 15, 16, 17, 18, 96

**Ambiguity.** Exact percentiles over a 7-day window require holding that window's values; "bounded memory", "configurable percentiles" and determinism must all hold.

**Recommended default:** exact percentiles (nearest-rank, documented) computed over tumbling windows, with a hard per-job ceiling (`MAX_JOB_MEASUREMENTS`, distinct from `MAX_BATCH_SIZE` which bounds a single page read) beyond which the job fails fast with `JOB_TOO_LARGE`. Variance via Welford's algorithm in a fixed sequential order; sums via Kahan compensation. Floating-point behaviour documented in `PERFORMANCE.md` and `ARCHITECTURE.md`. Approximate sketches (t-digest) are deferred until a benchmark shows the ceiling is a real limitation.

### OQ-31 — Window alignment and DST tests under UTC

**§:** 15, 86, 87

**Ambiguity.** The engine is UTC-only, so "daylight saving transitions" cannot affect it; and tumbling vs sliding windows and their alignment (epoch-aligned vs relative to the first sample) are unspecified.

**Recommended default:** tumbling windows aligned to UTC epoch boundaries (`window_start = floor(ts / width) * width`); rolling averages and moving standard deviation are trailing per-sample windows. DST tests verify that RFC 3339 inputs carrying offsets are normalised to UTC correctly across a DST transition and a leap day and that window boundaries do not shift. Both are documented as test fixtures.

### OQ-38 — Benchmark memory/CPU reporting tooling

**§:** 50, 97

**Ambiguity.** `criterion` reports latency/throughput but not memory or CPU.

**Recommended default:** `criterion` for latency/throughput; a small benchmark harness binary reporting peak RSS and CPU time via `/usr/bin/time -v` (Linux container) and, for end-to-end numbers, Prometheus queries against the container during k6 runs. Hardware and versions recorded per §97.

## B. Public API surface

### OQ-04 — Authentication endpoints, users API and JWT details

**§:** 9.1, 19 (`users` table), 30 ("secure password hashing"), 43 (`LOGIN`, `LOGOUT`, `ROLE_CHANGED`), 108 ("Create synthetic user → Authenticate")

**Ambiguity.** JWT authentication is mandated and password hashing is required, but no login, logout, or user-management endpoint appears in §10–13. JWT algorithm, lifetime, refresh strategy, and the bootstrap of the first administrator are unspecified.

**Recommended default.**
- Add to the public contract: `POST /api/v1/auth/login`, `POST /api/v1/auth/logout`, `GET /api/v1/auth/me`, `POST /api/v1/users` (ADMIN), `GET /api/v1/users`, `GET /api/v1/users/{user_id}`, `PATCH /api/v1/users/{user_id}/role` (ADMIN, emits `ROLE_CHANGED`).
- Access token: JWT, `HS256` with a `kid` header to allow secret rotation, 15-minute lifetime, claims `sub`, `role`, `iat`, `exp`, `jti`. No refresh tokens in v1 (re-authenticate). Revisit asymmetric keys only if a second verifier appears.
- Password hashing: Argon2id with parameters recorded in `SECURITY.md`.
- First administrator: created by the CLI (`vmctl users bootstrap-admin`) reading the password from an environment variable or prompt; never a hard-coded default. Local compose seeds one via the same CLI.

### OQ-05 — RBAC permission matrix

**§:** 9.1, 30, 82

**Ambiguity.** Three roles exist but no permissions are attached. There is no patient-to-user ownership field, so "USER sees own data" cannot be expressed.

**Recommended default (to be confirmed):**

| Capability | ADMIN | OPERATOR | USER |
|---|---|---|---|
| Manage users and roles | yes | no | no |
| Create / delete patients | yes | yes | no |
| Read patients | yes | yes | yes |
| Create / delete measurements | yes | yes | no |
| Read measurements | yes | yes | yes |
| Create / cancel processing jobs | yes | yes | no |
| Read jobs and results | yes | yes | yes |
| Run retention / operational commands | yes | no | no |

Authorization is enforced in middleware per route from a single declarative table, tested exhaustively (role × route).

### OQ-06 — Job cancellation has no public endpoint

**§:** 13, 17, 43 (`PROCESSING_JOB_CANCELLED`), 92

**Recommended default:** add `POST /api/v1/processing/jobs/{job_id}/cancel` (returns `202` with the job; `409` if already terminal) and internal `POST /internal/v1/jobs/{job_id}/cancel`. Cancellation is cooperative in Rust via a cancellation token; the terminal state is resolved by compare-and-set so a job that completes concurrently stays `COMPLETED`.

### OQ-07 — Job request parameters and result schema

**§:** 13, 14, 15, 16, 19, 95, 96

**Ambiguity.** §13 lists job fields without any parameters, but §14 requires reproducibility under "identical algorithm configuration". The shape of `processing_results` and of statistical (non-anomaly) results is unspecified; only anomaly result fields are listed in §16. §94 requires `attempt count`, `service version`, `algorithm version`, `trace ID` on failed jobs, which are absent from §13's field list.

**Recommended default.**
- `processing_jobs` gains `parameters` (JSONB: `measurement_types[]`, `from`, `to`, `windows[]`, `percentiles[]`, `anomaly_rules`, `algorithm_version`), `attempt_count`, `service_version`, `algorithm_version`, `request_id`, `trace_id`, `lease_expires_at`, `warnings` (JSONB). §13 is treated as a minimum field list.
- `processing_results`: one row per `(job_id, measurement_type, window, window_start)` with `statistics` (JSONB: count, min, max, mean, median, variance, stddev, percentiles map), `anomalies` (JSONB array of §16 records), `algorithm_version`, `service_version`, `created_at`. Indexed by `job_id` and by `(patient_id, measurement_type, window, window_start)` for the results-by-patient endpoint.
- Defaults for omitted parameters are server-side constants recorded into `parameters` at creation time so the stored job is self-describing.

### OQ-08 — Measurement units, canonical units and numeric ranges

**§:** 12, 14 (normalization), 88

**Ambiguity.** "Valid unit" and "numeric constraints" per type are not defined. Whether normalization converts units (°F → °C, mmol/L → mg/dL) is unspecified.

**Recommended default:** one canonical unit per type, rejected otherwise in v1 (`422 MEASUREMENT_VALIDATION_FAILED`), with technical plausibility bounds that are explicitly documented as data-validation limits, not medical thresholds:

| Type | Canonical unit | Technical bounds (inclusive) |
|---|---|---|
| HEART_RATE | `bpm` | 0 – 300 |
| BLOOD_PRESSURE_SYSTOLIC | `mmHg` | 0 – 300 |
| BLOOD_PRESSURE_DIASTOLIC | `mmHg` | 0 – 200 |
| SPO2 | `%` | 0 – 100 |
| BODY_TEMPERATURE | `C` | 20 – 45 |
| BLOOD_GLUCOSE | `mg/dL` | 0 – 1000 |
| RESPIRATORY_RATE | `breaths/min` | 0 – 100 |

Stored in a `measurement_types` lookup table (OQ-29) so the database enforces both type and unit. Unit conversion is a possible later `normalization` feature behind an explicit algorithm version bump.

### OQ-09 — Patient status values, update endpoint, delete semantics

**§:** 11, 43, 81

**Recommended default:** `status ∈ {ACTIVE, INACTIVE, DELETED}`; `DELETE /api/v1/patients/{id}` performs a soft delete (`status = DELETED`, `deleted_at`), keeping jobs and results referentially valid; `GET` of a deleted patient returns `404` for non-admins and the record with status for ADMIN. No update endpoint in v1 (matches the spec's list); `updated_at` changes only through status changes. Measurement `DELETE` is a hard delete recorded in `audit_logs`.

### OQ-11 — `/metrics` listed on the public API surface

**§:** 10, 30, 41

**Ambiguity.** §10 lists `GET /metrics` alongside public endpoints. Exposing Prometheus metrics to the internet is a security and cardinality risk.

**Recommended default:** serve `/health` and `/ready` unauthenticated on the main listener; serve `/metrics` on a separate admin listener (e.g. `:9090`) that is never routed by the Ingress and is only reachable from the cluster network (NetworkPolicy allows the Prometheus namespace). Locally both ports are published. This satisfies the literal path requirement while not making metrics public.

### OQ-34 — Missing list/filter endpoints

**§:** 12, 13, 107

**Recommended default:** add `GET /api/v1/processing/jobs?patient_id=&status=&cursor=&limit=` and support `type`, `from`, `to`, `cursor`, `limit` on `GET /api/v1/patients/{id}/measurements`. Both are additive and needed by the CLI and operations.

### OQ-35 — Batch ingestion semantics

**§:** 12 (`POST /api/v1/measurements/batch`), 22, 24, 89

**Ambiguity.** Whether a batch is atomic (all-or-nothing in one transaction) or per-item (accepted items persisted, rejected items reported) is not stated. This changes the response schema, the idempotency record, and the transaction shape.

**Recommended default:** all-or-nothing per batch in a single short transaction, `MAX_BATCH_SIZE` enforced early (§89), response `201` with created ids or `422` with per-item error positions. Per-item acceptance can be added later as an explicit `mode=partial` query parameter without breaking the contract. Must be decided before the public contract is frozen.

## C. Gateway cross-cutting behaviour

### OQ-10 — Idempotency storage: PostgreSQL table vs Redis metadata

**§:** 19 (`idempotency_keys` table), 23 (Redis "idempotency metadata"), 24, 90

**Ambiguity.** Both stores are named. Correctness cannot depend on Redis (§23, §90).

**Recommended default:** PostgreSQL `idempotency_keys` is the record of truth: `(user_id, method, path, key)` unique, `request_fingerprint` (SHA-256 of canonical body), `status` (`IN_PROGRESS`/`COMPLETED`), `response_status`, `response_body`, `expires_at` (24 h). Redis holds an optional short-lived lock (`SET NX PX`) to fail fast on concurrent replays; when Redis is unavailable the PostgreSQL unique constraint alone serialises replays. Fingerprint mismatch → `422 IDEMPOTENCY_KEY_REUSED`; concurrent in-progress → `409`. Expiry by the retention job (OQ-23).

### OQ-12 — Rate limiting details

**§:** 29, 23, 90

**Ambiguity.** "anonymous" identity (all documented endpoints require a JWT except health and, after OQ-04, login), trusted proxy depth for client IP, algorithm, response headers, and Redis-down behaviour are unspecified.

**Recommended default:** sliding-window counter implemented as a Redis Lua script (atomic); key = `sub` for authenticated requests, client IP for anonymous requests (login, health) with `TRUSTED_PROXY_HOPS` config for `X-Forwarded-For`; per-role limits from config; headers `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`, and `Retry-After` on `429`; Redis unavailable → fall back to a per-replica in-memory limiter with the same limits (degraded, logged once, `rate_limit_backend="local"` metric label) rather than failing open entirely.

### OQ-13 — Go → Rust internal API authentication

**§:** 8, 30, 33 (NetworkPolicy)

**Recommended default:** NetworkPolicy restricts the processor Service to gateway pods, plus a static internal bearer token from a Kubernetes Secret checked by the processor (`INTERNAL_API_TOKEN`). mTLS / service mesh is out of scope and would need justification under §71.

### OQ-14 — Readiness semantics per dependency

**§:** 35, 37, 90

**Recommended default:** liveness never checks dependencies. Gateway readiness = PostgreSQL reachable (Redis and Rust unavailability are degraded modes and do not fail readiness). Processor readiness = PostgreSQL reachable (given OQ-01 B) and admission gate not permanently wedged. Rust unavailability is surfaced to clients as `503 PROCESSING_UNAVAILABLE` on job creation only if the reconciler is also disabled; otherwise the job is accepted (`202`) as `PENDING`.

### OQ-28 — Request ID, correlation ID and trace context conventions

**§:** 8, 39, 42, 85

**Recommended default:** `X-Request-ID` is per inbound request, server-generated unless the client supplies a well-formed value (≤ 128 chars, `[A-Za-z0-9._-]`); `X-Correlation-ID` is end-to-end and defaults to the request ID when absent; `traceparent`/`tracestate` follow W3C. Go forwards all three to Rust; Rust generates its own request ID for its hop and logs all of them.

### OQ-36 — Logout with stateless JWT requires a denylist

**§:** 9.1, 23, 43, 90

**Recommended default:** `POST /auth/logout` stores `jti` in Redis with TTL = remaining token lifetime and emits `LOGOUT`. When Redis is unavailable the denylist check fails open; the 15-minute token lifetime (OQ-04) bounds the exposure. Documented explicitly in `SECURITY.md` as a known trade-off.

## D. Data model

### OQ-20 — Migration tool and runner

**§:** 47, 64, 68

**Recommended default:** `golang-migrate` with plain SQL up/down files under `services/api-gateway/migrations/` (advisory-lock based locking satisfies §64). Runner: `api-gateway migrate up` subcommand of the gateway binary, executed as a Kubernetes `Job` before the rollout in CD and as a one-shot service in compose. Rollback strategy: down files exist for every migration, and destructive changes follow expand/contract across two releases.

### OQ-29 — Measurement type storage

**§:** 20, 21

**Recommended default:** a `measurement_types` lookup table (`code PK`, `canonical_unit`, `min_value`, `max_value`) referenced by `measurements.type` with a foreign key, seeded by migration. Adding a type is a data migration, not an `ALTER TYPE`. Job and patient status values use `CHECK` constraints.

### OQ-30 — Semantic duplicate measurements

**§:** 12, 20, 24, 37

**Ambiguity.** The idempotency key protects against replayed requests but not against the same reading submitted twice under different keys.

**Recommended default:** unique constraint on `(patient_id, type, recorded_at, source)`; a duplicate returns `409 MEASUREMENT_ALREADY_EXISTS` with the existing id. Batches containing an internal duplicate are rejected as a whole (OQ-35).

## E. Platform and delivery

### OQ-17 — Kustomize vs Helm, and automatic rollback

**§:** 62, 101

**Recommended default:** Kustomize (`base/` + `overlays/{local,staging,production}`) for the application; Helm only for third-party charts (observability, AWS Load Balancer Controller). Automatic rollback = CD runs `kubectl rollout status --timeout` and on failure `kubectl rollout undo` plus the smoke test, all recorded in the job summary. Argo Rollouts is not introduced without evidence of need.

### OQ-18 — Ingress, TLS and DNS

**§:** 33, 57, 59, 104

**Ambiguity.** "DNS/TLS infrastructure where applicable" versus "TLS in deployed environments" (§30) and "use HTTPS" (§104). Whether a registered domain exists for the project is unknown.

**Recommended default:** AWS Load Balancer Controller (ALB Ingress) with ACM. Terraform takes an optional `hosted_zone_id`; when set, an ACM certificate is validated via Route 53; when not set, a self-signed certificate is imported into ACM so the ALB still terminates TLS. Clients are documented to trust that certificate in staging only.

### OQ-19 — Observability stack choice

**§:** 4, 39–42, 53, 57, 109

**Recommended default.** Local (compose profile `observability`): OpenTelemetry Collector, Prometheus, Grafana with provisioned dashboards, Jaeger all-in-one for traces; logs read via `docker compose logs` with optional Loki/Promtail. Cluster: `kube-prometheus-stack` (Helm), OpenTelemetry Collector, Tempo or Jaeger for traces, Fluent Bit shipping JSON logs to CloudWatch Logs. AWS Managed Prometheus/Grafana are optional cost trade-offs documented in §105 material.

### OQ-21 — Windows developer hosts

**§:** 53, 54, 111

**Findings on the current host:** Docker present; Go, Rust, `make`, `kind`, Terraform, k6, Trivy, Gitleaks absent; `core.autocrlf=true` with `* text=auto`.

**Recommended default:** keep `Makefile` as the canonical interface; document Windows setup (Git Bash + GNU make via `winget`, or WSL2); add a Dev Container definition so `make verify` is reproducible without host toolchains; extend `.gitattributes` with `eol=lf` for `*.sh`, `Makefile`, `Dockerfile*`, `*.yml`, `*.yaml`, `*.tf`, `*.sql`, `*.toml` to prevent CRLF corruption in containers.

### OQ-22 — Developer CLI and dataset generator

**§:** 68, 106, 107

**Recommended default:** a single Go binary `vmctl` at `services/api-gateway/cmd/vmctl/` sharing the module's validation rules and the generated public API client. The generator is a package (`internal/synthetic`) reused by tests, load tests and the CLI. No `tools/` directory is added unless the module boundary becomes awkward.

### OQ-23 — Retention job execution model

**§:** 81, 43

**Recommended default:** `api-gateway retention run` subcommand executed by a Kubernetes `CronJob` (and `make retention` locally); deletes measurements older than `MEASUREMENT_RETENTION_DAYS` in bounded batches, expires `idempotency_keys`, emits `RETENTION_RUN` audit records and Prometheus metrics. Results and jobs are retained separately (`RESULT_RETENTION_DAYS`).

### OQ-24 — Coverage thresholds and performance targets

**§:** 44–46, 50, 60

**Recommended default:** starting gates of 80 % statement coverage for `services/api-gateway/internal/...` (generated code excluded) and 85 % line coverage for the Rust engine crate, enforced in CI and only ever ratcheted upward. Performance targets are recorded only after Phase 11 benchmarks exist and are always stated with hardware context (§50, §97).

### OQ-25 — Custom-metric autoscaling and minimum replicas vs cost

**§:** 36, 105

**Recommended default:** CPU/memory HPA in v1; custom Prometheus metrics via Prometheus Adapter only if Phase 11 shows CPU is a poor signal. `minReplicas`: production 2, staging 1 (documented deviation for cost), local 1.

### OQ-32 — Backup restore test requires real AWS spend

**§:** 80, 105

**Recommended default:** performed once against the staging RDS instance during Phase 12 (snapshot restore to a temporary instance, verification query, destroy), recorded in `OPERATIONS.md` with date, duration and cost.

### OQ-33 — Terraform bootstrap and who applies

**§:** 57, 59, 63, 100, 111

**Recommended default:** `infrastructure/terraform/bootstrap/` (state bucket, lock table, GitHub OIDC provider, CI roles) applied once by a human with admin credentials; environment stacks are `plan`-ed in CI on pull requests through a read-only OIDC role and `apply`-ed via `workflow_dispatch` gated by a GitHub environment, with the human `terraform apply` path from §111 kept fully documented.

### OQ-37 — Production secret delivery into Kubernetes

**§:** 31, 59, 104

**Recommended default:** Terraform creates secrets in AWS Secrets Manager; the CD job reads them through OIDC and applies Kubernetes `Secret` objects with output masked. External Secrets Operator is an optional later improvement, not introduced in v1.

## F. Minor inconsistencies (no specification change required)

### OQ-26 — Diagram says "HTTP/gRPC", text says "HTTP + JSON"

**§:** 5 vs 8 and 113. **Resolution:** §8 and §113 are normative; the diagram label lists possibilities. HTTP/JSON is implemented; gRPC is not planned. A `JobDispatcher` abstraction keeps transport swappable.

### OQ-27 — Rust health paths

**§:** 8 (`/internal/v1/health`) vs 35 (`/health`, `/ready`). **Resolution:** Rust exposes `/health`, `/ready` for probes and `/internal/v1/health` as the versioned dependency check used by Go; they share one handler.
