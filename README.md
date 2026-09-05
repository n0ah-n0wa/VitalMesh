# VitalMesh

VitalMesh is a cloud-native e-health data processing platform built as a technical portfolio and reference implementation. A Go API gateway accepts synthetic health measurements, persists them and dispatches processing jobs to a Rust service that performs deterministic statistical and anomaly analysis.

VitalMesh is **not** a medical device, clinical decision-support system or healthcare product. All data it handles is synthetic, and any anomaly it reports is a technical data-processing result, never a diagnosis.

`SPECIFICATIONS.md` is the authoritative specification. `docs/IMPLEMENTATION_PLAN.md` describes how it is being delivered in phases.

## Status

Repository foundation. Both services build, test and expose health endpoints only. No domain functionality, database or cache integration exists yet.

## Repository structure

```text
services/api-gateway/     Go API gateway (public boundary: HTTP API, auth, persistence)
services/processor/       Rust processing service (statistics, anomaly detection)
contracts/openapi/        Public API contract (OpenAPI)
contracts/internal-api/   Gateway ↔ processor contract (OpenAPI)
infrastructure/terraform/ AWS infrastructure as code
infrastructure/kubernetes/ Kubernetes manifests
deployments/              Dockerfiles and Docker Compose
tests/                    Cross-service test suites (e2e, integration, load, chaos)
observability/            Prometheus, Grafana and OpenTelemetry configuration
scripts/                  Helper scripts used by the Makefile and CI
docs/                     Architecture, development and operations documentation
.github/workflows/        GitHub Actions pipelines
.devcontainer/            Reproducible toolchain image
Makefile                  Unified developer interface (`make help`)
```

## Quick start

```bash
make setup    # check prerequisites, download dependencies
make verify   # format check, lint, tests, build, line-ending check
make build    # bin/api-gateway and services/processor/target/debug/processor
```

Prerequisites, Windows notes and how to run the services are in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).
