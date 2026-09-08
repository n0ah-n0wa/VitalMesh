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

.PHONY: help setup format format-check lint test contracts-check contracts-lock integration-test e2e-test build line-endings verify clean dev-db dev-redis dev-db-down migrate

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

dev-db-down: ## Stop and remove the local infrastructure
	docker compose down

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
