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
TRIVY_IMAGE     ?= aquasec/trivy:0.58.2
TRIVY           := docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v vitalmesh-trivy:/root/.cache/trivy $(TRIVY_IMAGE)
TRIVY_FS        := docker run --rm -v vitalmesh-trivy:/root/.cache/trivy
TRIVY_SEVERITY  := --severity HIGH,CRITICAL --exit-code 1 --quiet
TRIVY_VULN      := --scanners vuln,secret,misconfig $(TRIVY_SEVERITY)

.PHONY: help setup format format-check lint test contracts-check contracts-lock integration-test e2e-test build line-endings verify clean dev-db dev-redis dev-db-down migrate observability-up observability-down observability-smoke docker-build docker-verify docker-scan k8s-validate k8s-local-test k8s-failure-test tf-validate up down demo

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

setup: ## Check prerequisites and download dependencies
	@command -v go >/dev/null 2>&1 || { echo "go is required (see docs/DEVELOPMENT.md)"; exit 1; }
	@command -v cargo >/dev/null 2>&1 || { echo "cargo is required (see docs/DEVELOPMENT.md)"; exit 1; }
	cd $(GO_DIR) && go mod download
	cd $(RUST_DIR) && cargo fetch --locked

format: ## Format Go and Rust sources in place
	cd $(GO_DIR) && gofmt -l -w .
	cd $(RUST_DIR) && cargo fmt

format-check: ## Fail if any source file is not formatted
	@cd $(GO_DIR) && files="$$(gofmt -l .)" && if [ -n "$$files" ]; then echo "Unformatted Go files:"; echo "$$files"; exit 1; fi
	cd $(RUST_DIR) && cargo fmt --check

lint: ## Run static analysis
	cd $(GO_DIR) && go vet ./... && go vet -tags integration ./...
	cd $(RUST_DIR) && cargo clippy --all-targets --locked -- -D warnings

test: ## Run unit tests
	cd $(GO_DIR) && go test -race ./...
	cd $(RUST_DIR) && cargo test --locked

contracts-check: ## Check the API contracts and that both services agree with them
	cd $(GO_DIR) && go test ./internal/contract/...
	cd $(RUST_DIR) && cargo test --locked --test contract

contracts-lock: ## Re-record a reviewed contract change in its lock file
	cd $(GO_DIR) && UPDATE_CONTRACT_LOCK=1 go test ./internal/contract/... -run TestContractMatchesItsLock

integration-test: ## Run integration tests against PostgreSQL and Redis (see make dev-db, make dev-redis)
	cd $(GO_DIR) && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -tags integration ./internal/infra/... ./internal/app/... ./internal/cache/... ./internal/ratelimit/...

e2e-test: ## Run cross-service tests: the real gateway against the real processor binary
	cd $(RUST_DIR) && cargo build --locked
	cd $(GO_DIR) && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" PROCESSOR_BINARY="$(RUST_TARGET_DIR)/debug/processor$(EXE)" go test -race -tags e2e ./tests/...

dev-db: ## Start the local PostgreSQL used by development and integration tests
	docker compose up -d --wait postgres

dev-redis: ## Start the local Redis used by development and integration tests
	docker compose up -d --wait redis

# One build definition, in docker-compose.yml, rather than two that drift
# apart. Compose owns the build arguments and the tags; this delegates so
# that what is verified and scanned is what `docker compose up` runs.
docker-build: ## Build the two service images
	IMAGE_TAG=$(IMAGE_TAG) VERSION=$(VERSION) docker compose build api-gateway processor

docker-verify: ## Check the images run as non-root, carry no shell and report healthy
	GATEWAY_IMAGE="$(GATEWAY_IMAGE)" PROCESSOR_IMAGE="$(PROCESSOR_IMAGE)" sh scripts/container-verify.sh

# Three scans, because none of them sees everything.
#
#   image   the operating system packages and the Go binary, which carries
#           its own module list
#   config  the two service Dockerfiles. It is scoped to them rather than
#           run over the repository, because .devcontainer/Dockerfile is a
#           toolchain image that runs as root on purpose and is never
#           shipped; failing this gate on it would train people to ignore it
#   source  the dependency lock files, which is the only place the Rust
#           crates are visible: unlike a Go binary, a Rust binary carries no
#           list of what went into it
docker-scan: ## Scan the images and the dependency lock files for known vulnerabilities
	@echo "== images: operating system packages and the Go binary =="
	$(TRIVY) image $(TRIVY_VULN) $(GATEWAY_IMAGE)
	$(TRIVY) image $(TRIVY_VULN) $(PROCESSOR_IMAGE)
	@echo "== the service Dockerfiles =="
	$(TRIVY_FS) -v "$(CURDIR)/$(GO_DIR):/scan" $(TRIVY_IMAGE) config $(TRIVY_SEVERITY) /scan
	$(TRIVY_FS) -v "$(CURDIR)/$(RUST_DIR):/scan" $(TRIVY_IMAGE) config $(TRIVY_SEVERITY) /scan
	@echo "== source: the dependency lock files, which cover the Rust crates =="
	$(TRIVY_FS) -v "$(CURDIR):/repo" $(TRIVY_IMAGE) fs --scanners vuln,secret $(TRIVY_SEVERITY) /repo

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

verify: format-check lint test contracts-check integration-test e2e-test build line-endings ## Run every quality gate (needs PostgreSQL: make dev-db)
	@echo "verify: all gates passed"

clean: ## Remove build outputs
	rm -rf $(BIN_DIR)
	cd $(RUST_DIR) && cargo clean
