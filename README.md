# Orpheus

> Multi-tenant SaaS for asynchronous audio processing.

Orpheus is a platform for audio upload, async processing (metadata extraction,
probing, slicing, transcription), and structured results — a polyglot Go + Python
monorepo built around a multi-tenant, row-level-security core.

**Status:** Core API + async worker pipeline are implemented and exercised
end-to-end in CI (see [Implementation status](docs/IMPLEMENTATION_STATUS.md)).
The full target architecture lives in
[`docs/architecture/PRODUCTION_DESIGN.md`](docs/architecture/PRODUCTION_DESIGN.md).

## Architecture

Two cooperating planes with a typed contract between them:

- **API tier (`apps/api/`, Go)** — public HTTP API: auth (Keycloak JWT + API keys),
  RLS-scoped persistence, S3 presigned multipart uploads, jobs, artifacts,
  webhooks, API keys, usage/audit, and the transactional-outbox publisher +
  webhook-delivery background loops.
- **Worker tier (`apps/workers/`, Python)** — a NATS JetStream consumer that runs
  ffmpeg/ffprobe/faster-whisper processors (`extract-metadata`, `probe`, `slice`,
  `transcribe`), plus a gRPC control plane.
- **Contracts (`packages/proto/`, gRPC/Protobuf)** — `.proto` is the source of
  truth; `buf` generates Go + Python stubs. OpenAPI is served by the running API.

### Request → result flow

```
client ─POST /v1/jobs─▶ API (RLS tx: insert job + outbox row, one commit)
                         │
        outbox publisher (service-role tx) ─▶ NATS JetStream
                                                 │
                          Python worker ◀────────┘  runs ffmpeg/whisper,
                            writes artifacts + jobs.result, emits job.* events
                                                 │
                         webhook delivery loop ─▶ HMAC-signed POST to subscriber
```

Every tenant table is `FORCE ROW LEVEL SECURITY`. Request handlers run inside
`WithTenant(org_id, …)` so RLS scopes every query; background components
(outbox, webhook delivery) run in short service-role transactions.

## Implemented surface

- **Uploads:** `POST /v1/uploads`, `POST /v1/uploads/{id}/complete`,
  `GET /v1/uploads`, `GET /v1/uploads/{id}` (S3 presigned multipart, size cap).
- **Artifacts:** `GET /v1/artifacts`, `GET /v1/artifacts/{id}`, `.../signed-url`.
- **Jobs:** `POST /v1/jobs`, `POST /v1/jobs/bulk`, `GET /v1/jobs`,
  `GET /v1/jobs/{id}`, `DELETE /v1/jobs/{id}` (cancel).
- **Processors:** `GET /v1/processors`, `GET /v1/processors/{name}`.
- **Webhooks:** CRUD + `GET .../deliveries` + `POST .../replay` (HMAC signing,
  exponential backoff, SSRF-safe delivery).
- **API keys:** `POST/GET/DELETE /v1/api-keys` (Argon2id-hashed, scoped).
- **Workflows:** `POST /v1/workflows/transcribe-long`, `GET /v1/workflows/{id}`.
- **System:** `GET /v1/usage`, `GET /v1/audit-log`, `/health`, `/ready`,
  `/metrics`, `/api/docs`.

Cross-cutting: Keycloak JWT + API-key auth (with a verification cache),
per-route scope enforcement, per-tenant rate limiting (atomic Redis sliding
window), idempotency keys (reserve-before-execute), audit logging, and an
SSRF guard for outbound webhooks.

## Quickstart

```bash
# 1. Tooling
curl -LsSf https://astral.sh/uv/install.sh | sh   # uv (Python)
# Go 1.25+, Docker

# 2. Bring up the local stack (Postgres, Redis, MinIO, NATS)
docker compose up -d postgres redis minio nats

# 3. Dependencies
uv sync --all-packages
cd apps/api && go mod download && cd ../..

# 4. Run the API
make dev        # → http://localhost:8080  (/health, /ready, /api/docs)

# 5. Run a worker (separate shell)
uv run --package orpheus-workers python -m orpheus_workers.worker
```

## Testing

Tests are layered; the integration/e2e tiers need the local stack up.

```bash
cd apps/api

# unit only
go test -short ./...

# integration (RLS, webhooks, idempotency, outbox, audit, rate limit, S3)
export ORPHEUS_TEST_DATABASE_URL="postgres://orpheus_app:orpheus_app@localhost:5432/orpheus_test?sslmode=disable"
export ORPHEUS_TEST_REDIS_URL="redis://localhost:6379/15"
export ORPHEUS_TEST_S3=1 ORPHEUS_S3_ENDPOINT=http://localhost:9000 \
       ORPHEUS_S3_ACCESS_KEY=orpheus ORPHEUS_S3_SECRET_KEY=orpheus-dev-secret ORPHEUS_S3_BUCKET=orpheus-uploads
go test -race ./...

# end-to-end (full pipeline: API → NATS → real Python worker → ffmpeg/whisper)
ORPHEUS_E2E=1 ORPHEUS_TEST_NATS_URL=nats://localhost:4222 go test ./internal/e2e/...
```

Python workers: `cd apps/workers && uv run pytest`.
Load testing: see [`docs/LOAD_TESTING.md`](docs/LOAD_TESTING.md).

The RLS/tenant-isolation tests are only meaningful against a **non-superuser**
Postgres role (a superuser bypasses RLS). `scripts/ci-db-setup.sh` provisions
the dedicated `orpheus_app` role CI uses.

## CI/CD

`.github/workflows/ci.yml` runs on every PR: Python lint/type-check/test, Go
`-race` **integration** tests against real services, a full **e2e** job (real
worker + ffmpeg/whisper, model cached), OpenAPI validation, `buf` contract
checks, and a security scan. A **load-test** job runs on demand / weekly.
`.github/workflows/release.yml` builds and pushes the API and worker images to
GHCR on `v*` tags.

## Repo layout

```
orpheus/
├── apps/
│   ├── api/          # Go public API (handlers, auth, db+RLS, outbox, webhooks, ratelimit, e2e)
│   └── workers/      # Python NATS worker + processors (ffmpeg/ffprobe/whisper)
├── packages/
│   ├── contracts/    # generated gRPC Python stubs
│   └── proto/        # gRPC/Protobuf definitions (buf)
├── infra/            # (placeholder — Helm/Terraform/ArgoCD are future work)
├── docs/
│   ├── adr/          # Architecture Decision Records
│   ├── architecture/PRODUCTION_DESIGN.md   # target architecture
│   ├── IMPLEMENTATION_STATUS.md            # what's built vs planned
│   └── LOAD_TESTING.md
├── scripts/          # ci-db-setup.sh, dev utilities
├── docker-compose.yml
└── Makefile
```

## Documentation

- **[Implementation status](docs/IMPLEMENTATION_STATUS.md)** — feature-by-feature: built vs planned.
- **[Production design](docs/architecture/PRODUCTION_DESIGN.md)** — the authoritative target architecture and roadmap.
- **[ADRs](docs/adr/)** — one per significant design choice.
- **[Load testing](docs/LOAD_TESTING.md)** — how to run it + baseline numbers.
- **API docs** — served at `/api/docs` (Swagger) and `/api/redoc` when running.

## License

Apache-2.0.
