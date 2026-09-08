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
| `make dev-db-down` | stop and remove the local infrastructure |
| `make integration-test` | integration tests against `TEST_DATABASE_URL` and `TEST_REDIS_URL` (default: the local PostgreSQL and Redis) |
| `make e2e-test` | cross-service tests: the real gateway against the real processor binary, which it builds first |
| `make migrate` | apply migrations to `DATABASE_URL` (default: the local PostgreSQL) |
| `make build` | builds `bin/api-gateway` and `services/processor/target/debug/processor` |
| `make line-endings` | fails if any tracked file is stored with CRLF |
| `make verify` | `format-check lint test contracts-check integration-test e2e-test build line-endings`; needs the local PostgreSQL and Redis (`make dev-db`, `make dev-redis`) |
| `make clean` | removes build outputs |

Cargo runs with `--locked`, so `Cargo.lock` must be updated deliberately (`cargo update -p <crate>`) and committed.

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
