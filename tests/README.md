# Cross-service tests

Suites that need more than one service running: `e2e/`, `integration/` (including contract tests), `load/` and `chaos/` (SPECIFICATIONS.md sections 44, 48–52). Service-local unit and integration tests live next to the code in `services/`.

Not yet populated; see `docs/IMPLEMENTATION_PLAN.md`, Phases 5 and 7.

The contract *documents* are already checked without running anything: `make contracts-check` validates them and confronts both services' types with them (see `contracts/internal-api/README.md`).

The first cross-service suite is `services/api-gateway/tests/e2e`, run by `make e2e-test`: the fully wired gateway, a real PostgreSQL and the real processor binary as a separate process over a real socket. It lives inside the gateway module because it wires the gateway in process, which needs that module's internal packages; a suite that drives both services purely over HTTP belongs here.
