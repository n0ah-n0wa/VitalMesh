# VitalMesh — System Specifications

**Project:** VitalMesh  
**Document:** `SPECIFICATIONS.md`  
**Version:** 1.0  
**Status:** Authoritative Engineering Specification  
**Audience:** AI coding agents, software engineers, reviewers  
**Primary Development Tools:** Cursor, Claude Code  
**Target:** Production-grade portfolio/reference implementation

---

# 1. Project Overview

## 1.1 Purpose

VitalMesh is a cloud-native, production-oriented e-health data processing platform designed to demonstrate modern backend engineering, systems programming, distributed systems, cloud infrastructure, observability, security, automated testing, and CI/CD.

The platform accepts synthetic health measurements, validates and persists them, submits processing jobs, performs high-throughput statistical and time-series processing using Rust, and exposes the resulting data through a Go API gateway.

VitalMesh is explicitly designed as a **technical portfolio and educational project**.

It is **not a medical device, clinical decision-support system, diagnostic system, treatment recommendation system, or production healthcare product**.

All patient and measurement data used by the system must be synthetic.

---

# 2. Primary Engineering Goals

The system must demonstrate proficiency in:

- Rust systems programming
- Go backend development
- REST API design
- asynchronous processing
- distributed service architecture
- PostgreSQL database engineering
- Docker containerization
- Kubernetes orchestration
- AWS cloud infrastructure
- infrastructure as code
- GitHub Actions CI/CD
- authentication and authorization
- secure secret management
- observability
- distributed tracing
- metrics
- structured logging
- automated testing
- performance testing
- resilience engineering
- graceful degradation
- horizontal scalability
- production-oriented operational practices

The implementation must prioritize:

1. correctness;
2. maintainability;
3. explicit contracts;
4. security;
5. observability;
6. testability;
7. reproducibility;
8. performance;
9. operational simplicity.

Premature optimization must not compromise correctness or maintainability.

---

# 3. Non-Goals

VitalMesh must NOT implement:

- real patient data;
- real medical records;
- clinical diagnosis;
- medical treatment recommendations;
- drug recommendations;
- emergency response functionality;
- medical device integrations;
- real-world hospital integrations;
- insurance processing;
- billing;
- real clinical decision making.

The application may calculate statistical anomalies or threshold violations, but these results must always be treated as **technical data-processing results**, not medical diagnoses.

---

# 4. High-Level Architecture

VitalMesh consists of two primary application services:

1. **Go API Gateway**
2. **Rust Data Processing Service**

Supporting infrastructure:

- PostgreSQL
- Redis
- Kubernetes
- AWS EKS
- AWS ECR
- AWS RDS PostgreSQL
- AWS ElastiCache Redis
- OpenTelemetry
- Prometheus
- Grafana
- centralized/structured logging
- GitHub Actions

Optional infrastructure components may be introduced only when they provide a clear architectural or operational benefit.

---

# 5. Logical Architecture

```text
                         ┌─────────────────────┐
                         │       Clients       │
                         │ Web / CLI / Mobile  │
                         └──────────┬──────────┘
                                    │
                                  HTTPS
                                    │
                                    ▼
                         ┌─────────────────────┐
                         │   Go API Gateway    │
                         │                     │
                         │ REST API            │
                         │ Authentication      │
                         │ Authorization       │
                         │ Validation          │
                         │ Rate Limiting       │
                         │ API Documentation    │
                         │ Observability       │
                         └──────┬───────┬──────┘
                                │       │
                           HTTP/gRPC     │
                                │       │
                                ▼       ▼
                    ┌────────────────┐ ┌───────────────┐
                    │ Rust Processor  │ │ PostgreSQL    │
                    │                │ │               │
                    │ Validation     │ │ Patients      │
                    │ Normalization   │ │ Measurements  │
                    │ Aggregation     │ │ Jobs          │
                    │ Statistics      │ │ Results       │
                    │ Anomaly Scoring │ │ Audit Logs    │
                    └───────┬────────┘ └───────────────┘
                            │
                            ▼
                         Results

                         ┌───────────────┐
                         │ Redis         │
                         │               │
                         │ Rate limits   │
                         │ Cache         │
                         │ Job metadata  │
                         └───────────────┘
```

---

# 6. Service Responsibilities

## 6.1 Go API Gateway

The Go service is the public application boundary.

Responsibilities:

- HTTP REST API;
- request authentication;
- authorization;
- request validation;
- API versioning;
- rate limiting;
- idempotency handling;
- database access;
- processing job creation;
- communication with Rust;
- response serialization;
- error handling;
- OpenAPI documentation;
- health endpoints;
- metrics;
- tracing;
- structured logging;
- audit events.

The Go service must not contain complex numerical processing logic that belongs to the Rust processor.

---

# 7. Rust Data Processing Service

The Rust service is responsible for computationally intensive data processing.

Responsibilities:

- receiving processing jobs;
- validating processing payloads;
- normalization;
- time-series aggregation;
- statistical calculations;
- rolling-window calculations;
- percentile calculations;
- anomaly scoring;
- batch processing;
- concurrent processing;
- deterministic result generation;
- processing metrics;
- tracing;
- structured logging.

The Rust service must be designed to support horizontal scaling.

---

# 8. Inter-Service Communication

The initial synchronous communication protocol must be:

**HTTP + JSON**

The internal API must be versioned independently from the public API.

Example:

```text
POST /internal/v1/process
GET  /internal/v1/jobs/{job_id}
GET  /internal/v1/health
```

The implementation must use:

- request IDs;
- correlation IDs;
- explicit timeouts;
- bounded request sizes;
- structured errors;
- deterministic response schemas.

Future asynchronous transport may be introduced behind an explicit abstraction.

The architecture must not become dependent on a specific message broker.

---

# 9. API Design

Public API prefix:

```text
/api/v1
```

## 9.1 Authentication

The API must use JWT-based authentication.

Tokens must contain:

- subject;
- role;
- issued-at timestamp;
- expiration;
- unique token identifier where appropriate.

Supported roles:

```text
ADMIN
OPERATOR
USER
```

Authorization must be enforced server-side.

---

# 10. Public API

## 10.1 Health

```http
GET /health
```

Returns application liveness information.

```http
GET /ready
```

Returns readiness state.

```http
GET /metrics
```

Returns Prometheus metrics.

---

# 11. Patient API

```http
POST /api/v1/patients
GET /api/v1/patients/{patient_id}
GET /api/v1/patients
DELETE /api/v1/patients/{patient_id}
```

Patients are synthetic entities.

Patient fields:

```text
id
external_reference
date_of_birth
sex
created_at
updated_at
status
```

The API must never expose internal database implementation details.

---

# 12. Measurement API

```http
POST /api/v1/measurements
POST /api/v1/measurements/batch

GET /api/v1/patients/{patient_id}/measurements
GET /api/v1/measurements/{measurement_id}

DELETE /api/v1/measurements/{measurement_id}
```

Supported measurement types must initially include:

```text
HEART_RATE
BLOOD_PRESSURE_SYSTOLIC
BLOOD_PRESSURE_DIASTOLIC
SPO2
BODY_TEMPERATURE
BLOOD_GLUCOSE
RESPIRATORY_RATE
```

Each measurement must contain:

```text
id
patient_id
type
value
unit
recorded_at
created_at
source
metadata
```

Validation must include:

- valid measurement type;
- valid unit;
- numeric constraints;
- timestamp validation;
- payload size limits;
- patient existence;
- malformed metadata rejection.

---

# 13. Processing API

```http
POST /api/v1/processing/jobs

GET /api/v1/processing/jobs/{job_id}

GET /api/v1/patients/{patient_id}/processing-results
```

Processing jobs must support:

```text
PENDING
PROCESSING
COMPLETED
FAILED
CANCELLED
```

A job must have:

```text
id
patient_id
status
requested_at
started_at
completed_at
failed_at
error_code
error_message
created_by
```

---

# 14. Processing Model

The processing pipeline must support:

```text
Raw Measurements
        │
        ▼
Validation
        │
        ▼
Normalization
        │
        ▼
Time Ordering
        │
        ▼
Window Aggregation
        │
        ▼
Statistical Analysis
        │
        ▼
Anomaly Detection
        │
        ▼
Result Generation
```

Processing must be deterministic.

Given identical input data and identical algorithm configuration, the same processing result must be produced.

---

# 15. Statistical Processing

The Rust service must support:

- count;
- minimum;
- maximum;
- mean;
- median;
- variance;
- standard deviation;
- configurable percentiles;
- rolling averages;
- moving standard deviation;
- time-window aggregation.

Default windows:

```text
1 minute
5 minutes
15 minutes
1 hour
6 hours
24 hours
7 days
```

The processing engine must use UTC internally.

---

# 16. Anomaly Detection

The initial anomaly engine must support configurable rules.

Examples:

### Threshold rule

```text
value < lower_bound
value > upper_bound
```

### Z-score rule

```text
abs(z_score) > configured_threshold
```

### Rolling deviation rule

```text
abs(value - rolling_mean) >
    threshold * rolling_standard_deviation
```

Results must contain:

```text
measurement_type
window
metric
value
threshold
severity
detected_at
algorithm_version
```

Severity:

```text
INFO
WARNING
CRITICAL
```

These labels are technical anomaly severity classifications only and must not imply medical urgency.

---

# 17. Batch Processing

The platform must support processing large measurement batches.

Requirements:

- bounded memory usage;
- streaming where practical;
- backpressure;
- configurable batch size;
- cancellation support;
- timeout handling;
- partial failure reporting;
- processing metrics.

The Rust implementation must avoid loading unbounded datasets into memory.

---

# 18. Concurrency

The Rust processor must support concurrent processing of independent jobs.

Concurrency must be bounded.

The implementation must provide configuration for:

```text
MAX_CONCURRENT_JOBS
MAX_BATCH_SIZE
PROCESSING_TIMEOUT
```

Unbounded task spawning is prohibited.

---

# 19. PostgreSQL

PostgreSQL is the system of record.

Required tables:

```text
users
patients
measurements
processing_jobs
processing_results
audit_logs
idempotency_keys
```

The schema must use:

- UUID or equivalent globally unique identifiers;
- UTC timestamps;
- foreign keys;
- appropriate indexes;
- explicit constraints;
- transactional boundaries.

---

# 20. Database Constraints

The database must enforce important invariants wherever practical.

Examples:

- patient must exist before measurement;
- job must reference a valid patient;
- result must reference a valid job;
- status values must be constrained;
- required timestamps cannot be NULL;
- measurement types must be valid;
- uniqueness constraints must exist where required.

Application validation must not be considered a replacement for database integrity.

---

# 21. Indexing

Indexes must exist for common access paths.

At minimum:

```text
measurements(patient_id, recorded_at)
measurements(patient_id, type, recorded_at)
processing_jobs(patient_id, created_at)
processing_jobs(status, created_at)
processing_results(job_id)
audit_logs(resource_id, created_at)
```

Index decisions must be validated using realistic query plans.

---

# 22. Transactions

Transactions must be used whenever multiple state changes must be atomic.

Examples:

- creating a processing job;
- recording audit information;
- idempotent measurement ingestion;
- state transitions.

Transactions must be kept short.

External service calls must not be performed while holding database transactions unless explicitly justified.

---

# 23. Redis

Redis is used for:

- API rate limiting;
- short-lived cache;
- idempotency metadata;
- distributed coordination where required.

Redis must never become the system of record.

The application must remain recoverable if Redis is unavailable.

---

# 24. Idempotency

Write endpoints must support idempotency where duplicate requests could create duplicate state.

The client may send:

```http
Idempotency-Key: <unique-key>
```

The server must:

1. validate the key;
2. detect an existing request;
3. return the original result when appropriate;
4. prevent conflicting reuse;
5. expire old idempotency records.

---

# 25. Error Handling

The public API must use consistent error responses.

Example:

```json
{
  "error": {
    "code": "MEASUREMENT_VALIDATION_FAILED",
    "message": "The measurement payload is invalid.",
    "request_id": "..."
  }
}
```

Internal implementation details must never leak through API errors.

Stack traces must never be returned to clients.

---

# 26. HTTP Status Codes

The implementation must use conventional status codes.

Examples:

```text
200 OK
201 Created
202 Accepted
204 No Content

400 Bad Request
401 Unauthorized
403 Forbidden
404 Not Found
409 Conflict
422 Unprocessable Entity
429 Too Many Requests

500 Internal Server Error
502 Bad Gateway
503 Service Unavailable
504 Gateway Timeout
```

---

# 27. API Documentation

The public API must be described using OpenAPI.

Requirements:

- complete schemas;
- request examples;
- response examples;
- authentication documentation;
- error responses;
- pagination;
- idempotency;
- versioning.

OpenAPI specification must be generated or validated automatically in CI.

---

# 28. Pagination

Collection endpoints must use cursor-based pagination where appropriate.

Response:

```json
{
  "items": [],
  "next_cursor": "...",
  "has_more": true
}
```

Pagination must be stable under concurrent inserts.

---

# 29. Rate Limiting

The Go gateway must implement distributed rate limiting using Redis.

Limits must be configurable.

Example defaults:

```text
anonymous: 60 requests/minute
authenticated: 300 requests/minute
admin: 1000 requests/minute
```

Limits must not be hard-coded.

Responses must include appropriate rate-limit metadata.

---

# 30. Security

Security is a first-class requirement.

The implementation must include:

- secure password hashing;
- JWT authentication;
- RBAC;
- input validation;
- output encoding;
- rate limiting;
- request size limits;
- secure headers;
- TLS in deployed environments;
- secret management;
- dependency scanning;
- container scanning;
- least-privilege IAM;
- Kubernetes security contexts;
- NetworkPolicies;
- audit logging.

---

# 31. Secrets

Secrets must never be committed to Git.

Forbidden:

```text
passwords
JWT secrets
AWS credentials
database credentials
private keys
API tokens
```

Local development must use environment variables or local secret files excluded by Git.

Production secrets must use AWS/Kubernetes secret management.

---

# 32. Containerization

Each service must have a production-grade Dockerfile.

Requirements:

- multi-stage builds;
- minimal runtime image;
- non-root execution;
- deterministic dependency installation;
- health checks where appropriate;
- no unnecessary packages;
- no secrets in images.

Images must be tagged using immutable identifiers.

Recommended:

```text
service:<git-sha>
```

Mutable tags such as `latest` must not be used for production deployments.

---

# 33. Kubernetes

The application must be deployable to Kubernetes without manual modification of application source code.

Required resources:

```text
Namespace
Deployment
Service
ConfigMap
Secret
HorizontalPodAutoscaler
PodDisruptionBudget
NetworkPolicy
ServiceAccount
Role
RoleBinding
```

Ingress/API gateway infrastructure must be included where required by the AWS deployment.

---

# 34. Kubernetes Security

Pods must:

- run as non-root;
- use read-only root filesystem where practical;
- drop unnecessary Linux capabilities;
- use explicit resource limits;
- use security contexts;
- avoid privileged mode.

Service accounts must follow least privilege.

---

# 35. Kubernetes Health Checks

Every service must expose:

```text
/health
/ready
```

Liveness checks must verify process health.

Readiness checks must verify required dependencies.

A temporary PostgreSQL or Redis failure must not cause unnecessary pod restarts.

---

# 36. Autoscaling

The Rust processing service must support horizontal autoscaling.

Scaling signals may include:

- CPU;
- memory;
- custom Prometheus metrics.

The target architecture must support:

```text
min replicas: 2
max replicas: configurable
```

The exact values must be environment-specific.

---

# 37. Resilience

The system must tolerate:

- Rust service unavailable;
- PostgreSQL temporarily unavailable;
- Redis unavailable;
- transient network failures;
- slow downstream responses;
- duplicate requests;
- pod restarts;
- rolling deployments.

The Go gateway must use:

- timeouts;
- bounded retries;
- exponential backoff where appropriate;
- circuit breaking or equivalent protection where justified.

Retries must never create uncontrolled duplicate writes.

---

# 38. Graceful Shutdown

Both services must implement graceful shutdown.

On termination:

1. stop accepting new work;
2. finish safe in-flight work;
3. cancel or requeue appropriate processing;
4. flush telemetry;
5. close database connections;
6. close Redis connections;
7. terminate cleanly.

Kubernetes termination grace periods must be configured accordingly.

---

# 39. Observability

Observability is mandatory.

The system must provide:

- structured logs;
- metrics;
- distributed traces;
- request IDs;
- correlation IDs;
- service metadata.

---

# 40. OpenTelemetry

OpenTelemetry must be used for distributed tracing.

Trace propagation must work across:

```text
Client
  ↓
Go Gateway
  ↓
Rust Processor
  ↓
Database / Redis
```

Trace attributes must include safe operational metadata.

Sensitive payload contents must not be placed in traces.

---

# 41. Metrics

Prometheus-compatible metrics must be exposed.

Application metrics must include:

```text
HTTP request count
HTTP request latency
HTTP error count
database latency
Redis latency
processing jobs
processing duration
processing failures
processing queue depth
active jobs
batch sizes
rate-limit events
```

Metrics must use bounded cardinality.

User IDs, patient IDs, request IDs, and other high-cardinality identifiers must not be used as metric labels.

---

# 42. Logging

Logs must be structured JSON.

Every relevant request log must contain:

```text
timestamp
level
service
environment
request_id
trace_id
message
```

Logs must not contain:

- passwords;
- JWT tokens;
- secrets;
- sensitive personal data;
- complete health measurement payloads.

---

# 43. Audit Logging

Security-sensitive operations must generate audit records.

Examples:

```text
LOGIN
LOGOUT
PATIENT_CREATED
PATIENT_DELETED
MEASUREMENT_CREATED
MEASUREMENT_DELETED
PROCESSING_JOB_CREATED
PROCESSING_JOB_CANCELLED
ROLE_CHANGED
```

Audit records must contain:

```text
actor
action
resource
timestamp
request_id
metadata
```

Audit logs must be append-oriented.

---

# 44. Testing Strategy

Testing is mandatory at every layer.

Required test categories:

```text
Unit tests
Integration tests
API tests
Contract tests
End-to-end tests
Database tests
Security tests
Load tests
Performance benchmarks
Failure-injection tests
```

---

# 45. Go Testing

The Go service must have:

- unit tests;
- handler tests;
- service-layer tests;
- repository tests;
- middleware tests;
- authentication tests;
- authorization tests;
- rate-limit tests;
- integration tests.

Target:

```text
go test ./...
```

must pass from a clean environment.

---

# 46. Rust Testing

The Rust service must have:

- unit tests;
- integration tests;
- serialization tests;
- algorithm correctness tests;
- concurrency tests;
- timeout tests;
- cancellation tests;
- benchmark suites.

The processing algorithms must include deterministic fixtures.

---

# 47. Database Testing

Database integration tests must execute against a real PostgreSQL instance.

SQLite must not be used as a substitute for PostgreSQL.

Tests must verify:

- migrations;
- constraints;
- indexes;
- transactions;
- concurrent operations;
- rollback behavior.

---

# 48. End-to-End Tests

The E2E suite must validate:

```text
Authenticate
    ↓
Create patient
    ↓
Submit measurements
    ↓
Create processing job
    ↓
Rust processing
    ↓
Persist results
    ↓
Retrieve results
```

The E2E suite must run against containerized services.

---

# 49. Contract Testing

The Go ↔ Rust API contract must be explicitly tested.

Changes to the internal API must fail CI if they introduce an incompatible contract without an intentional version change.

---

# 50. Performance Requirements

Performance targets must be measurable rather than theoretical.

The project must include benchmarks for:

- measurement ingestion;
- batch ingestion;
- statistical aggregation;
- anomaly detection;
- serialization;
- Rust processing throughput.

The benchmark suite must report:

```text
throughput
latency
p50
p95
p99
memory usage
CPU utilization
```

Exact performance targets must be documented based on benchmark hardware and workload.

No unsupported absolute performance claims may appear in the README.

---

# 51. Load Testing

The project must include a reproducible load-testing setup.

The load test must simulate:

- concurrent clients;
- measurement ingestion;
- processing-job creation;
- result retrieval;
- authentication;
- rate limiting.

The load-testing tool may be:

- k6;
- Locust;
- or an equivalent open-source tool.

Load tests must be executable locally and against staging.

---

# 52. Chaos / Failure Testing

The project should include controlled failure scenarios.

Examples:

```text
PostgreSQL unavailable
Redis unavailable
Rust processor unavailable
Rust processor timeout
pod restart
network latency
processing failure
database connection exhaustion
```

Expected behavior must be documented.

---

# 53. Local Development

The complete system must be runnable locally using Docker Compose.

Expected command:

```bash
docker compose up
```

Development environment must include:

```text
Go Gateway
Rust Processor
PostgreSQL
Redis
observability dependencies where practical
```

A developer must not need AWS credentials to run the complete local environment.

---

# 54. Developer Experience

The repository must provide a unified developer interface.

Recommended commands:

```bash
make setup
make dev
make test
make lint
make format
make build
make integration-test
make e2e
make load-test
make security-scan
make verify
```

Equivalent task runners are acceptable.

---

# 55. Code Quality

All code must follow idiomatic language conventions.

Go:

- `gofmt`;
- `go vet`;
- static analysis;
- idiomatic error handling.

Rust:

- `rustfmt`;
- `clippy`;
- idiomatic ownership;
- explicit error types;
- no unnecessary `unwrap()`/`expect()` in production paths.

---

# 56. Dependency Management

Dependencies must be:

- explicitly versioned;
- regularly auditable;
- scanned for vulnerabilities;
- minimized.

Unused dependencies must be removed.

Dependency upgrades must not be performed blindly by AI agents.

Compatibility must be verified with tests.

---

# 57. Infrastructure as Code

AWS infrastructure must be defined as code.

Preferred technology:

**Terraform**

Infrastructure must include:

```text
VPC
subnets
security groups
EKS
ECR
RDS PostgreSQL
ElastiCache Redis
IAM
CloudWatch/logging integration
DNS/TLS infrastructure where applicable
```

Infrastructure must support:

```text
staging
production
```

without duplicating large amounts of configuration.

---

# 58. AWS Architecture

Target production architecture:

```text
                         Internet
                            │
                            ▼
                     AWS Load Balancer
                            │
                            ▼
                       EKS Cluster
                            │
                 ┌──────────┴──────────┐
                 ▼                     ▼
             Go Gateway          Rust Processor
                 │                     │
                 └──────────┬──────────┘
                            │
                ┌───────────┴───────────┐
                ▼                       ▼
          RDS PostgreSQL         ElastiCache Redis

GitHub Actions
       │
       ▼
      ECR
       │
       ▼
      EKS
```

---

# 59. AWS Security

The infrastructure must follow least privilege.

Requirements:

- IAM roles instead of long-lived credentials where possible;
- GitHub Actions OIDC;
- private database subnets;
- restricted security groups;
- encrypted storage;
- encrypted traffic;
- secrets outside Git;
- Kubernetes RBAC;
- separate staging and production environments.

---

# 60. CI Pipeline

Every pull request must execute:

```text
Checkout
   ↓
Dependency installation
   ↓
Formatting checks
   ↓
Lint
   ↓
Static analysis
   ↓
Unit tests
   ↓
Integration tests
   ↓
Contract tests
   ↓
Build
   ↓
Container build
   ↓
Container security scan
   ↓
Dependency/security scan
```

Pull requests must not be mergeable if required quality gates fail.

---

# 61. CD Pipeline

Deployment pipeline:

```text
main
 │
 ▼
Build
 │
 ▼
Test
 │
 ▼
Build Docker images
 │
 ▼
Scan images
 │
 ▼
Push images to ECR
 │
 ▼
Deploy staging
 │
 ▼
Smoke tests
 │
 ▼
Integration/E2E tests
 │
 ▼
Production approval
 │
 ▼
Deploy production
 │
 ▼
Post-deployment verification
```

---

# 62. Deployment Strategy

Production deployments must use rolling updates.

The deployment must support:

- zero/minimal downtime;
- readiness gates;
- automatic rollback;
- immutable image versions;
- deployment health checks.

---

# 63. GitHub Actions Security

GitHub Actions must:

- use OIDC for AWS authentication;
- avoid long-lived AWS credentials;
- use minimal permissions;
- pin or carefully manage third-party actions;
- separate staging and production environments;
- require approval for production;
- avoid exposing secrets in logs.

---

# 64. Database Migrations

Database migrations must be version-controlled.

Requirements:

- forward migrations;
- migration validation;
- migration execution in deployment;
- rollback strategy documented;
- migration locking;
- no destructive migration without explicit compatibility planning.

Application deployments must remain compatible with the database during rolling upgrades.

---

# 65. API Versioning

Public API:

```text
/api/v1
```

Breaking changes require:

```text
/api/v2
```

Internal API changes must use explicit compatibility rules.

---

# 66. Configuration

Configuration must be externalized.

Configuration categories:

```text
DATABASE_URL
REDIS_URL
JWT configuration
service URLs
timeouts
rate limits
processing limits
observability configuration
AWS configuration
```

Configuration must have:

- typed parsing;
- validation;
- safe defaults where appropriate;
- startup failure for invalid mandatory configuration.

---

# 67. Environment Separation

At minimum:

```text
local
test
staging
production
```

Environment-specific configuration must not be embedded in application code.

Production configuration must never be required for local development.

---

# 68. Repository Structure

Recommended repository structure:

```text
vitalmesh/
├── services/
│   ├── api-gateway/
│   │   ├── cmd/
│   │   ├── internal/
│   │   ├── migrations/
│   │   └── tests/
│   │
│   └── processor/
│       ├── src/
│       ├── tests/
│       └── benches/
│
├── contracts/
│   ├── openapi/
│   └── internal-api/
│
├── infrastructure/
│   ├── terraform/
│   └── kubernetes/
│
├── deployments/
│   ├── docker/
│   └── compose/
│
├── tests/
│   ├── e2e/
│   ├── integration/
│   └── load/
│
├── observability/
│   ├── prometheus/
│   ├── grafana/
│   └── otel/
│
├── scripts/
│
├── docs/
│
├── .github/
│   └── workflows/
│
├── Makefile
├── docker-compose.yml
├── README.md
└── SPECIFICATIONS.md
```

The exact directory layout may be refined during implementation, but architectural boundaries must remain intact.

---

# 69. Git Strategy

The repository must use meaningful commits.

Recommended format:

```text
feat:
fix:
refactor:
test:
docs:
build:
ci:
infra:
perf:
security:
```

Commits must be:

- focused;
- reviewable;
- independently understandable.

AI agents must not create giant unrelated commits.

---

# 70. AI-First Development Rules

This repository is explicitly designed for development using Cursor and Claude Code.

AI agents must treat `SPECIFICATIONS.md` as the authoritative product and architecture specification.

Before implementing a feature, the agent must:

1. inspect the existing repository;
2. inspect relevant specifications;
3. inspect existing tests;
4. identify architectural constraints;
5. identify affected components;
6. propose the implementation plan;
7. implement the smallest coherent change;
8. add/update tests;
9. run relevant quality gates;
10. review the resulting diff.

AI agents must not blindly rewrite unrelated code.

---

# 71. AI Change Discipline

An AI coding agent must not:

- introduce new technologies without justification;
- duplicate existing abstractions;
- bypass tests;
- disable lint rules to make CI pass;
- suppress compiler warnings without justification;
- weaken security controls;
- remove failing tests;
- modify CI checks merely to obtain green CI;
- introduce secrets;
- make unrelated refactors;
- silently change API contracts.

---

# 72. AI Verification Loop

Every implementation task must follow:

```text
Understand
   ↓
Plan
   ↓
Implement
   ↓
Format
   ↓
Lint
   ↓
Test
   ↓
Inspect diff
   ↓
Review against SPECIFICATIONS.md
   ↓
Fix
   ↓
Re-run verification
```

The agent must report:

- files changed;
- tests added;
- commands executed;
- verification results;
- known limitations.

---

# 73. Definition of Done

A feature is complete only when:

- implementation exists;
- architecture remains consistent;
- tests exist;
- edge cases are covered;
- documentation is updated;
- observability is included where relevant;
- security implications are addressed;
- lint passes;
- type/static checks pass;
- tests pass;
- build succeeds;
- relevant integration tests pass;
- no unrelated regressions are introduced.

---

# 74. Global Verification

The repository must provide a single command:

```bash
make verify
```

or equivalent.

It must execute all mandatory local quality gates.

Expected categories:

```text
format
lint
static analysis
unit tests
integration tests
contract tests
build
container validation
```

The exact command may evolve, but a new contributor must have one obvious way to determine whether the repository is healthy.

---

# 75. Security Verification

CI must include:

- dependency vulnerability scanning;
- secret scanning;
- container image scanning;
- IaC scanning;
- Kubernetes manifest validation.

Recommended tooling may include:

```text
Trivy
Gitleaks
Checkov
Hadolint
Kubeconform
```

Equivalent tools are acceptable.

---

# 76. Documentation Requirements

The repository must contain:

```text
README.md
ARCHITECTURE.md
DEVELOPMENT.md
OPERATIONS.md
SECURITY.md
API.md
PERFORMANCE.md
```

Documentation must describe:

- architecture;
- local setup;
- API;
- deployment;
- infrastructure;
- observability;
- security;
- performance;
- troubleshooting.

---

# 77. Architecture Documentation

`ARCHITECTURE.md` must contain:

- system context;
- container diagram;
- service responsibilities;
- database architecture;
- communication protocols;
- deployment architecture;
- observability architecture;
- security boundaries;
- major architectural decisions.

Architecture diagrams may use Mermaid.

---

# 78. Operational Documentation

`OPERATIONS.md` must document:

- deployment;
- rollback;
- migrations;
- health checks;
- monitoring;
- common failures;
- log inspection;
- scaling;
- incident troubleshooting.

---

# 79. Disaster Recovery

The production architecture must document:

- PostgreSQL backups;
- restore procedures;
- backup retention;
- recovery objectives;
- infrastructure recreation;
- secret recovery;
- failure scenarios.

Exact RPO/RTO targets must be documented rather than implied.

---

# 80. Backup Requirements

Production PostgreSQL must use managed AWS backups.

The project must document:

- automated backups;
- retention;
- point-in-time recovery;
- restore verification.

A restore procedure must be tested at least once as part of the project's operational validation.

---

# 81. Data Retention

Synthetic health measurements must have configurable retention policies.

The implementation must support eventual archival/deletion without breaking referential integrity.

Retention jobs must be observable and auditable.

---

# 82. Privacy

Even though all data is synthetic, the architecture must follow privacy-oriented engineering practices.

Requirements:

- data minimization;
- no sensitive data in logs;
- no sensitive data in metrics;
- no secrets in source control;
- explicit access control;
- auditability.

---

# 83. Performance Architecture

The system must be designed for horizontal scaling.

Go Gateway:

```text
multiple replicas
stateless application layer
```

Rust Processor:

```text
multiple replicas
bounded concurrency
stateless processing
```

PostgreSQL:

```text
central persistent state
```

Redis:

```text
shared ephemeral state
```

No service must rely on local filesystem state for correctness.

---

# 84. Caching

Caching must be used only where it provides measurable benefit.

Potential cache candidates:

- patient summaries;
- frequently requested processing results;
- configuration metadata.

Cache invalidation must be explicit.

Correctness must not depend on stale cached data.

---

# 85. Request Correlation

Every inbound request must receive or propagate a request ID.

The ID must propagate through:

```text
Go → Rust → database/telemetry
```

Where supported, W3C trace context must be propagated.

---

# 86. Time Handling

All persisted timestamps must use UTC.

All service-to-service communication must use UTC timestamps.

Client timezone conversion belongs at the presentation layer.

Time-dependent algorithms must have deterministic tests around:

- daylight saving transitions;
- leap days;
- clock boundaries;
- future timestamps;
- stale timestamps.

---

# 87. Clock and Timestamp Validation

The system must reject or explicitly handle:

- impossible timestamps;
- excessively future timestamps;
- timestamps outside configured retention windows.

Server-side timestamps must be generated by trusted server clocks.

---

# 88. Data Validation

Validation must happen at multiple boundaries:

```text
HTTP boundary
service boundary
database boundary
processing boundary
```

Validation must never rely solely on frontend behavior.

---

# 89. Large Payload Protection

The API must enforce:

- maximum request body size;
- maximum batch size;
- maximum metadata size;
- maximum string lengths.

Oversized requests must fail early.

---

# 90. Dependency Failure Policy

Each dependency must have an explicit failure mode.

Example:

| Dependency | Failure behavior |
|---|---|
| PostgreSQL | API degraded/unavailable |
| Redis | rate limiting/cache degraded |
| Rust Processor | processing unavailable |
| OpenTelemetry | application continues |
| Prometheus | application continues |

Telemetry failure must never make the application unavailable.

---

# 91. Backpressure

The processing system must implement bounded queues or equivalent flow control.

When processing capacity is exhausted, the system must:

- reject;
- defer;
- or explicitly queue work.

It must never create unbounded in-memory queues.

---

# 92. Job State Machine

Valid transitions:

```text
PENDING → PROCESSING
PENDING → CANCELLED

PROCESSING → COMPLETED
PROCESSING → FAILED
PROCESSING → CANCELLED
```

Invalid transitions must be rejected.

State transitions must be atomic.

---

# 93. Retry Policy

Retries must be classified.

Retryable:

```text
temporary network errors
temporary downstream availability failures
transient infrastructure failures
```

Non-retryable:

```text
validation failures
authentication failures
authorization failures
invalid data
contract violations
```

Retries must use bounded exponential backoff.

---

# 94. Dead Letter / Failed Jobs

Failed processing jobs must preserve diagnostic metadata.

A failed job must not disappear.

Failure records must include:

```text
error_code
safe error message
attempt count
timestamps
service version
algorithm version
request ID
trace ID
```

Sensitive internal stack traces must remain server-side.

---

# 95. Versioning

The following must be versioned:

- public API;
- internal processing contract;
- database migrations;
- Docker images;
- processing algorithms.

Processing results must record:

```text
algorithm_version
service_version
```

This ensures reproducibility.

---

# 96. Algorithm Reproducibility

Processing algorithms must be deterministic.

Tests must verify that:

```text
same input
+
same configuration
+
same algorithm version
=
same result
```

Floating-point behavior must be documented where relevant.

---

# 97. Benchmark Reproducibility

Benchmarks must document:

- CPU;
- RAM;
- OS;
- Rust version;
- Go version;
- dataset size;
- concurrency;
- benchmark command.

Performance numbers must not be presented without workload context.

---

# 98. Release Management

Releases must be reproducible from Git.

Each release must identify:

```text
Git commit
Go version
Rust version
dependency lockfiles
container image digest
infrastructure version
database migration version
```

---

# 99. Supply Chain Security

The project should support:

- dependency lockfiles;
- SBOM generation;
- container vulnerability scanning;
- provenance where practical;
- signed artifacts where practical.

Production deployment should prefer immutable artifact references.

---

# 100. Infrastructure Environments

Terraform must support environment separation.

Recommended:

```text
infrastructure/
└── terraform/
    ├── modules/
    └── environments/
        ├── staging/
        └── production/
```

Environment state must be isolated.

---

# 101. Kubernetes Environments

Recommended:

```text
kubernetes/
├── base/
└── overlays/
    ├── staging/
    └── production/
```

Kustomize or Helm may be used.

The chosen mechanism must avoid excessive duplication.

---

# 102. Local Kubernetes

The project should support a local Kubernetes environment using one of:

```text
kind
minikube
k3d
```

The local cluster must be suitable for validating Kubernetes manifests without AWS.

---

# 103. Staging Environment

Staging must approximate production architecture sufficiently to validate:

- deployments;
- migrations;
- networking;
- observability;
- autoscaling;
- service communication;
- security;
- rollback.

Staging must use synthetic data only.

---

# 104. Production Environment

Production configuration must:

- use private managed database infrastructure;
- use managed Redis;
- use HTTPS;
- use restricted network access;
- use separate secrets;
- use production-specific IAM roles;
- enable monitoring;
- enable backups;
- enable auditability.

No developer workstation credentials may be required for runtime.

---

# 105. Cost Awareness

AWS infrastructure must be designed with portfolio-project cost constraints in mind.

The implementation must document:

- expected infrastructure cost drivers;
- optional expensive components;
- how to scale down or destroy environments;
- resources that may incur charges even when idle.

A local development environment must remain available without AWS.

---

# 106. Demo Dataset

The repository must include a synthetic dataset generator.

It must be capable of generating:

- patients;
- realistic-looking measurement streams;
- normal values;
- noisy data;
- anomalies;
- multiple measurement types;
- configurable time ranges;
- configurable volume.

The generator must never use real patient information.

---

# 107. CLI / Developer Utilities

The project should provide a developer CLI or scripts capable of:

```text
generate demo data
create test users
seed database
submit processing jobs
inspect job status
run benchmarks
run smoke tests
```

The tooling must be safe to execute against local/staging environments.

Production destructive operations must require explicit safeguards.

---

# 108. Demo Scenario

The repository must provide a documented end-to-end demo:

```text
1. Start local infrastructure
2. Create synthetic user
3. Authenticate
4. Create synthetic patient
5. Generate measurements
6. Submit measurements
7. Create processing job
8. Rust processes data
9. Retrieve results
10. View metrics
11. View traces
12. Inspect structured logs
```

The complete demo should be executable by a new developer.

---

# 109. Acceptance Criteria

VitalMesh is considered complete when:

### Application

- Go gateway is implemented.
- Rust processor is implemented.
- PostgreSQL persistence is implemented.
- Redis integration is implemented.
- authentication works.
- RBAC works.
- processing jobs work.
- statistical processing works.
- anomaly detection works.
- idempotency works.
- rate limiting works.

### Infrastructure

- Docker images build reproducibly.
- Docker Compose environment works.
- Kubernetes manifests work.
- local Kubernetes deployment works.
- Terraform provisions AWS infrastructure.
- staging deployment works.
- production deployment architecture is defined and deployable.

### CI/CD

- PR validation works.
- security scanning works.
- container scanning works.
- images are published to ECR.
- staging deployment is automated.
- production deployment requires controlled promotion.
- rollback is documented and tested.

### Observability

- logs work.
- metrics work.
- tracing works.
- Go → Rust trace propagation works.
- dashboards exist.
- alerts are documented.

### Testing

- unit tests pass.
- integration tests pass.
- contract tests pass.
- E2E tests pass.
- load tests are reproducible.
- failure scenarios are tested.

### Documentation

- README exists.
- architecture documentation exists.
- API documentation exists.
- development documentation exists.
- operations documentation exists.
- security documentation exists.
- performance documentation exists.

---

# 110. AI Agent Acceptance Protocol

Before declaring the project complete, an AI agent must perform a final repository audit.

The audit must verify:

```text
[ ] SPECIFICATIONS.md compliance
[ ] architecture consistency
[ ] API contract consistency
[ ] database integrity
[ ] security controls
[ ] observability
[ ] test coverage
[ ] CI/CD
[ ] Docker
[ ] Kubernetes
[ ] Terraform
[ ] AWS deployment
[ ] documentation
[ ] performance benchmarks
[ ] failure handling
[ ] dependency security
[ ] secret scanning
```

The agent must identify any remaining deviations.

A green build alone is not sufficient evidence of completion.

---

# 111. Final Quality Gate

The final project must be reproducible from a clean checkout.

The following workflow must be documented and functional:

```text
git clone
     ↓
install prerequisites
     ↓
make setup
     ↓
make verify
     ↓
make dev
     ↓
run demo
```

For cloud deployment:

```text
configure AWS
     ↓
terraform plan
     ↓
terraform apply
     ↓
deploy application
     ↓
run smoke tests
     ↓
verify observability
```

No undocumented manual steps may be required.

---

# 112. Engineering Principles

The following principles are mandatory:

### Explicit over implicit

Architectural behavior must be visible in code and configuration.

### Boring infrastructure over clever infrastructure

Use established technologies and patterns unless there is a measurable reason not to.

### Fail safely

Dependencies can fail. The application must have defined failure modes.

### Secure by default

Security controls must not be optional merely because the application is a portfolio project.

### Observable by default

Every production-relevant operation must be measurable and traceable.

### Test behavior, not implementation

Tests should protect contracts and invariants rather than internal implementation details.

### Optimize with evidence

Performance optimization must be based on benchmarks and profiling.

### Automate everything repeatable

Builds, tests, deployments, migrations, scans, and environment setup must be reproducible.

---

# 113. Technology Baseline

The target technology stack is:

| Layer | Technology |
|---|---|
| API Gateway | Go |
| Data Processor | Rust |
| Database | PostgreSQL |
| Cache / Rate Limiting | Redis |
| API | REST / JSON |
| Internal Service Communication | HTTP/JSON |
| API Contract | OpenAPI |
| Containers | Docker |
| Orchestration | Kubernetes |
| Local Kubernetes | kind/minikube/k3d |
| Cloud | AWS |
| Kubernetes Cloud | EKS |
| Container Registry | ECR |
| Database Cloud | RDS PostgreSQL |
| Redis Cloud | ElastiCache |
| IaC | Terraform |
| CI/CD | GitHub Actions |
| Authentication | JWT |
| Observability | OpenTelemetry |
| Metrics | Prometheus |
| Dashboards | Grafana |
| Load Testing | k6 or equivalent |
| Security Scanning | Trivy / Gitleaks / Checkov |
| Configuration | Environment-based |
| Documentation | Markdown + Mermaid |

Technology substitutions require explicit architectural justification.

---

# 114. Specification Authority

`SPECIFICATIONS.md` is the authoritative functional and technical specification for VitalMesh.

If implementation decisions conflict with this document, the AI agent must:

1. identify the conflict;
2. explain the implications;
3. propose a resolution;
4. avoid silently changing the specification.

Specification changes must be deliberate, documented, and reflected in the implementation.

---

# 115. Completion Standard

VitalMesh is not complete when the application merely "works."

It is complete when the repository demonstrates that a competent engineering team could:

- understand the architecture;
- run it locally;
- test it;
- inspect it;
- deploy it;
- monitor it;
- diagnose failures;
- scale it;
- secure it;
- reproduce it;
- and evolve it safely.

The final result should represent a credible **production-oriented cloud-native distributed systems project**, while remaining explicitly limited to synthetic e-health data and educational/portfolio use.