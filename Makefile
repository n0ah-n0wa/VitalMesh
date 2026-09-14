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
GOVULNCHECK     ?= golang.org/x/vuln/cmd/govulncheck@v1.8.0
TRIVY_SEVERITY  := --severity HIGH,CRITICAL --exit-code 1 --quiet
TRIVY_VULN      := --scanners vuln,secret,misconfig $(TRIVY_SEVERITY)

.PHONY: help setup setup-go setup-rust format format-check format-check-go format-check-rust lint lint-go lint-rust test test-go test-rust contracts-check contracts-lock integration-test integration-test-postgres integration-test-redis e2e-test coverage-go build line-endings verify ci-local clean dev-db dev-redis dev-db-down migrate observability-up observability-down observability-smoke docker-build docker-verify docker-scan deps-scan secret-scan k8s-validate k8s-local-test k8s-failure-test tf-validate up down demo

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

lint-go: ## go vet, with and without the integration build tag
	cd $(GO_DIR) && go vet ./... && go vet -tags integration ./... && go vet -tags e2e ./...

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
	cd $(RUST_DIR) && cargo test --locked --test contract

contracts-lock: ## Re-record a reviewed contract change in its lock file
	cd $(GO_DIR) && UPDATE_CONTRACT_LOCK=1 go test ./internal/contract/... -run TestContractMatchesItsLock

# Split by the service each package needs, so CI can report "PostgreSQL"
# and "Redis" as separate steps and a developer with only one of them
# running can run that half. A new package with integration tests is added
# to exactly one of the two lists; the combined target is their union.
integration-test: integration-test-postgres integration-test-redis ## Run integration tests against PostgreSQL and Redis (see make dev-db, make dev-redis)

integration-test-postgres: ## Integration tests backed by PostgreSQL (the repository, the application layer, the processor client)
	cd $(GO_DIR) && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -tags integration $(GO_COVER) -coverprofile=coverage-integration-postgres.out ./internal/infra/postgres/... ./internal/infra/processorclient/... ./internal/app/...

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

# Static checks for the Terraform (fmt, validate, trivy, checkov), none of
# which needs an AWS account. Planning against a real account is separate.
tf-validate: ## Validate the Terraform without touching AWS (fmt, validate, trivy, checkov)
	sh scripts/tf-validate.sh

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

demo: ## Run the guided demo against the running environment
	sh scripts/demo.sh

observability-smoke: ## Check the running stack is scraping, recording and receiving spans
	sh scripts/observability-smoke.sh

dev-db-down: ## Stop the environment and discard its volumes
	docker compose down -v

migrate: ## Apply pending migrations to DATABASE_URL (defaults to the local database)
	cd $(GO_DIR) && DATABASE_URL="$${DATABASE_URL:-$(TEST_DATABASE_URL)}" go run ./cmd/api-gateway migrate up

build: ## Build both services (Go binary in bin/, Rust binary in services/processor/target/)
	mkdir -p $(BIN_DIR)
	cd $(GO_DIR) && go build -ldflags "$(GO_LDFLAGS)" -o ../../$(BIN_DIR)/api-gateway ./cmd/api-gateway
	cd $(RUST_DIR) && VITALMESH_VERSION=$(VERSION) cargo build --locked

line-endings: ## Fail if any tracked file is stored with CRLF line endings
	sh scripts/check-line-endings.sh

verify: format-check lint test contracts-check integration-test e2e-test coverage-go build line-endings ## Run every code quality gate (needs PostgreSQL and Redis: make dev-db dev-redis)
	@echo "verify: all gates passed"

# Everything CI runs, in the order CI's dependency graph would settle on if
# it ran serially. Needs Docker as well as the toolchains; see
# docs/DEVELOPMENT.md, "Reproducing CI locally".
ci-local: verify deps-scan secret-scan docker-build docker-verify docker-scan k8s-validate tf-validate ## Run every check CI runs, locally
	@echo "ci-local: every CI check passed"

clean: ## Remove build outputs
	rm -rf $(BIN_DIR)
	cd $(RUST_DIR) && cargo clean
