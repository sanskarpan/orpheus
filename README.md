# Orpheus

> Multi-tenant SaaS for asynchronous audio processing.

Orpheus is a production-grade platform for audio upload, async processing (transcription, classification, source separation, generation), and structured results — designed to operate at the scale and reliability of Replicate, Modal, or AssemblyAI.

**Status:** Phase 0 — Foundation. Polyglot monorepo, hello-world Go API, CI scaffolding, ADRs.

## Polyglot architecture

Orpheus runs as two cooperating planes with a typed contract between them:

- **API tier (`apps/api/`, Go)** — public HTTP API, auth, request validation, and dispatch. Go for throughput, low memory footprint, and static binaries.
- **Worker tier (`apps/workers/`, Python — Phase 2)** — Arq / Temporal / Ray Serve workers that run ML inference and post-processing. Python for the ML ecosystem (PyTorch, librosa, transformers).
- **Contracts (`packages/proto/`, gRPC/Protobuf — Phase 1+)** — `.proto` files are the single source of truth. `buf` generates Go stubs into `apps/api/internal/gen/` and Python stubs into `packages/contracts/src/orpheus_contracts/gen/`.

In Phase 0 the Go API is a hello-world service; the worker plane and proto package are empty placeholders.

## Quickstart

```bash
# 1. Install uv (if needed)
curl -LsSf https://astral.sh/uv/install.sh | sh

# 2. Python (workers + contracts package)
cd orpheus
uv sync --all-packages

# 3. Go (API tier)
cd apps/api && go mod download && go mod tidy && cd ../..

# 4. Lint and test everything
make check

# 5. Run the API
make dev
# → http://localhost:8080
# → http://localhost:8080/api/docs
# → http://localhost:8080/health
# → http://localhost:8080/ready
```

## Repo layout

```
orpheus/
├── apps/
│   ├── api/                    # Go public API (Phase 0: hello-world)
│   └── workers/                # Python workers (Phase 2)
├── packages/
│   ├── contracts/              # OpenAPI / AsyncAPI / shared Pydantic models (Python)
│   └── proto/                  # gRPC / Protobuf definitions (Phase 1+)
├── infra/                      # Terraform, Helm, ArgoCD appsets (Phase 0: README only)
├── docs/
│   ├── adr/                    # Architecture Decision Records
│   └── architecture/
│       └── PRODUCTION_DESIGN.md # The full target architecture
├── scripts/                    # bootstrap, dev utilities
├── .github/workflows/          # GitHub Actions CI (lint, test, go, proto, security, contract)
├── pyproject.toml              # uv workspace root (Python)
├── package.json                # pnpm workspace root
├── docker-compose.yml          # Local stack: Postgres, Redis, MinIO
└── Makefile                    # Convenience commands (Python + Go)
```

## Phase 0 success criteria

A new engineer can:

1. `git clone` the repo
2. `make install` (uv sync + go mod download)
3. `make dev` → Go API starts
4. `curl localhost:8080/health` → 200 OK
5. Open `http://localhost:8080/api/docs` → see the OpenAPI explorer

…in **30 minutes or less**.

## Status

| Phase | Title | Status |
|---|---|---|
| 0 | Foundation (you are here) | ✅ scaffold |
| 1 | Core API & Auth | ⏳ not started |
| 2 | Jobs & Arq | ⏳ not started |
| 3 | Observability & SRE | ⏳ not started |
| 4 | First Workflow (Transcribe-Long) | ⏳ not started |
| 5 | Production Hardening | ⏳ not started |
| 6 | Scale & Polish | ⏳ not started |
| 7 | Marketplace & BYO Model | ⏳ not started |
| 8 | Streaming & Realtime | ⏳ not started |

See `docs/architecture/PRODUCTION_DESIGN.md` §17 for the full roadmap and dependencies.

## Documentation

- **[`docs/architecture/PRODUCTION_DESIGN.md`](docs/architecture/PRODUCTION_DESIGN.md)** — the authoritative target architecture (this is what we are building toward).
- **[`docs/adr/`](docs/adr/)** — Architecture Decision Records, one per significant design choice.
- **API docs** — once running, served at `/api/docs` (Swagger UI) and `/api/redoc` (ReDoc).

## License

Apache-2.0.
