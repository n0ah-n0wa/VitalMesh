# Cross-service tests

Suites that need more than one service running: `e2e/`, `integration/` (including contract tests), `load/` and `chaos/` (SPECIFICATIONS.md sections 44, 48–52). Service-local unit and integration tests live next to the code in `services/`.

Not yet populated; see `docs/IMPLEMENTATION_PLAN.md`, Phases 5 and 7.

The contract *documents* are already checked without running anything: `make contracts-check` validates them and confronts both services' types with them (see `contracts/internal-api/README.md`).

Two cross-service suites exist, both inside the gateway module for the sake of its types and database helpers:

- `services/api-gateway/tests/e2e`, run by `make e2e-test`: the fully wired gateway in process, a real PostgreSQL and the real processor binary as a separate process over a real socket.
- `services/api-gateway/tests/stack`, run by `make stack-test`: the real containerized stack (both images, PostgreSQL, Redis) started clean by `scripts/stack-test.sh`, driven purely over HTTP, with the failure cases injected by stopping, pausing and restarting containers. See "The stack suite" in [docs/DEVELOPMENT.md](../docs/DEVELOPMENT.md).
