SHELL := /bin/sh
.SHELLFLAGS := -eu -c
.DEFAULT_GOAL := help

GO_DIR    := services/api-gateway
RUST_DIR  := services/processor
BIN_DIR   := bin
GO_MODULE := github.com/n0ah-n0wa/VitalMesh/services/api-gateway
VERSION   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
GO_LDFLAGS := -X $(GO_MODULE)/internal/buildinfo.Version=$(VERSION)

.PHONY: help setup format format-check lint test build line-endings verify clean

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
	cd $(GO_DIR) && go vet ./...
	cd $(RUST_DIR) && cargo clippy --all-targets --locked -- -D warnings

test: ## Run all tests
	cd $(GO_DIR) && go test -race ./...
	cd $(RUST_DIR) && cargo test --locked

build: ## Build both services (Go binary in bin/, Rust binary in services/processor/target/)
	mkdir -p $(BIN_DIR)
	cd $(GO_DIR) && go build -ldflags "$(GO_LDFLAGS)" -o ../../$(BIN_DIR)/api-gateway ./cmd/api-gateway
	cd $(RUST_DIR) && VITALMESH_VERSION=$(VERSION) cargo build --locked

line-endings: ## Fail if any tracked file is stored with CRLF line endings
	sh scripts/check-line-endings.sh

verify: format-check lint test build line-endings ## Run every quality gate
	@echo "verify: all gates passed"

clean: ## Remove build outputs
	rm -rf $(BIN_DIR)
	cd $(RUST_DIR) && cargo clean
