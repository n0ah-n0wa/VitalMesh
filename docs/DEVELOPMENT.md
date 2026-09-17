# Development

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.25.14 | pinned in `services/api-gateway/go.mod`. The Makefile exports `GOTOOLCHAIN=local`, as CI does, so the installed Go must be exactly this one; set `GOTOOLCHAIN=auto` in your own environment if you would rather have Go download it |
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

Run `make help` for the list. Every CI job runs one of these targets and nothing else, so what CI checks and what you run are the same commands; `make verify` is the code gates together, `make ci-local` is everything (see [Reproducing CI locally](#reproducing-ci-locally)).

| Target | What it does |
|---|---|
| `make setup` | checks that `go` and `cargo` are installed, then `setup-go` (Go modules) and `setup-rust` (Cargo crates, `--locked`) |
| `make format` | `gofmt -w` and `cargo fmt` |
| `make format-check` | `format-check-go` and `format-check-rust`: fails if any file is not formatted |
| `make lint` | `lint-go` (`go vet`, with every build tag) and `lint-rust` (`cargo clippy --all-targets -D warnings`) |
| `make test` | `test-go` (`go test -race ./...`, with the unit coverage floor) and `test-rust` (`cargo test`) |
| `make contracts-check` | checks the API contracts and that both services' types agree with them |
| `make contracts-lock` | re-records a reviewed contract change in its lock file |
| `make dev-db` / `make dev-redis` | start the local PostgreSQL / Redis (`docker compose`) |
| `make docker-build` | build both service images |
| `make docker-verify` | check the images run as non-root, carry no shell and report healthy |
| `make docker-scan` | scan the built images and the service Dockerfiles, failing on HIGH or CRITICAL |
| `make deps-scan` | scan `Cargo.lock` and `go.sum` (trivy) and the reachable Go call graph (govulncheck), failing on HIGH or CRITICAL |
| `make secret-scan` | gitleaks over the whole git history and the working tree |
| `make observability-up` / `-down` | start / stop Prometheus, Grafana and the OpenTelemetry collector |
| `make observability-smoke` | check the running stack is scraping, recording and receiving spans |
| `make dev-db-down` | stop and remove the local infrastructure |
| `make integration-test` | `integration-test-postgres` (the repository, the application layer, the processor client) and `integration-test-redis` (the client, the cache, rate limiting), against `TEST_DATABASE_URL` and `TEST_REDIS_URL` (default: the local PostgreSQL and Redis) |
| `make e2e-test` | cross-service tests: the real gateway against the real processor binary, which it builds first |
| `make coverage-go` | merges the Go coverage profiles from every suite and checks the merged floor (`GO_COVERAGE_MIN_ALL`) |
| `make stack-test` | starts a clean containerized stack as its own compose project, runs the end-to-end suite against it and removes it (`scripts/stack-test.sh`) |
| `make perf-baseline` | the performance baseline ([PERFORMANCE.md](PERFORMANCE.md) is the entry point): sign-in cost, the k6 load run with container stats, the server-side view from Prometheus, batch and job size sweeps, database sizes and plans; `RESET=1` for an empty database first ([PERFORMANCE_BASELINE.md](PERFORMANCE_BASELINE.md)) |
| `make load-test` | k6 in a pinned container against the local environment (`LOAD_PROFILE=smoke` or `standard`); the report lands in `tests/load/results/` ([LOAD_TESTING.md](LOAD_TESTING.md)) |
| `make migrate` | apply migrations to `DATABASE_URL` (default: the local PostgreSQL) |
| `make synth-generate` / `make synth-load` | write a synthetic fixture (`SYNTH_DIR`, flags in `SYNTH_ARGS`) and load it into the local environment: accounts, patients, readings and jobs; see [SYNTHETIC_DATA.md](SYNTHETIC_DATA.md) |
| `make build` | builds `bin/api-gateway`, `bin/synth` and `services/processor/target/debug/processor` |
| `make line-endings` | fails if any tracked file is stored with CRLF |
| `make verify` | `format-check lint test contracts-check integration-test e2e-test coverage-go build line-endings`; needs the local PostgreSQL and Redis (`make dev-db`, `make dev-redis`) |
| `make ci-local` | `verify`, then `deps-scan secret-scan docker-build docker-verify docker-scan stack-test k8s-validate tf-validate`: every check CI runs; needs Docker as well |
| `make clean` | removes build outputs |

Cargo runs with `--locked`, so `Cargo.lock` must be updated deliberately (`cargo update -p <crate>`) and committed.

## Reproducing CI locally

CI (`.github/workflows/ci.yml`) is eleven jobs that each run one `make`
target, plus a final `ci` job that fails unless all eleven succeeded; that
last one is what branch protection requires. The workflow adds only what a
laptop does not have by default: the runners, the PostgreSQL and Redis
service containers, and caches. Nothing it checks is CI-only.

| CI job | Runs | What it needs |
|---|---|---|
| `go` | `make setup-go format-check-go lint-go test-go` | Go |
| `rust` | `make setup-rust format-check-rust lint-rust test-rust` | Rust |
| `contracts` | `make contracts-check` | Go and Rust |
| `integration` | `make test-go integration-test-postgres integration-test-redis e2e-test coverage-go` | Go, Rust, PostgreSQL and Redis (`make dev-db dev-redis`) |
| `build` | `make build line-endings` | Go, Rust, git |
| `containers` | `make docker-build docker-verify docker-scan` | Docker |
| `stack end-to-end` | `make stack-test` | Docker and Go (see [The stack suite](#the-stack-suite)) |
| `dependency scan` | `make deps-scan` | Docker (trivy) and Go (govulncheck) |
| `secret scan` | `make secret-scan` | Docker (gitleaks), the full git history |
| `kubernetes manifests` | `make k8s-validate` | Docker |
| `terraform` | `make tf-validate` | Docker |

So:

```bash
make dev-db dev-redis   # the two services CI provides as containers
make ci-local           # every job above, in order
```

or `make verify` for the code gates alone, which is what to run before
every push; the scans and the image build are slower and rarely change
their answer for a code-only change.

**Toolchains.** CI installs the Go version from `go.mod` and the Rust
channel from `rust-toolchain.toml`, which is what `go` and `rustup` do
locally without being asked. The scanners (trivy, gitleaks, checkov,
kubeconform, kube-linter, kustomize, terraform) run in pinned containers
in both places, so their versions cannot differ between your machine and
CI. The one tool that runs natively is `govulncheck`, pinned by module
version in the Makefile and fetched by `go run`.

**Without the toolchains** (Windows, or a machine with Docker only), run
the code gates inside the dev container and the Docker-based jobs from the
host:

```bash
docker build -t vitalmesh-dev .devcontainer
docker run --rm --network vitalmesh -v "$(pwd):/workspace" \
  -v vitalmesh-target:/tmp/target -e CARGO_TARGET_DIR=/tmp/target \
  -e TEST_DATABASE_URL='postgres://vitalmesh:vitalmesh@postgres:5432/vitalmesh?sslmode=disable' \
  -e TEST_REDIS_URL='redis://redis:6379' \
  vitalmesh-dev make verify
```

`--network vitalmesh` is the compose network, so `postgres` and `redis`
resolve inside the container. In Git Bash set `MSYS_NO_PATHCONV=1` first.
The Docker-based targets (`deps-scan` is the exception: its govulncheck
half needs Go, so run it in the container too) work from the host with the
Docker socket, exactly as CI runs them.

**What CI caches, and why it is safe.** Go modules (keyed on `go.sum`),
the Cargo registry and target directory (keyed on `Cargo.lock` and the
toolchain; Cargo re-checks every fingerprint), image layers in the GitHub
Actions cache (`.github/docker-compose.ci-cache.yml`; a miss rebuilds the
same image), the trivy vulnerability database (trivy refreshes a stale
one itself), and the Terraform provider plugins (keyed on the lock files,
whose checksums `init` verifies). Every cache holds downloads or build
products whose staleness is detected by the tool that uses them; no test
result, scan verdict or rendered manifest is ever cached. Locally the
scanner and provider caches are the Docker volumes `vitalmesh-trivy` and
`vitalmesh-tf-plugins`; `TRIVY_CACHE` and `TF_PLUGIN_CACHE` point them
elsewhere, which is what CI does.

**The four workflows that touch AWS** (`terraform-plan.yml`,
`release.yml`, `deploy.yml`, `promote.yml`) are not reproducible locally as workflows:
they authenticate with short-lived OIDC tokens as roles that trust only
GitHub's own job identities, and they skip themselves with a notice until
the repository is configured. [GITHUB_CONFIGURATION.md](GITHUB_CONFIGURATION.md)
has that configuration; [DEPLOYMENT.md](DEPLOYMENT.md) describes the
staging pipeline, its timeouts and artifacts, and the rollback procedure.
The checks those workflows run (`scripts/smoke.sh`, `scripts/demo.sh`, the
image gates) run locally against `docker compose up`.

**When CI is red and local is green**, the difference is almost always one
of: the branch is behind `main` (CI merges the pull request first); a
scanner database updated overnight (a new advisory against an unchanged
lock file is a real finding, not flakiness); or the git history, which
`secret-scan` reads in full and a shallow clone lacks.

**Pins outside Dependabot's reach**, to review each quarter: the tool
images in the Makefile (`TRIVY_IMAGE`, `GITLEAKS_IMAGE`, `GOVULNCHECK`)
and in `scripts/k8s-validate.sh`, `scripts/tf-validate.sh` and
`.github/workflows/deploy.yml` (kustomize, kubeconform, kube-linter,
checkov, terraform, yq, actionlint), and the Helm charts in
`infrastructure/kubernetes/platform`. Bump the pin, run `make ci-local`,
triage anything new: a scanner update that surfaces a real finding is
the update doing its job. `GOVULNCHECK` runs through `go run`, so the
release's own `go` directive must be within the pinned toolchain; the
Makefile exports `GOTOOLCHAIN=local`, as CI does, so a release that
needs a newer Go fails locally too rather than fetching one.

**Coverage.** Every Go suite writes a profile with cross-package
accounting (`-coverpkg=./...`, so a handler exercised only end to end
still counts). `make test-go` holds the unit profile at `GO_COVERAGE_MIN`
(70%); `make coverage-go` merges unit, integration and E2E into
`services/api-gateway/coverage.out` and holds it at `GO_COVERAGE_MIN_ALL`
(85%). Both floors are ratchets: raise them when coverage rises, never
lower them to pass. `go tool cover -html=services/api-gateway/coverage.out`
shows what is uncovered.

**Do not fix a red gate by loosening it.** A suppression is a comment on
the finding with the reason, in the file that carries the finding
(`#checkov:skip`, `#trivy:ignore`, `.trivyignore.yaml` in
`infrastructure/kubernetes`), and it is reviewed like code.

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

## The stack suite

`make stack-test` is the end-to-end suite against the real containerized
application: the gateway and processor images built from the working
tree, PostgreSQL and Redis, started by `scripts/stack-test.sh` as a
compose project of its own (`vitalmesh-e2e`, gateway on port 18080,
PostgreSQL on 15433) with its volumes removed first, so every run starts
from an empty, freshly migrated database and never touches the
environment `make up` runs. The suite
(`services/api-gateway/tests/stack`, build tag `stack`) is a client of the
public API. It reads the database only to check what was persisted, and
drives `docker compose` to take services away.

It validates, in a fixed order: authentication, patient creation,
measurement and batch ingestion, processing (the job, the call from Go to
Rust, the results persisted and read back, the planted anomaly found),
authorization, idempotency, audit logging and rate limiting; then the
failure cases: invalid input, duplicate requests (sequential and
concurrent), the processor stopped, the processor hung (a timeout), the
database stopped and Redis stopped, each followed by recovery, with the
assertion that no container restarted and no job was left non-terminal.

Every value it sends is fixed and every wait polls for a condition with a
deadline, so a run either passes or names the assertion that failed. The
whole run takes about two minutes, most of it the failure cases waiting
for the gateway's own timeouts. `sh scripts/stack-test.sh up` starts the
stack and leaves it for a look around, `test` runs the suite against it,
`down` removes it; `E2E_KEEP=1` keeps a failed stack.

Without Go on the host, start the stack from the host and run the suite
in the dev container, which carries the Docker CLI and reaches the host's
daemon through the socket:

```bash
sh scripts/stack-test.sh up
docker run --rm --network vitalmesh-e2e -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$(pwd):/workspace" -w /workspace/services/api-gateway \
  -e E2E_GATEWAY_URL=http://api-gateway:8080 \
  -e E2E_DATABASE_URL='postgres://vitalmesh:vitalmesh@postgres:5432/vitalmesh?sslmode=disable' \
  -e E2E_COMPOSE_PROJECT=vitalmesh-e2e -e E2E_COMPOSE_FILE=/workspace/docker-compose.yml \
  -e VITALMESH_NETWORK=vitalmesh-e2e \
  vitalmesh-dev go test -tags stack -count=1 -timeout 25m -v ./tests/stack/...
sh scripts/stack-test.sh down
```

## The demo

```bash
make demo
```

Twelve steps, end to end: it starts the environment, creates an operator
account, signs in, registers a synthetic patient, generates and submits
sixty heart-rate readings with one deliberate spike, runs them through the
Rust engine, waits for the job, reads the statistics and anomalies back,
and then shows the metrics, the trace and the logs the run produced. Every
step prints what it sent and what came back. [DEMO.md](DEMO.md) walks
through it for a new developer.

It is safe to run repeatedly: each run uses a fresh patient reference and
fresh idempotency keys, and the account is created only if it is not
already there. `DEMO_NO_START=1 make demo` skips the start when the
environment is already up.

For more than one patient and one spike, `make synth-generate` and
`make synth-load` fill the environment with a seeded synthetic data set:
accounts, patients, every measurement type with noise and anomalies over a
configurable range. [SYNTHETIC_DATA.md](SYNTHETIC_DATA.md) has the flags,
the spec format and the safeguard that keeps it away from production.

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

### Publishing

`.github/workflows/release.yml` publishes the images for a commit once CI
has passed on `main`: it builds both, runs `docker-verify` and
`docker-scan` on exactly what it is about to push, then tags them
`sha-<commit>` and pushes to ECR. The scan runs before any registry
credential exists in the job; a HIGH or CRITICAL finding stops the job
there, and a stopped job pushes nothing. The repositories refuse to retag
(`IMMUTABLE` in Terraform), so a tag always means the image first pushed
with it, and there is no `latest`: the deployment pins the digests the
release job outputs, and checks the registry still holds them under that
tag before applying anything.

The build and the gates are the same commands locally, with the tag the
release job would use:

```bash
IMAGE_TAG="sha-$(git rev-parse HEAD)" make docker-build docker-verify docker-scan
```

That reproduces everything up to the push, which needs the account (the
push role, `docs/GITHUB_CONFIGURATION.md`) and is the one step a laptop
does not do. To see the gate refuse, scan something old:

```bash
GATEWAY_IMAGE=alpine:3.12 PROCESSOR_IMAGE=alpine:3.12 make docker-scan   # exits 1
```

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

Both services read `HTTP_ADDR` and expose `GET /health` (liveness) and `GET /ready` (readiness), returning `{"status":"ok","service":"...","version":"..."}`; the gateway adds `"environment"` (local, test, staging or production), which the synthetic data loader checks before writing anything (see [SYNTHETIC_DATA.md](SYNTHETIC_DATA.md)).

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
