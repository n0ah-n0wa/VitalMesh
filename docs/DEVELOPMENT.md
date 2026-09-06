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
| `make test` | `go test -race ./...` and `cargo test` |
| `make build` | builds `bin/api-gateway` and `services/processor/target/debug/processor` |
| `make line-endings` | fails if any tracked file is stored with CRLF |
| `make verify` | `format-check lint test build line-endings` |
| `make clean` | removes build outputs |

Cargo runs with `--locked`, so `Cargo.lock` must be updated deliberately (`cargo update -p <crate>`) and committed.

## Running the services

Both services read `HTTP_ADDR` and expose `GET /health` (liveness) and `GET /ready` (readiness), returning `{"status":"ok","service":"...","version":"..."}`.

```bash
make build
./bin/api-gateway                                   # listens on :8080
./services/processor/target/debug/processor         # listens on 0.0.0.0:8081
curl localhost:8080/health
curl localhost:8081/ready
```

`version` is the short git SHA injected by the Makefile (`VERSION=...` overrides it), or `dev` when built directly with `go build` / `cargo build`.

The gateway's full configuration surface, package layout and layering rules are documented in [services/api-gateway/README.md](../services/api-gateway/README.md).

## Repository layout

See the structure table in the top-level [README.md](../README.md). Directories that are not yet populated contain a short `README.md` stating what will live there and which phase of [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) delivers it.
