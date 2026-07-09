# Orpheus — convenience commands.
#
# Polyglot monorepo:
#   - Go: apps/api/         (public API tier)
#   - Python: apps/workers/ (worker tier, Phase 2) + packages/contracts/
#
# Run `make help` for a list of available targets.

.DEFAULT_GOAL := help

GO_DIR       := apps/api
WORKERS_DIR  := apps/workers
PY_SOURCES   := apps packages

# ─────────────────────────────────────────────────────────────────────
# Help
# ─────────────────────────────────────────────────────────────────────
.PHONY: help
help: ## Show this help message
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ─────────────────────────────────────────────────────────────────────
# Setup
# ─────────────────────────────────────────────────────────────────────
.PHONY: install
install: install-py install-go ## Install all dependencies (Python + Go)

.PHONY: install-py
install-py: ## Install Python dependencies via uv
	uv sync --all-packages

.PHONY: install-go
install-go: ## Download and tidy Go modules for apps/api
	cd $(GO_DIR) && go mod download && go mod tidy

.PHONY: install-pre-commit
install-pre-commit: ## Install pre-commit hooks
	uv tool run pre-commit install

.PHONY: bootstrap
bootstrap: install install-pre-commit ## First-time setup: install deps + pre-commit
	@echo "✓ Orpheus bootstrapped. Run 'make dev' to start."

# ─────────────────────────────────────────────────────────────────────
# Dev
# ─────────────────────────────────────────────────────────────────────
.PHONY: dev
dev: api-dev ## Run the API server (Go, with hot reload)

.PHONY: api-dev
api-dev: ## Run the Go API server
	cd $(GO_DIR) && go run ./cmd/api

.PHONY: up
up: infra-up ## Start the full local stack (alias for infra-up)

.PHONY: down
down: infra-down ## Stop the local stack (alias for infra-down)

.PHONY: nuke
nuke: infra-reset ## Wipe all infra volumes (alias for infra-reset)

.PHONY: infra-up
infra-up: ## Start local stack (Postgres, Redis, MinIO, Keycloak, NATS) via docker compose
	docker compose up -d
	@echo ""
	@echo "✓ Local stack up."
	@echo "    Postgres  :5432       (orpheus / orpheus)"
	@echo "    Redis     :6379"
	@echo "    MinIO     :9000       (console :9001, orpheus / orpheus-dev-secret)"
	@echo "    Keycloak  :8088       (admin console, admin / admin)"
	@echo "    NATS      :4222       (HTTP monitor :8222)"

.PHONY: infra-down
infra-down: ## Stop local stack
	docker compose down

.PHONY: infra-logs
infra-logs: ## Follow docker compose logs
	docker compose logs -f --tail=100

.PHONY: infra-reset
infra-reset: ## Stop local stack AND wipe volumes (DESTRUCTIVE)
	docker compose down -v
	@echo "✓ Volumes wiped. Next 'make up' will re-init Postgres, Keycloak, NATS."

# ─────────────────────────────────────────────────────────────────────
# Quality — combined (Python + Go)
# ─────────────────────────────────────────────────────────────────────
.PHONY: lint
lint: py-lint api-lint ## Run linters (Python + Go)

.PHONY: format
format: py-format go-fmt ## Auto-format all code

.PHONY: format-check
format-check: py-format-check go-fmt-check ## Check formatting without modifying

.PHONY: type-check
type-check: py-type-check ## Run pyright (Python only — Go type-checks at build time)

.PHONY: test
test: py-test api-test ## Run all tests (Python + Go)

.PHONY: test-cov
test-cov: py-test-cov api-test-cov ## Run all tests with coverage

.PHONY: pre-commit
pre-commit: ## Run pre-commit on all files
	uv tool run pre-commit run --all-files

.PHONY: check
check: lint format-check type-check test ## Run all checks (CI-equivalent)

# ─────────────────────────────────────────────────────────────────────
# Quality — Python
# ─────────────────────────────────────────────────────────────────────
.PHONY: py-lint
py-lint: ## Ruff (Python lint)
	uv run ruff check . --output-format=github

.PHONY: py-format
py-format: ## Ruff format (Python)
	uv run ruff format .

.PHONY: py-format-check
py-format-check: ## Ruff format check (Python)
	uv run ruff format --check .

.PHONY: py-type-check
py-type-check: ## Pyright (Python type-check)
	uv run pyright

.PHONY: py-test
py-test: ## Pytest (Python tests)
	uv run pytest

.PHONY: py-test-cov
py-test-cov: ## Pytest with coverage
	uv run pytest --cov=$(PY_SOURCES) --cov-report=term-missing

# ─────────────────────────────────────────────────────────────────────
# Quality — Go (apps/api)
# ─────────────────────────────────────────────────────────────────────
.PHONY: api-lint
api-lint: ## golangci-lint (Go)
	cd $(GO_DIR) && golangci-lint run ./...

.PHONY: api-vet
api-vet: ## go vet (Go)
	cd $(GO_DIR) && go vet ./...

.PHONY: go-fmt
go-fmt: ## gofmt (Go)
	cd $(GO_DIR) && gofmt -w .

.PHONY: go-fmt-check
go-fmt-check: ## gofmt check (Go)
	cd $(GO_DIR) && gofmt -l .

.PHONY: api-test
api-test: ## go test (Go)
	cd $(GO_DIR) && go test ./...

.PHONY: api-test-cov
api-test-cov: ## go test with coverage
	cd $(GO_DIR) && go test -race -coverprofile=coverage.out ./...

# ─────────────────────────────────────────────────────────────────────
# Quality — Python workers (deferred; apps/workers arrives in Phase 2)
# ─────────────────────────────────────────────────────────────────────
.PHONY: workers-install
workers-install: ## Install Python worker dependencies (deferred; Phase 2)
	cd $(WORKERS_DIR) && uv sync

.PHONY: workers-test
workers-test: ## Run Python worker tests (deferred; Phase 2)
	cd $(WORKERS_DIR) && uv run pytest

.PHONY: workers-lint
workers-lint: ## Lint Python workers (deferred; Phase 2)
	cd $(WORKERS_DIR) && uv run ruff check .

# ─────────────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────────────
.PHONY: build
build: py-build api-build ## Build all artifacts (Python packages + Go binary)

.PHONY: py-build
py-build: ## Build Python packages
	cd packages/contracts && uv build

.PHONY: api-build
api-build: ## Build the Go API binary into apps/api/bin/
	cd $(GO_DIR) && go build -o bin/api ./cmd/api

# ─────────────────────────────────────────────────────────────────────
# Proto (deferred; Phase 1+)
# ─────────────────────────────────────────────────────────────────────
.PHONY: proto-gen
proto-gen: ## Generate Go + Python stubs from .proto files (deferred; Phase 1+)
	@echo "⏭  proto codegen deferred to Phase 1+"

.PHONY: proto-lint
proto-lint: ## Lint protos with buf (deferred; Phase 1+)
	@echo "⏭  proto lint deferred to Phase 1+"

# ─────────────────────────────────────────────────────────────────────
# Clean
# ─────────────────────────────────────────────────────────────────────
.PHONY: clean
clean: ## Remove build artifacts
	rm -rf packages/contracts/dist packages/contracts/build packages/contracts/*.egg-info
	rm -rf $(GO_DIR)/bin $(GO_DIR)/coverage.out
	find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
	find . -type d -name .pytest_cache -exec rm -rf {} + 2>/dev/null || true
	find . -type d -name .ruff_cache -exec rm -rf {} + 2>/dev/null || true
	find . -type d -name .pyright_cache -exec rm -rf {} + 2>/dev/null || true
