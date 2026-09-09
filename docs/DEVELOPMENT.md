# Development

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.25.14 | pinned in `services/api-gateway/go.mod`; an older 1.25 install downloads it automatically via `GOTOOLCHAIN=auto` |
| Rust | 1.89.0 | pinned in `services/processor/rust-toolchain.toml`; install via [rustup](https://rustup.rs), which picks up the pin and the `rustfmt`/`clippy` components automatically |
| GNU make | 4.x | |
| git | any recent | the line-ending gate reads the git index |
| Docker | any recent | only needed for the container-based workflow below |

### Windows

The Makefile targets a POSIX shell. Use one of:

- **Dev Container (recommended).** Open the repository in VS Code and choose *Reopen in Container*; `.devcontainer/` provides Go, Rust, make and git at the pinned versions.
- **Container one-liner** without VS Code:

  ```bash
  docker build -t vitalmesh-dev .devcontainer
  docker run --rm -v "$(pwd):/workspace" -e CARGO_TARGET_DIR=/tmp/target vitalmesh-dev make verify
  ```

  In Git Bash set `MSYS_NO_PATHCONV=1` first so `/workspace` is not rewritten to a Windows path.
- **WSL2** with the tools installed natively.

Line endings: `.gitattributes` forces LF for every text file, so a Windows `core.autocrlf=true` setting does not corrupt scripts, Makefiles or manifests inside containers.

## Make targets

Run `make help` for the list. The gates that CI runs are exactly the ones `make verify` runs locally.

| Target | What it does |
|---|---|
| `make setup` | checks that `go` and `cargo` are installed, downloads Go modules and Cargo crates |
| `make format` | `gofmt -w` and `cargo fmt` |
| `make format-check` | fails if any file is not formatted |
| `make lint` | `go vet` and `cargo clippy --all-targets -D warnings` |
| `make test` | unit tests: `go test -race ./...` and `cargo test` |
| `make contracts-check` | checks the API contracts and that both services' types agree with them |
| `make contracts-lock` | re-records a reviewed contract change in its lock file |
| `make dev-db` / `make dev-redis` | start the local PostgreSQL / Redis (`docker compose`) |
| `make docker-build` | build both service images |
| `make docker-verify` | check the images run as non-root, carry no shell and report healthy |
| `make docker-scan` | scan the images and the lock files, failing on HIGH or CRITICAL |
| `make observability-up` / `-down` | start / stop Prometheus, Grafana and the OpenTelemetry collector |
| `make observability-smoke` | check the running stack is scraping, recording and receiving spans |
| `make dev-db-down` | stop and remove the local infrastructure |
| `make integration-test` | integration tests against `TEST_DATABASE_URL` and `TEST_REDIS_URL` (default: the local PostgreSQL and Redis) |
| `make e2e-test` | cross-service tests: the real gateway against the real processor binary, which it builds first |
| `make migrate` | apply migrations to `DATABASE_URL` (default: the local PostgreSQL) |
| `make build` | builds `bin/api-gateway` and `services/processor/target/debug/processor` |
| `make line-endings` | fails if any tracked file is stored with CRLF |
| `make verify` | `format-check lint test contracts-check integration-test e2e-test build line-endings`; needs the local PostgreSQL and Redis (`make dev-db`, `make dev-redis`) |
| `make clean` | removes build outputs |

Cargo runs with `--locked`, so `Cargo.lock` must be updated deliberately (`cargo update -p <crate>`) and committed.

## The local environment

One command brings up everything: both services, PostgreSQL, Redis,
Prometheus, Grafana and the OpenTelemetry collector.

```bash
docker compose up
```

Nothing needs setting first. Every value has a development default, so a
clean checkout starts; `.env.example` documents what can be overridden and
`.env` is git-ignored, which is where a local value belongs.

| Service | Address |
|---|---|
| API gateway | http://localhost:8080 |
| Processor (internal API) | http://localhost:8081 |
| Grafana | http://localhost:3000 |
| Prometheus | http://localhost:9090 |
| PostgreSQL | localhost:5432 |
| Redis | localhost:6379 |

Every one of those ports is published on 127.0.0.1 rather than on all
interfaces. Docker's default is all of them, which would put the database,
an unauthenticated Redis and a login-free Grafana on whatever network the
machine happens to be attached to.

It also means start-up fails, rather than quietly taking the port, when
something else already holds one. A machine-wide PostgreSQL service is the
usual cause, and Docker words it unhelpfully:

```
Ports are not available: exposing port TCP 127.0.0.1:5432 -> 0.0.0.0:0:
listen tcp 127.0.0.1:5432: bind: An attempt was made to access a socket in
a way forbidden by its access permissions.
```

(Linux and macOS say `address already in use`.) Move the container's host
port and leave the other server alone:

```bash
POSTGRES_PORT=5433 docker compose up -d --wait
```

`.env.example` lists every port variable, and copying it to `.env` makes
the change stick. Only the host side moves: inside the network the services
still reach `postgres:5432`, so nothing else changes. Integration tests run
from the host need `TEST_DATABASE_URL` pointed at the same port; the ones
`make verify` runs are inside the network and are unaffected.

Start-up is ordered by readiness rather than by hope. The gateway waits for
PostgreSQL and Redis to report healthy, for the migration job to exit
successfully, and for the processor's own health check to pass. Migrations
run as their own one-shot service rather than inside the gateway, because
several gateway replicas starting at once must not race each other through
the schema.

A subset works too, because nothing depends on more than it needs:

```bash
docker compose up -d --wait postgres redis   # what the tests need
docker compose up -d --wait api-gateway      # and everything it depends on
```

The observability stack is not a dependency of either service, so stopping
all three leaves both running. That is the property that matters: metrics
are served whether or not anyone scrapes them, and traces go to a collector
that may not exist.

`docker compose down` stops everything and keeps the database volume;
`docker compose down -v` discards it and starts from an empty schema next
time.

## The demo

```bash
docker compose up -d --wait
./scripts/demo.sh
```

It creates an operator account, signs in, registers a patient, records
sixty heart-rate readings with one deliberate spike, runs them through the
Rust engine, reads the statistics and anomalies back, and then repeats a
request with the same `Idempotency-Key` to show the stored response coming
back rather than a second patient being created. Every step prints what it
sent and what came back.

It is safe to run repeatedly: each run uses a fresh patient reference and
fresh idempotency keys, and the account is created only if it is not
already there.

## Container images

Each service has a Dockerfile beside it. The images are built and checked
separately from `make verify`, so the verification gates stay runnable
without a container runtime and a slow image build does not sit in front of
the tests. CI builds, verifies and scans them in a job of its own.

```bash
make docker-build
make docker-verify   # non-root, no shell, read-only root, healthy
make docker-scan     # Trivy over the images and the dependency lock files
```

`make docker-build` delegates to `docker compose build`, so what the gates
check is what `docker compose up` runs: one build definition rather than
two that drift apart. The tag and the version inside the binary are
separate. `IMAGE_TAG` names the image and defaults to `dev`, which is what
compose expects; `VERSION` is compiled in and reported by `/health`, and
`make docker-build` sets it to the current commit. A locally tagged image
can therefore still say which commit produced it.

Both images are two-stage: a builder with the toolchain, and a runtime with
the binary and nothing else. Neither contains a shell or a package manager,
so a health check cannot be a `curl` command; each binary instead takes a
`healthcheck` argument that probes its own `/health`, and that is what the
image's `HEALTHCHECK` runs.

Running one directly needs the same configuration a host process does:

```bash
docker run --rm -p 127.0.0.1:8080:8080   -e DATABASE_URL=postgres://vitalmesh:vitalmesh@host.docker.internal:5432/vitalmesh?sslmode=disable   -e JWT_SECRET="$(openssl rand -base64 48)"   --read-only --cap-drop=ALL --security-opt no-new-privileges   vitalmesh/api-gateway:dev
```

`make docker-verify` starts both images with exactly that hardening and
fails if either needs to write to its root filesystem, runs as root, or has
a shell to exec into.

## Watching what the services do

The observability stack is optional and nothing depends on it, which is why
it lives behind a compose profile and no gate touches it.

```bash
make observability-up      # Prometheus :9090, Grafana :3000, collector :4318
make observability-smoke   # asserts it is actually observing something
```

Grafana needs no login locally and opens on four provisioned dashboards:
API traffic, latency and errors, Rust processing and job status, and
infrastructure. Both services must be running for them to show anything;
Prometheus reaches host processes through `host.docker.internal`.

Traces appear in the collector's own output, so a request crossing both
services can be followed without a trace store:

```bash
docker compose logs -f otel-collector
```

Set `OTEL_EXPORTER_OTLP_ENDPOINT` before starting a service to export from
it. Leaving it unset is a supported way to run: trace context still
propagates and trace ids still reach the logs, and nothing is exported.

See [`observability/README.md`](../observability/README.md) for the rules,
the dashboards and how to add to them.

## Changing an API contract

The contracts under [`contracts/`](../contracts) are checked by `make contracts-check`, which `make verify` includes. Editing prose in a contract needs nothing extra. Changing anything a client can observe fails the lock gate until the change is re-recorded:

```bash
make contracts-lock
```

Do that only after checking the change against the compatibility rules in [`contracts/internal-api/README.md`](../contracts/internal-api/README.md); a breaking change needs a new major version and a new path prefix, not a new fingerprint. The lock file is committed with the contract, so the diff shows both.

## Running the services

Both services read `HTTP_ADDR` and expose `GET /health` (liveness) and `GET /ready` (readiness), returning `{"status":"ok","service":"...","version":"..."}`.

```bash
make build
make dev-db && make dev-redis && make migrate
export DATABASE_URL=postgres://vitalmesh:vitalmesh@localhost:5432/vitalmesh?sslmode=disable
export REDIS_URL=redis://localhost:6379                 # optional: without it the gateway runs degraded
export JWT_SECRET="$(openssl rand -base64 48)"       # local only; never commit a value (SPECIFICATIONS.md section 31)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 # optional: only if the collector is running
./bin/api-gateway                                   # :8080
./services/processor/target/debug/processor         # listens on 0.0.0.0:8081
curl localhost:8080/health
curl localhost:8080/ready                           # 503 until PostgreSQL is reachable
curl localhost:8081/ready
```

To call authenticated endpoints, create an account and log in (the password is read from standard input so that it never appears in the shell history or process list):

```bash
printf '%s\n' 'choose-a-local-password' | ./bin/api-gateway users create admin@example.com ADMIN
curl -s localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"choose-a-local-password"}'      # {"access_token":"...","token_type":"Bearer",...}
curl -s localhost:8080/api/v1/auth/me -H "Authorization: Bearer $TOKEN"
```

Local secrets belong in a `.env` file (ignored by Git) or the shell, never in tracked files.

`version` is the short git SHA injected by the Makefile (`VERSION=...` overrides it), or `dev` when built directly with `go build` / `cargo build`.

The gateway's full configuration surface, package layout and layering rules are documented in [services/api-gateway/README.md](../services/api-gateway/README.md); the processor's in [services/processor/README.md](../services/processor/README.md).

## Repository layout

See the structure table in the top-level [README.md](../README.md). Directories that are not yet populated contain a short `README.md` stating what will live there and which phase of [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) delivers it.
