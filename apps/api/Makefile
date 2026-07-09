# Orpheus API (Go) — convenience commands.
#
# Run `make help` for the list of available targets.

.DEFAULT_GOAL := help

# ─────────────────────────────────────────────────────────────────────
# Help
# ─────────────────────────────────────────────────────────────────────
.PHONY: help
help: ## Show this help message
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ─────────────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────────────
.PHONY: build
build: ## Compile the api binary to bin/api
	go build -o bin/api ./cmd/api

.PHONY: run
run: ## Run the api server (foreground)
	go run ./cmd/api

# ─────────────────────────────────────────────────────────────────────
# Quality
# ─────────────────────────────────────────────────────────────────────
.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: test-cov
test-cov: ## Run tests with coverage report
	go test -coverprofile=coverage.out ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Auto-format with gofmt + goimports
	golangci-lint fmt
	gofmt -s -w .

# ─────────────────────────────────────────────────────────────────────
# Modules
# ─────────────────────────────────────────────────────────────────────
.PHONY: tidy
tidy: ## Sync go.mod and go.sum
	go mod tidy

# ─────────────────────────────────────────────────────────────────────
# Cleanup
# ─────────────────────────────────────────────────────────────────────
.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin/ coverage.out
