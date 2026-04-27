.DEFAULT_GOAL := help

# /tmp is mounted noexec on some workstations; redirect Go's test build dir.
GOTMPDIR ?= $(HOME)/.cache/go-tmp
export GOTMPDIR

GO       ?= go
PKG      := ./...
BIN_DIR  := bin
BIN_NAME := vcluster-manager

# Read VERSION lazily so a missing file doesn't break Makefile parsing.
VERSION  = $(shell cat VERSION 2>/dev/null || echo dev)
LDFLAGS  = -X github.com/gmalfray/vcluster-manager/internal/version.Version=$(VERSION)

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Compile the server binary into ./bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_NAME) ./cmd/server

.PHONY: run
run: ## Run the server locally
	$(GO) run ./cmd/server

.PHONY: test
test: | $(GOTMPDIR) ## Run tests
	$(GO) test -race -count=1 $(PKG)

.PHONY: test-short
test-short: | $(GOTMPDIR) ## Run tests without -race (faster)
	$(GO) test -count=1 $(PKG)

.PHONY: coverage
coverage: | $(GOTMPDIR) ## Generate coverage report (coverage.html)
	$(GO) test -race -count=1 -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKG)

.PHONY: fmt
fmt: ## Format code (gofmt + goimports if available)
	$(GO) fmt $(PKG)
	@if command -v goimports >/dev/null 2>&1; then \
		goimports -w -local github.com/gmalfray/vcluster-manager .; \
	fi

.PHONY: lint
lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 \
		|| { echo "golangci-lint not found; install with: $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix
	golangci-lint run --fix

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy

.PHONY: check
check: vet test lint ## Run vet + tests + lint

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.out coverage.html

$(GOTMPDIR):
	@mkdir -p $(GOTMPDIR)
