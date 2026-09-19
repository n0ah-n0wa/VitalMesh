SHELL := /bin/sh
.SHELLFLAGS := -eu -c
.DEFAULT_GOAL := help

GO_DIR    := services/api-gateway
RUST_DIR  := services/processor
BIN_DIR   := bin
GO_MODULE := github.com/n0ah-n0wa/VitalMesh/services/api-gateway
VERSION   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
GO_LDFLAGS := -X $(GO_MODULE)/internal/buildinfo.Version=$(VERSION)
# Executable suffix, so the end-to-end target finds the processor on Windows
# as well as on Linux.
EXE       ?=

# CARGO_TARGET_DIR moves the Rust build output; the end-to-end target needs
# to find the binary wherever it landed.
RUST_TARGET_DIR ?= $(if $(CARGO_TARGET_DIR),$(CARGO_TARGET_DIR),$(CURDIR)/$(RUST_DIR)/target)

# PostgreSQL used by the database integration tests; `make dev-db` starts one.
# Tests create and drop their own databases on this server.
TEST_DATABASE_URL ?= postgres://vitalmesh:vitalmesh@localhost:5432/vitalmesh?sslmode=disable

# Redis used by the tests that exercise rate limiting, caching and the
# idempotency lock; `make dev-redis` starts one. Tests namespace their own
# keys, so one server serves them all.
TEST_REDIS_URL ?= redis://localhost:6379

# Container images.
#
# The tag defaults to `dev`, which is what docker-compose.yml builds, so
# every target here acts on the images the local environment actually runs.
# They used to disagree: the Makefile tagged by commit and compose tagged
# `dev`, so `make docker-verify` checked images nobody was running. Set
# IMAGE_TAG to publish under a commit or a release instead; the binary is
# stamped with VERSION either way.
IMAGE_TAG       ?= dev
# The two synthetic releases the rollback rehearsal rolls between; they
# only have to be two builds that can be told apart (docs/ROLLBACK.md).
ROLLBACK_RELEASES ?= a1b2c3d e4f5a6b
GATEWAY_IMAGE   ?= vitalmesh/api-gateway:$(IMAGE_TAG)
PROCESSOR_IMAGE ?= vitalmesh/processor:$(IMAGE_TAG)
# Pinned: a scanner that changes under you turns a clean run into a failure
# that has nothing to do with the code.
# `docker compose` by default. CI adds an override file that turns on the
# GitHub Actions layer cache (.github/docker-compose.ci-cache.yml); the
# build definition itself stays in docker-compose.yml either way.
COMPOSE         ?= docker compose

# Scanner and provider caches: Docker named volumes locally, so they persist
# between runs without touching the checkout; CI points these at directories
# it restores from its cache instead. Only caches live here: nothing that
# could change a result.
TRIVY_CACHE     ?= vitalmesh-trivy
TF_PLUGIN_CACHE ?= vitalmesh-tf-plugins
export TRIVY_CACHE TF_PLUGIN_CACHE

# The Go toolchain is exactly the one installed, never one downloaded on
# demand: actions/setup-go sets this in CI, so setting it here too means a
# tool or module that needs a newer Go fails the same way locally as it
# does in CI, instead of passing locally on a silently fetched toolchain.
export GOTOOLCHAIN ?= local

# Coverage floors for the Go service, as percentages of statements, checked
# by scripts/coverage-floor.sh. GO_COVERAGE_MIN applies to the unit suite
# alone (make test-go); GO_COVERAGE_MIN_ALL to the unit, integration and
# end-to-end profiles merged (make coverage-go), which is what the code is
# actually covered by. Measured at 71.0% and 87.1% when introduced; the
# floors only ever go up.
GO_COVERAGE_MIN     ?= 70
GO_COVERAGE_MIN_ALL ?= 85
GO_COVER            := -coverpkg=./... -covermode=atomic

TRIVY_IMAGE     ?= aquasec/trivy:0.74.0
TRIVY           := docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v $(TRIVY_CACHE):/root/.cache/trivy $(TRIVY_IMAGE)
TRIVY_FS        := docker run --rm -v $(TRIVY_CACHE):/root/.cache/trivy
GITLEAKS_IMAGE  ?= ghcr.io/gitleaks/gitleaks:v8.30.1
# govulncheck runs through `go run`, so its own go directive must be within
# the pinned toolchain (go.mod): v1.8.0 requires Go 1.26 and fails under
# GOTOOLCHAIN=local. Check the directive before bumping this pin.
GOVULNCHECK     ?= golang.org/x/vuln/cmd/govulncheck@v1.7.0
# gosec reads this repository's own code rather than its dependencies, which
# is the half govulncheck does not cover. Pinned for the same reason and with
# the same caveat: the release must build under the toolchain in go.mod.
GOSEC           ?= github.com/securego/gosec/v2/cmd/gosec@v2.22.9
# Where `make sbom` writes. Git-ignored: an SBOM describes one build of one
# image, so it is an artifact of a run rather than a file to keep in the
# tree. CI uploads it; the release attaches it to the images it describes.
SBOM_DIR        ?= sbom
TRIVY_SEVERITY  := --severity HIGH,CRITICAL --exit-code 1 --quiet
TRIVY_VULN      := --scanners vuln,secret,misconfig $(TRIVY_SEVERITY)

.PHONY: help setup setup-go setup-rust format format-check format-check-go format-check-rust lint lint-go lint-rust test test-go test-rust contracts-check contracts-lock integration-test integration-test-postgres integration-test-redis e2e-test coverage-go build release-metadata line-endings verify ci-local clean dev-db dev-redis dev-db-down migrate observability-up observability-down observability-smoke docker-build docker-verify docker-scan sbom sast policy-check db-restore-test deps-verify deps-verify-go deps-verify-rust deps-scan secret-scan k8s-validate k8s-local-test k8s-failure-test k8s-resilience-test rollback-images rollback-test tf-validate up down dev demo synth-generate synth-load stack-test load-test perf-baseline bench bench-go bench-rust smoke

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

setup: ## Check prerequisites and download dependencies
	@command -v go >/dev/null 2>&1 || { echo "go is required (see docs/DEVELOPMENT.md)"; exit 1; }
	@command -v cargo >/dev/null 2>&1 || { echo "cargo is required (see docs/DEVELOPMENT.md)"; exit 1; }
	$(MAKE) setup-go setup-rust

# The gates below come in pairs, one per language, and a combined target
# that runs both. CI runs the halves as separate jobs so that a Go failure
# and a Rust failure are reported at the same time; `make verify` runs the
# combined ones. Both routes run identical commands.
setup-go: ## Download the Go modules
	cd $(GO_DIR) && go mod download

setup-rust: ## Download the Cargo crates, exactly as locked
	cd $(RUST_DIR) && cargo fetch --locked

format: ## Format Go and Rust sources in place
	cd $(GO_DIR) && gofmt -l -w .
	cd $(RUST_DIR) && cargo fmt

format-check: format-check-go format-check-rust ## Fail if any source file is not formatted

format-check-go: ## Fail if any Go file is not gofmt-formatted
	@cd $(GO_DIR) && files="$$(gofmt -l .)" && if [ -n "$$files" ]; then echo "Unformatted Go files:"; echo "$$files"; exit 1; fi

format-check-rust: ## Fail if any Rust file is not rustfmt-formatted
	cd $(RUST_DIR) && cargo fmt --check

lint: lint-go lint-rust ## Run static analysis

lint-go: ## go vet, with every build tag
	cd $(GO_DIR) && go vet ./... && go vet -tags integration ./... && go vet -tags e2e ./... && go vet -tags stack ./...

lint-rust: ## cargo clippy on every target, warnings are errors
	cd $(RUST_DIR) && cargo clippy --all-targets --locked -- -D warnings

test: test-go test-rust ## Run unit tests

test-go: ## Go unit tests with the race detector, and the unit coverage floor
	cd $(GO_DIR) && go test -race $(GO_COVER) -coverprofile=coverage-unit.out ./...
	cd $(GO_DIR) && sh ../../scripts/coverage-floor.sh coverage-unit.out $(GO_COVERAGE_MIN) unit

test-rust: ## Rust unit and integration tests
	cd $(RUST_DIR) && cargo test --locked

contracts-check: ## Check the API contracts and that both services agree with them
	cd $(GO_DIR) && go test ./internal/contract/...
	cd $(GO_DIR) && go test ./internal/httpapi/ -run 'PublicContract|UnservedAuthorizationRules'
	cd $(RUST_DIR) && cargo test --locked --test contract

contracts-lock: ## Re-record a reviewed contract change in its lock file
	cd $(GO_DIR) && UPDATE_CONTRACT_LOCK=1 go test ./internal/contract/... -run 'ContractMatchesItsLock'

# Split by the service each package needs, so CI can report "PostgreSQL"
# and "Redis" as separate steps and a developer with only one of them
# running can run that half. A new package with integration tests is added
# to exactly one of the two lists; the combined target is their union.
integration-test: integration-test-postgres integration-test-redis ## Run integration tests against PostgreSQL and Redis (see make dev-db, make dev-redis)

integration-test-postgres: ## Integration tests backed by PostgreSQL (the repository, the application layer, the processor client)
	cd $(GO_DIR) && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -tags integration $(GO_COVER) -coverprofile=coverage-integration-postgres.out ./internal/infra/postgres/... ./internal/infra/processorclient/... ./internal/app/... ./internal/synth/...

integration-test-redis: ## Integration tests backed by Redis (the client, the cache, rate limiting)
	cd $(GO_DIR) && TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -tags integration $(GO_COVER) -coverprofile=coverage-integration-redis.out ./internal/infra/redisclient/... ./internal/cache/... ./internal/ratelimit/...

e2e-test: ## Run cross-service tests: the real gateway against the real processor binary
	cd $(RUST_DIR) && cargo build --locked
	cd $(GO_DIR) && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" PROCESSOR_BINARY="$(RUST_TARGET_DIR)/debug/processor$(EXE)" go test -race -tags e2e $(GO_COVER) -coverprofile=coverage-e2e.out ./tests/...

# The unit, integration and end-to-end profiles merged: what the code is
# covered by once every suite has run. Fails below GO_COVERAGE_MIN_ALL.
coverage-go: ## Merge the Go coverage profiles from every suite and check the merged floor
	cd $(GO_DIR) && sh ../../scripts/coverage-floor.sh merge coverage.out coverage-unit.out coverage-integration-postgres.out coverage-integration-redis.out coverage-e2e.out
	cd $(GO_DIR) && sh ../../scripts/coverage-floor.sh coverage.out $(GO_COVERAGE_MIN_ALL) "unit + integration + e2e"

dev-db: ## Start the local PostgreSQL used by development and integration tests
	docker compose up -d --wait postgres

dev-redis: ## Start the local Redis used by development and integration tests
	docker compose up -d --wait redis

# One build definition, in docker-compose.yml, rather than two that drift
# apart. Compose owns the build arguments and the tags; this delegates so
# that what is verified and scanned is what `docker compose up` runs.
docker-build: ## Build the two service images
	IMAGE_TAG=$(IMAGE_TAG) VERSION=$(VERSION) $(COMPOSE) build api-gateway processor

docker-verify: ## Check the images run as non-root, carry no shell and report healthy
	GATEWAY_IMAGE="$(GATEWAY_IMAGE)" PROCESSOR_IMAGE="$(PROCESSOR_IMAGE)" sh scripts/container-verify.sh

# Two scans of what ships, because neither sees everything.
#
#   image   the operating system packages and the Go binary, which carries
#           its own module list
#   config  the two service Dockerfiles. It is scoped to them rather than
#           run over the repository, because .devcontainer/Dockerfile is a
#           toolchain image that runs as root on purpose and is never
#           shipped; failing this gate on it would train people to ignore it
#
# The dependency lock files are scanned by deps-scan, which does not need
# the images and so runs earlier and in parallel in CI.
docker-scan: ## Scan the built images and the service Dockerfiles, failing on HIGH or CRITICAL
	@echo "== images: operating system packages and the Go binary =="
	$(TRIVY) image $(TRIVY_VULN) $(GATEWAY_IMAGE)
	$(TRIVY) image $(TRIVY_VULN) $(PROCESSOR_IMAGE)
	@echo "== the service Dockerfiles =="
	$(TRIVY_FS) -v "$(CURDIR)/$(GO_DIR):/scan" $(TRIVY_IMAGE) config $(TRIVY_SEVERITY) /scan
	$(TRIVY_FS) -v "$(CURDIR)/$(RUST_DIR):/scan" $(TRIVY_IMAGE) config $(TRIVY_SEVERITY) /scan

# A software bill of materials per image, in CycloneDX (SPECIFICATIONS.md
# section 99). It is generated from the built image rather than from the
# lock files, so it lists what actually shipped: the operating system
# packages of the base layer as well as the Go modules the binary carries.
# The Rust binary carries no module list, which is why the image is the only
# place the processor's dependencies are visible at all.
#
# Written per image and named by the tag, so two tags do not overwrite each
# other. Generating an SBOM is not a gate: it reports, and the gate that
# blocks is docker-scan.
sbom: ## Generate a CycloneDX SBOM for each built image into $(SBOM_DIR)
	@mkdir -p $(SBOM_DIR)
	$(TRIVY) image --quiet --format cyclonedx --output /dev/stdout $(GATEWAY_IMAGE) > $(SBOM_DIR)/api-gateway-$(IMAGE_TAG).cdx.json
	$(TRIVY) image --quiet --format cyclonedx --output /dev/stdout $(PROCESSOR_IMAGE) > $(SBOM_DIR)/processor-$(IMAGE_TAG).cdx.json
	@for f in $(SBOM_DIR)/*.cdx.json; do 		components=$$(grep -o '"bom-ref"' "$$f" | wc -l); 		echo "$$f: $$components components"; 		[ "$$components" -gt 0 ] || { echo "$$f lists no components"; exit 1; }; 	done

# Lockfile integrity, which is a different question from whether the locked
# versions are vulnerable (deps-scan) or current (Dependabot).
#
#   go mod verify      every module in the cache still hashes to what
#                      go.sum records, so a tampered or swapped module is
#                      refused rather than built
#   go mod tidy -diff  go.mod and go.sum describe exactly what the code
#                      imports: no stale requirement that a scanner would
#                      keep reporting, and nothing missing that a build
#                      would resolve at random
#   cargo fetch        --locked refuses to proceed if Cargo.lock would have
#                      to change, which is the same guarantee for Rust and
#                      is already how every cargo target here runs
# The gates enforce the policy; this checks the gates still say what the
# policy says. Every threshold here is one flag away from passing
# everything, so a weakened flag is itself a finding. Needs no toolchain.
policy-check: ## Check the security gates still match the documented policy (docs/SECURITY.md)
	sh scripts/security-policy-check.sh

# The restore test of sections 79 and 80: a base backup, archived
# write-ahead log, and point-in-time recovery of the real schema, verified
# row by row. It drives PostgreSQL's own machinery, which is what RDS wraps,
# so it proves the procedure and the verification queries -- not RDS's
# provisioning time. docs/DISASTER_RECOVERY.md records what it measured and
# what remains an assumption. Needs the api-gateway image (make
# docker-build) because the schema comes from the real migrator.
db-restore-test: ## Point-in-time restore test against a real PostgreSQL, verified and timed
	sh scripts/db-restore-test.sh

deps-verify: deps-verify-go deps-verify-rust ## Check both lockfiles are intact and describe exactly what is imported

deps-verify-go: ## go.sum hashes match the module cache, and go.mod is tidy
	cd $(GO_DIR) && go mod verify
	cd $(GO_DIR) && go mod tidy -diff

deps-verify-rust: ## Cargo.lock needs no change to build what is locked
	cd $(RUST_DIR) && cargo fetch --locked

# Two views of the dependencies, because they answer different questions.
#
#   trivy        every crate in Cargo.lock and every module in go.sum
#                against the vulnerability databases: the only place the
#                Rust crates are visible, since a Rust binary carries no
#                list of what went into it
#   govulncheck  the Go module graph against the Go vulnerability database,
#                reported only where the vulnerable function is actually
#                reachable from this code, so a finding here is one to act
#                on rather than one to argue about
# Static application security testing of this repository's own Go code:
# hard-coded credentials, weak randomness, unsafe integer conversions, file
# permissions, path handling. Findings that are deliberate carry an inline
# `#nosec <rule> -- <reason>`; docs/SECURITY.md lists them and argues each.
# Medium severity and above, which is what a reviewer can act on.
sast: ## Static application security analysis of the Go service (gosec)
	cd $(GO_DIR) && go run $(GOSEC) -severity=medium -confidence=medium -quiet ./...

deps-scan: ## Scan the dependency lock files (trivy) and the reachable Go call graph (govulncheck) for known vulnerabilities
	@echo "== lock files: Cargo.lock and go.sum =="
	$(TRIVY_FS) -v "$(CURDIR):/repo" $(TRIVY_IMAGE) fs --scanners vuln $(TRIVY_SEVERITY) /repo
	@echo "== govulncheck: vulnerable code reachable from the gateway =="
	cd $(GO_DIR) && go run $(GOVULNCHECK) ./...

# Every commit that has ever been made, not just the working tree: a secret
# that was committed and then removed is still in the history, and still
# leaked. Runs as root inside its container, so git is told the mounted
# checkout is safe to read.
secret-scan: ## Scan the whole git history and the working tree for committed secrets (gitleaks)
	docker run --rm -v "$(CURDIR):/repo" -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/repo $(GITLEAKS_IMAGE) git --config /repo/.gitleaks.toml --redact --exit-code 1 /repo
	docker run --rm -v "$(CURDIR):/repo" $(GITLEAKS_IMAGE) dir --config /repo/.gitleaks.toml --redact --exit-code 1 /repo

# Five tools in one script, none of which needs a cluster; the script says
# what each one is for. No Kubernetes toolchain is installed locally, so
# every tool runs in a pinned container.
k8s-validate: ## Validate the manifests (kustomize, kubeconform, kube-linter, trivy, checkov)
	sh scripts/k8s-validate.sh

# The other half of validation, and the half that needs a cluster: applies
# the local overlay to kind and exercises it. Needs kind and kubectl on
# PATH, which is the one place this repository asks for a tool outside
# Docker — kind builds its node as a container on the host's Docker and so
# cannot run inside one.
k8s-local-test: k8s-validate ## Deploy the local overlay to a kind cluster and test it
	sh scripts/k8s-local-test.sh

k8s-monitoring-test: k8s-validate ## Deploy the monitoring stack to a kind cluster and test scrape, rules and alert delivery
	sh scripts/k8s-monitoring-test.sh

# Controlled failure scenarios. Section 52 asks for these and section 90
# says what each dependency should do; docs/FAILURE_MODES.md records what
# they actually do.
#
# This needs a cluster that is still up, which `make k8s-local-test` does
# not leave behind — that target tears its cluster down so it can be run on
# its own. Deploy with the script directly first:
#
#   sh scripts/k8s-local-test.sh --keep
#   make k8s-failure-test
#
# The script says so too, and refuses to run against nothing.
k8s-failure-test: ## Break each dependency in turn and check the behaviour
	sh scripts/k8s-failure-test.sh

# The production-style resilience test: a three-node kind cluster, two
# replicas of each service and a metrics-server, so a PodDisruptionBudget, a
# node drain and an HPA all have something to act on. It owns its own
# cluster (vitalmesh-ha) and creates and destroys it, so unlike
# k8s-failure-test it needs no cluster deployed first. docs/OPERATIONS.md
# records what it observes.
#
#   sh scripts/k8s-resilience-test.sh --keep   to leave the cluster up
k8s-resilience-test: ## Production-style resilience test on a 3-node kind cluster (drain, PDB, HPA, outages)
	sh scripts/k8s-resilience-test.sh

# The rollback rehearsal. It deploys two releases by digest and rolls one
# back, so it needs a registry beside the cluster (the kind config says why)
# and two builds of each service, which the target below produces.
rollback-images: ## Build the two releases the rollback rehearsal rolls between
	for v in $(ROLLBACK_RELEASES); do for s in api-gateway processor; do docker build --build-arg VERSION=$$v -t vitalmesh/$$s:sha-$$v services/$$s; done; done

rollback-test: ## Rehearse the production rollback on a kind cluster and verify every part of it (docs/ROLLBACK.md)
	sh scripts/rollback-test.sh

# Static checks for the Terraform (fmt, validate, trivy, checkov), none of
# which needs an AWS account. Planning against a real account is separate.
tf-validate: ## Validate the Terraform without touching AWS (fmt, validate, trivy, checkov)
	sh scripts/tf-validate.sh

# SPECIFICATIONS.md section 111 names `make dev` in the local workflow. It
# is the same thing as `make up`, which the rest of the documentation uses;
# the alias exists so the workflow the specification writes out can be
# followed literally.
dev: up ## Start the whole local environment (an alias for make up)

up: ## Start the whole local environment (the same as: docker compose up -d --wait)
	docker compose up -d --wait
	@echo "Gateway    http://localhost:8080"
	@echo "Grafana    http://localhost:3000"
	@echo "Demo       ./scripts/demo.sh"

down: ## Stop the environment, keeping the database volume
	docker compose down

observability-up: ## Start only Prometheus, Grafana and the OpenTelemetry collector
	docker compose up -d --wait prometheus grafana otel-collector
	@echo "Grafana    http://localhost:3000"
	@echo "Prometheus http://localhost:9090"
	@echo "Collector  OTLP/HTTP on http://localhost:4318 (set OTEL_EXPORTER_OTLP_ENDPOINT to it)"

observability-down: ## Stop the observability stack, leaving the services running
	docker compose rm -sf prometheus grafana otel-collector

demo: ## The twelve-step demonstration: starts the environment and runs the whole system end to end (docs/DEMO.md)
	sh scripts/demo.sh

# The end-to-end suite against the real containerized stack: a clean compose
# project (vitalmesh-e2e, its own ports and network) is built and started,
# the suite runs against it over HTTP and takes services away for the
# failure cases, and the project is removed. Needs Docker and Go.
stack-test: ## Start a clean containerized stack, run the end-to-end suite against it, remove it
	COMPOSE="$(COMPOSE)" sh scripts/stack-test.sh

# Load test with k6 in a pinned container against the local environment;
# the report lands in tests/load/results/ (docs/LOAD_TESTING.md).
LOAD_PROFILE ?= standard

load-test: ## Run the k6 load test (LOAD_PROFILE=smoke|standard) and write the report to tests/load/results/
	PROFILE=$(LOAD_PROFILE) sh scripts/load-test.sh

perf-baseline: ## Measure the performance baseline (RESET=1 for an empty database first); report in tests/load/results/ (docs/PERFORMANCE_BASELINE.md)
	sh scripts/perf-baseline.sh

# Micro-benchmarks, as distinct from the load tests above: these measure one
# function, those measure the system. Section 107 asks for both to be
# reachable, and `make help` is where a developer looks.
bench: bench-go bench-rust ## Run the Go and Rust micro-benchmarks

bench-go: ## Go benchmarks (needs PostgreSQL for the repository ones: make dev-db)
	cd $(GO_DIR) && TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$(TEST_DATABASE_URL)}" 		go test -tags integration -run '^$$' -bench . -benchmem ./...

bench-rust: ## Rust Criterion benchmarks (services/processor/benches)
	cd $(RUST_DIR) && cargo bench --locked

smoke: ## Smoke-test a deployed gateway (GATEWAY_URL=https://...), as release.yml does after a deploy
	sh scripts/smoke.sh

# Synthetic data (SPECIFICATIONS.md sections 106 and 107; docs/SYNTHETIC_DATA.md).
# SYNTH_ARGS passes flags through: make synth-generate SYNTH_ARGS="--seed 7 --patients 50 --days 30"
SYNTH_DIR  ?= .synth/default
SYNTH_ARGS ?=

synth-generate: ## Write a synthetic fixture to SYNTH_DIR (flags in SYNTH_ARGS; see docs/SYNTHETIC_DATA.md)
	cd $(GO_DIR) && go run ./cmd/synth generate --out ../../$(SYNTH_DIR) --overwrite $(SYNTH_ARGS)

synth-load: ## Load SYNTH_DIR into the local environment (make up): creates its accounts, patients, readings and jobs
	cd $(GO_DIR) && DATABASE_URL="$${DATABASE_URL:-$(TEST_DATABASE_URL)}" go run ./cmd/synth load --from ../../$(SYNTH_DIR) \
		--target "$${GATEWAY_URL:-http://localhost:8080}" --environment local --users --jobs $(SYNTH_ARGS)

observability-smoke: ## Check the running stack is scraping, recording and receiving spans
	sh scripts/observability-smoke.sh

dev-db-down: ## Stop the environment and discard its volumes
	docker compose down -v

migrate: ## Apply pending migrations to DATABASE_URL (defaults to the local database)
	cd $(GO_DIR) && DATABASE_URL="$${DATABASE_URL:-$(TEST_DATABASE_URL)}" go run ./cmd/api-gateway migrate up

build: ## Build both services and the synth tool (Go binaries in bin/, Rust binary in services/processor/target/)
	mkdir -p $(BIN_DIR)
	cd $(GO_DIR) && go build -ldflags "$(GO_LDFLAGS)" -o ../../$(BIN_DIR)/api-gateway ./cmd/api-gateway
	cd $(GO_DIR) && go build -ldflags "$(GO_LDFLAGS)" -o ../../$(BIN_DIR)/synth ./cmd/synth
	cd $(RUST_DIR) && VITALMESH_VERSION=$(VERSION) cargo build --locked

release-metadata: build ## Check that this build identifies itself: commit, toolchains, lockfiles, migration and algorithm versions (docs/RELEASE.md)
	sh scripts/release-metadata-check.sh

line-endings: ## Fail if any tracked file is stored with CRLF line endings
	sh scripts/check-line-endings.sh

verify: format-check lint deps-verify test contracts-check integration-test e2e-test coverage-go build release-metadata line-endings ## Run every code quality gate (needs PostgreSQL and Redis: make dev-db dev-redis)
	@echo "verify: all gates passed"

# Everything CI runs, in the order CI's dependency graph would settle on if
# it ran serially. Needs Docker as well as the toolchains; see
# docs/DEVELOPMENT.md, "Reproducing CI locally".
ci-local: verify policy-check sast deps-scan secret-scan docker-build docker-verify docker-scan sbom db-restore-test stack-test k8s-validate tf-validate ## Run every check CI runs, locally
	@echo "ci-local: every CI check passed"

clean: ## Remove build outputs
	rm -rf $(BIN_DIR)
	cd $(RUST_DIR) && cargo clean
