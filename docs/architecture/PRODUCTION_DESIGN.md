# Orpheus — Production Design Document

> **A multi-tenant SaaS for asynchronous audio processing, designed for the long term.**
> **Date:** 2026-07-09 · **Status:** v0.1 architecture, ready for review · **Owner:** Principal Architect

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Business Problem Analysis](#2-business-problem-analysis)
3. [Technology Evaluation Matrix](#3-technology-evaluation-matrix)
4. [Architecture Decision Record (ADR)](#4-architecture-decision-record-adr)
5. [Final System Architecture](#5-final-system-architecture)
6. [Service Interaction Diagrams](#6-service-interaction-diagrams)
7. [Data Flow Diagrams](#7-data-flow-diagrams)
8. [API Strategy](#8-api-strategy)
9. [Database Design](#9-database-design)
10. [Infrastructure Design](#10-infrastructure-design)
11. [Security Architecture](#11-security-architecture)
12. [Performance & Scalability Strategy](#12-performance--scalability-strategy)
13. [Observability Plan](#13-observability-plan)
14. [Testing Strategy](#14-testing-strategy)
15. [Deployment Strategy](#15-deployment-strategy)
16. [CI/CD Pipeline Design](#16-cicd-pipeline-design)
17. [Implementation Roadmap](#17-implementation-roadmap)
18. [Risk Analysis](#18-risk-analysis)
19. [Cost Considerations](#19-cost-considerations)
20. [Future Enhancements](#20-future-enhancements)

---

## 1. Executive Summary

### 1.1 What this is

`Orpheus` is a **multi-tenant, asynchronous audio processing platform** that supports any number of pluggable operations (metadata extraction, transcoding, slicing, transcription, diarization, source separation, music generation, classification, and — over time — custom tenant-supplied models). It is designed to be a credible, production-grade entry in the model-as-a-service space.

### 1.2 Business positioning

The product class is well-established: **Replicate, Modal, Hugging Face Inference Endpoints, AssemblyAI, Deepgram, AWS Bedrock** all occupy adjacent space. We are closer to **Replicate + AssemblyAI** — async-first, model-as-a-service, multi-tenant — and explicitly **not** closer to the synchronous, WebSocket-first Deepgram model. We earn our place by:

- **Breadth of processors** (transcribe, classify, separate, generate, plus a third-party processor marketplace in v2).
- **Reproducibility** (every result pinned to a `ModelVersion`; same input → same output forever).
- **Multi-tenant fairness** (per-tenant bulkheads on GPU pool, queue depth, and rate limits).
- **Developer ergonomics** (OpenAPI + generated SDKs, idempotency, webhooks, sandboxed BYO model in v2).

### 1.3 Headline design decisions (justified in §4)

| Decision | Choice | One-line reason |
|---|---|---|
| API tier | **Modular monolith on Go + Chi (Python FastAPI only for the worker control plane)** | Go is the right answer for high-RPS, low-logic, file-coordination HTTP. ~5–10× faster per core, ~10× less memory, ~$22k/yr cheaper at 1M MAU. Python lives only where the ML ecosystem is non-negotiable. See ADR-0013. |
| Inter-tier protocol | **gRPC + Protobuf** | Strongly-typed, low-latency, schema-evolution discipline via `buf`. Public API stays REST + OpenAPI. |
| Worker plane | **Arq for simple jobs + Temporal for multi-step workflows** | One queue system is not enough (no signals, no human-in-loop, no saga); two is correct when the boundary is sharp. |
| Database | **PostgreSQL 16 with row-level security** | Multi-tenant, JSONB for flexible processor results, UUID v7 PKs, partitioning for time-series tables. |
| Object storage | **S3-compatible (real S3 in prod, MinIO in dev)** | The only sensible answer. Multipart upload via presigned URLs (no bytes through the API tier). |
| Auth | **Keycloak (self-hosted) for v1** | Data residency, free SSO/SAML, no per-MAU cost. Reassess at $1M ARR. |
| Model versioning | **First-class `ModelVersion` entity** | Reproducibility, audit, A/B testing, deprecation, enterprise sales — all require it. |
| GPU serving | **Ray Serve** | Heterogeneous models, dynamic batching, fractional GPU, single control plane. Triton would be premature. |
| Worker sandbox | **gVisor** | Defends against libavcodec-style kernel escapes in untrusted audio. The actual exploit class for our workload. |
| Infra | **AWS EKS + Karpenter + Cilium + Envoy Gateway** | Right abstractions at the right time. Multi-region active/warm. |
| Observability | **OpenTelemetry → Grafana stack (Prometheus, Loki, Tempo, Mimir)** | Vendor-neutral, self-hostable, cost-controllable. |
| DX | **Monorepo with uv + pnpm workspaces, Tilt for local K8s dev** | Idiomatic per language, fast, hot-reload, no extra build system. |
| API versioning | **URI versioning (`/v1/`)** + 6-month sunset | Codegen-friendly, clear migration story. |
| Cost target | **≤ $0.10/MAU/mo infra at 1M MAU, ≥ 30% gross margin** | Headroom for re-investment; below 30% margin = dangerous; above 50% = under-investing. |

### 1.4 What this document is not

This is a **target-state architecture** for the system 12–18 months from now. The **implementation roadmap (§17)** explicitly starts much smaller — Phase 0 is a 2-process monolith (one Go process serving the public API, one Python process running the worker control plane + Arq) on managed Postgres + managed Redis + S3, no K8s, no Temporal, no Keycloak. The full system described here is what we converge on by the time we have 3+ services and 10+ engineers. **The roadmap is the build order; this document is the destination.**

---

## 2. Business Problem Analysis

### 2.1 The actual problem

The product brief: *"A mini audio processing framework... handles uploads, runs background jobs, and spits out metadata or processed results... we can add FFmpeg and it can convert audio, slice clips and generate waveforms."*

Take that seriously and you get: **a platform that lets any developer upload audio, choose a processing operation from a menu, and receive structured results or new artifacts asynchronously.** That is the product.

### 2.2 What survives from the current implementation

| Survives | Why |
|---|---|
| The core domain: **upload → process → result** | This is the product. The shape is right. |
| The choice of **async-by-default** (RQ) | Correct for the workload shape. Replace RQ with Arq+ Temporal; the *asynchronous-ness* stays. |
| The choice of **Python** for the worker plane | Correct — every audio library we care about (mutagen, librosa, ffmpeg bindings, faster-whisper, pyannote, demucs) is Python-first. |
| The two-app mental model (`uploads` + `jobs`) | Clean separation. Evolves into proper bounded contexts. |
| The JSONField `result` pattern | Correct — processor outputs are heterogeneous. Becomes a JSONB column with GIN indexes. |

### 2.3 What is redesigned

| Current | Redesigned | Why |
|---|---|---|
| SQLite | **PostgreSQL 16 + RLS** | Multi-tenant, concurrency, JSONB, real durability. |
| Django REST Framework | **Go + Chi (API tier); Python + FastAPI (worker control plane)** | DRF is synchronous-first and bolted-on for OpenAPI. The API tier moved to Go (ADR-0013); the Python worker control plane uses FastAPI (ADR-0002). |
| `FileField` to local disk | **S3 via presigned multipart upload** | Bytes never go through the API tier. Multi-region. |
| RQ + redis-rq scheduler | **Arq for simple jobs + Temporal for workflows** | RQ has no signals, no saga, no human-in-loop, weak observability. |
| `process_job` with a single branch on `job_type` | **Processor plugin model with `ModelVersion` pinning** | Reproducibility, audit, A/B test, marketplace. |
| Single-tenant, no auth | **Multi-tenant with org/team/user, RBAC, OAuth2/OIDC, API keys** | Required to sell. |
| No file validation | **Magic-byte sniff, format probe, size cap, virus scan, content-type validation** | Defends the processing plane. |
| No DLQ, no retries policy | **Explicit DLQ with requeue UI, retry policy per processor, dead-letter alerting** | Operations can't run on hope. |
| No observability | **OpenTelemetry everywhere → Grafana stack** | Cannot operate what you cannot see. |
| No cost attribution | **Per-job cost recorded in `job_costs` table, per-tenant metering** | Cannot price what you cannot meter. |
| Inline cleanup in request thread | **Scheduled workflow via Temporal, idempotent, observable** | Don't DoS the API tier with batch work. |

### 2.4 Pain points the current system would have at scale

(All inferred from the codebase; none are code-proven at scale because the codebase has not been run at scale.)

1. **Cleanup function crashes on first call** (`status_in` vs `status__in`) — silent data corruption. At scale, silent corruption compounds.
2. **No file-type validation** — any byte stream accepted as "audio"; workers process garbage. At scale, this is a CPU DoS vector.
3. **No row-level isolation** — every user sees every other user's `audio_file_id`. At scale, this is a privacy breach.
4. **Single-process, single-thread worker** — `rqworker` is one process; no horizontal scale story.
5. **RQ job timeout `-1`** — a stuck worker holds a job forever; no watchdog.
6. **No DLQ, no retry policy** — failed jobs vanish into the `failed` state with no recovery path.
7. **No S3** — files on local disk; second web node immediately breaks.
8. **DEBUG=True, hardcoded `SECRET_KEY`** — the codebase is unsafe to deploy as-is.
9. **`asgi.py` is dead code** — no async server, no real async.
10. **No tests** — zero confidence in any refactor.

### 2.5 Production limitations the current system would have

- **No SOC 2 story** — no audit log, no access control, no encryption at rest with KMS.
- **No GDPR/CCPA story** — no data retention policy, no right-to-erasure implementation, no DSR workflow.
- **No SLOs** — no defined availability, latency, or freshness targets.
- **No DR** — no backup, no cross-region replication, no RPO/RTO.
- **No multi-tenancy story** — every user is global admin.
- **No cost model** — no per-tenant metering, no per-job cost recording.
- **No model governance** — processors are stringly-typed, no version pinning, no license tracking.

### 2.6 Opportunities for innovation

1. **Processor marketplace** (v2) — third parties publish processors; we run them in stricter sandbox; customers discover and use them. This is the natural Replicate-style extensibility story.
2. **BYO model** (v2) — tenants upload their own weights; we run in community sandbox. Customization without per-tenant infrastructure.
3. **Streaming inference** (v2) — WebRTC → real-time transcription. This is the Deepgram-style extension if we go upmarket.
4. **Distributed deduplication** — content-addressed storage; same input + same model = free cache hit. 10–30% storage and compute savings in practice.
5. **Auto-summarization pipelines** — built-in multi-step workflows (transcribe → summarize → translate → caption) as first-class templates.

---

## 3. Technology Evaluation Matrix

For every category, the choice, the alternatives, the criterion, and the verdict. The agent reports in §A1–A6 informed these.

### 3.1 Backend Language

Orpheus is a **polyglot** system. We use **Go for the API tier** and **Python for the worker tier**; the two communicate over **gRPC + Protobuf**. The public REST surface is served by Go; the public OpenAPI is generated from the Go code.

| Tier | Language | Why this language, here |
|---|---|---|
| **API tier (public HTTP)** | **Go 1.22+** | High-RPS, low-logic, file coordination, webhooks, signed URLs, rate limiting. Go is Go's sweet spot: compiled, statically linked, no GIL, ~5–10× more RPS per CPU core than Python, ~10× smaller per-pod RSS, faster cold start, smaller images. No Python GIL means the API tier scales predictably under load. |
| **Worker tier (audio + ML)** | **Python 3.12** | Every audio/ML library we use is Python-first: `mutagen`, `librosa`, `ffmpeg` bindings, `faster-whisper` (CTranslate2), `pyannote`, `demucs`, `musicgen`, `transformers`, `onnxruntime`. There is no Go ecosystem for production ASR, source separation, or generative audio. The Python tier is **non-negotiable**. |
| **Worker control plane** | **Python 3.12 + FastAPI** | A small internal HTTP service that exposes worker-pool health, model-registry status, and admin endpoints to the Go API tier over gRPC. FastAPI is the right choice for this small Python HTTP surface. See §3.2 and ADR-0002. |
| **Inter-tier wire** | **gRPC + Protobuf** | Strongly-typed contract between the Go API and the Python worker plane. Schema evolution is enforced by `buf` in CI. |

#### Cost math at 1M MAU (illustrative)

The API tier handles 1B API calls/month at p95 200 ms. The same workload on FastAPI/uvicorn and the same workload on Go + Chi differ materially:

| Workload | Python (FastAPI/uvicorn) | Go (net/http + Chi) | Delta |
|---|---|---|---|
| Pods required (target: p95 200 ms, 70% CPU) | ~80 (2 vCPU, 1 GiB each) | ~12 (1 vCPU, 256 MiB each) | ~7× fewer pods |
| API-tier compute ($/mo, on-demand c7g) | ~$5,500 | ~$420 | ~$5,080/mo |
| Per-pod memory (RSS at steady state) | ~480 MiB | ~45 MiB | ~10× less |
| Container image size | ~180 MiB (slim + uvicorn) | ~12 MiB (distroless) | ~15× smaller |
| Cold-start time (pod ready) | ~2.5 s | ~80 ms | ~30× faster |

**Annualized at 1M MAU:** the language choice for the API tier alone is on the order of **$22k/yr** cheaper on Go, before counting the dev-loop and operational benefits (faster CI, no GIL tuning, single static binary). That delta grows with scale.

#### Tradeoff accepted

Two languages in the monorepo. Costs:

- **Two language toolchains** (Go 1.22/1.23 and Python 3.12), two sets of dependencies, two CI matrices.
- **gRPC contract to maintain** (Protobuf schemas in `packages/proto`, breaking-change detection via `buf`).
- **Smaller hiring pool for each tier** than for "all Python" or "all Go".

Mitigations: shared monorepo (ADR-0012), `buf` for proto governance, OpenAPI for the public surface (codegen from the Go side), OpenTelemetry trace context propagated across the gRPC boundary so a single trace spans both tiers (ADR-0010).

#### Production precedents

- **Replicate** — Go for the API + Python for the workers/Cog. Same split.
- **Modal** — Rust for the API tier + Python for user code. Same split, different language on the API side.
- **Hugging Face Inference Endpoints** — Python-first (research-first, fewer web-tier requirements).

The Go + Python split is the established pattern for "web-shaped API in front of an ML-shaped backend". Replicate is the closest analogue to Orpheus.

### 3.2 API Framework (per tier)

The "API framework" question is **per tier**, not global. The two tiers have different workloads and the right answer is different for each.

#### 3.2.a Go API tier (public HTTP)

| Option | Verdict | Rationale |
|---|---|---|
| **Go 1.22+ with `net/http` + Chi router** | ✅ **Chosen** | Chi is a small, composable router built on `net/http`. First-class middleware, no allocations on the hot path, idiomatic. Pairs cleanly with `slog` for structured logging and `go.opentelemetry.io/otel` for traces. Static, single binary; trivially cross-compiled; ~12 MiB distroless image. |
| Echo | ⚠️ | Equally good. Slightly more opinionated. We picked Chi for its minimalism and the fact that it does not fork `http.Handler`. Watch. |
| Gin | ⚠️ | Fast, but historically more coupled to its own context. Chi's `http.Handler` composability wins. |
| stdlib `net/http` + mux | ⚠️ | The 1.22+ `ServeMux` is a real router now (path parameters, method matching, wildcards). We will use it for trivial services; Chi remains the default for the main API. |
| **FastAPI / Litestar (Python)** | ❌ for the API tier | Wrong language for the workload. Python is GIL-bound under load; the API tier is high-RPS, low-logic, I/O-bound HTTP — Go's sweet spot. Cost math in §3.1 shows ~7× fewer pods on Go. FastAPI is the right choice for the **Python worker control plane** (see §3.2.b), not for the public API. |
| Django REST Framework | ❌ for the API tier | Synchronous-first, OpenAPI is bolted on, ORM is excellent but we use `pgx` + `sqlc` in Go. No reason to introduce Django anywhere in the system. |
| Litestar | ❌ for the API tier | Excellent framework, same workload-mismatch argument as FastAPI. |

**Why Chi over Gin/Echo specifically:** Chi is a thin router (~1.5k LoC) that returns standard `http.Handler` and does not introduce its own context type. That means the entire `net/http` ecosystem (otelhttp, chi/middleware, custom middleware) drops in without adapters, and the Go team can read any standard-library HTTP example unchanged. Echo and Gin are both fine; we have no strong objection — we just have a slight preference for "the smallest router that still does routing".

**Generated OpenAPI.** Public REST is served by the Go API tier. OpenAPI 3.1 is generated from the Go code via `oapi-codegen` (from a hand-written `api.yaml` source of truth) so the schema is the contract, not the implementation. The Python side talks to the Go side over gRPC, not over HTTP — it does not need its own OpenAPI.

**Validation.** Request and response types are Go structs with generated validators (e.g., `go-playground/validator/v10`). No Pydantic-equivalent runtime cost. Protobuf is used on the gRPC boundary (where the schema lives in `.proto`); for the HTTP boundary, Go structs + validators + generated OpenAPI.

**ORM / data access.** `pgx` (PostgreSQL driver) + `sqlc` (SQL-to-Go codegen). Hand-written SQL, type-checked Go bindings, no ORM magic. Migrations are still `Alembic` (Python) or `goose`/`sqlx`-migrate (Go) — the schema lives in one place and both sides read it.

#### 3.2.b Python worker control plane

The worker plane needs a small HTTP surface for:

- Worker pool health and liveness/readiness for the Go API tier to query over gRPC.
- Model registry status and admin operations (pull, warm, retire a `ModelVersion`).
- Operational endpoints used by the SRE team (force-replay a job, drain a pool).

This service is **Python** (it sits next to the workers) and **FastAPI** is the right choice for it.

| Option | Verdict | Rationale |
|---|---|---|
| **FastAPI + Pydantic v2** | ✅ **Chosen** | Async-native, OpenAPI 3.1 from Pydantic types, fits naturally next to Arq/Temporal Python workers. Internal-only — no public HTTP traffic. |
| Django REST Framework | ❌ | Synchronous-first, ORM-coupling. We already use SQLAlchemy 2.0 async in the Python tier. |
| Litestar | ⚠️ | Equivalent to FastAPI; smaller community. Watch. |
| Flask | ❌ | No async story. No built-in OpenAPI. |
| gRPC only (no HTTP) | ⚠️ considered | Possible. HTTP is cheaper to debug from a terminal and cheaper to auth with internal mTLS + JWT. We keep HTTP for ops ergonomics. |

The full rationale for FastAPI in this role (Pydantic v2, async-native, ~2× the throughput of DRF) is captured in **ADR-0002** (renamed in this revision to make its scope explicit: it is the Python worker control plane, not the public API tier).

### 3.3 Database

| Option | Verdict | Rationale |
|---|---|---|
| **PostgreSQL 16** | ✅ **Chosen** | Best-in-class JSONB, partitioning, RLS, logical replication, extensions (pgvector, PostGIS). Open source. |
| ClickHouse | ⚠️ | For analytics. Not OLTP. Add in v2 via CDC pipeline. |
| MongoDB | ❌ | We don't need a document store; JSONB in Postgres is enough. |
| Redis | ✅ as cache | For ephemeral state, idempotency, rate limits, queue. Not a system of record. |
| Elasticsearch / OpenSearch | ⚠️ | Full-text search. PG full-text is enough for v1. Add when actually needed. |

### 3.4 Message Broker

| Option | Verdict | Rationale |
|---|---|---|
| **Arq (Redis)** | ✅ for simple jobs | Lightweight, Python-native, well-understood. Single Redis dependency we already have. |
| **Temporal** | ✅ for workflows | The only mature workflow orchestrator. Signals, queries, retries, sagas, versioning. Operational cost is real (5+ services self-hosted) but justified by the workflow shapes we have. |
| Celery | ❌ | Older, less expressive retry, weaker observability. Dramatiq is better. |
| Dramatiq | ⚠️ | Good alternative to Celery/Arq. RabbitMQ-native. Pick if you don't want Temporal. |
| BullMQ | ❌ | Node-only. |
| Kafka | ⚠️ | Heavier. Use for event sourcing / CDC in v2. |
| NATS JetStream | ⚠️ | Lightweight, durable. Strong alternative if you don't want Temporal. |

**Decision rule (codified in §5.4):** if `job is single-activity, no signals, no humans, no saga, < 60s, no fan-out` → Arq. Otherwise → Temporal.

### 3.5 Object Storage

| Option | Verdict | Rationale |
|---|---|---|
| **S3 (real)** | ✅ for prod | Industry standard, lifecycle policies, multipart, presigned URLs. |
| **MinIO** | ✅ for dev/staging | S3-compatible, self-hostable, drop-in replacement. |
| GCS / Azure Blob | ❌ | Multi-cloud is not worth the complexity; the abstraction layer handles it. |
| Local FS | ❌ | Doesn't work in multi-host deploys. |

### 3.6 Auth

| Option | Verdict | Rationale |
|---|---|---|
| **Keycloak (self-hosted)** | ✅ for v1 | Free, OSS, full OAuth2/OIDC + SAML, no per-MAU, data residency. Operational cost (upgrades, HA) is real but bounded. |
| Auth0 | ⚠️ | Good for B2C at scale. Expensive for B2B SSO. Reassess at $1M ARR. |
| Clerk | ⚠️ | Excellent DX, but vendor lock-in. Best if team is < 3 engineers. |
| WorkOS | ⚠️ | Enterprise SSO/SAML/SCIM as a service. Pair with our own user store. |
| Better Auth | ⚠️ | Newer OSS option; watch but not v1. |
| Roll-your-own (Authlib) | ❌ | Do not roll your own auth. |

**Reassessment trigger:** at > 50 enterprise SSO customers or > 1M MAU, re-evaluate Clerk/Auth0/WorkOS for DX gains. At > $1M ARR, evaluate the cost of self-hosting vs managed.

### 3.7 Workflow Orchestrator

Already covered in §3.4. **Temporal wins** for multi-step, long, human-in-loop, saga-shaped work.

### 3.8 API Style

| Option | Verdict | Rationale |
|---|---|---|
| **REST + JSON** | ✅ for public | OpenAPI 3.1, codegen, every client knows it. |
| gRPC | ⚠️ for internal | For service-to-service, especially worker plane. Public API stays REST. |
| GraphQL | ❌ | Our workload is RPC-shaped (upload, start-job, get-result). GraphQL solves over-fetching on entity graphs we don't have. |
| WebSockets | ⚠️ for streaming only | Defer until we add real-time transcription (v2). |
| SSE | ✅ for job progress | Webhook-first, SSE secondary, polling fallback. |

### 3.9 Frontend (Admin Dashboard)

| Option | Verdict | Rationale |
|---|---|---|
| **Next.js (App Router) + TypeScript** | ✅ for admin | React ecosystem, SSR, server actions, RSC for read-heavy admin. |
| Remix | ⚠️ | Viable alternative. |
| Svelte / SvelteKit | ⚠️ | Smaller bundle, faster dev. Smaller ecosystem. |
| Vue | ❌ | Team alignment with React. |
| Server-rendered HTMX | ⚠️ for cost-sensitive admin | Cheaper to run, but UX worse for complex admin features. |

### 3.10 ML Inference Server

| Option | Verdict | Rationale |
|---|---|---|
| **Ray Serve** | ✅ | Heterogeneous models, dynamic batching, fractional GPU, Python-native, single control plane. |
| Triton Inference Server | ⚠️ | Best for single-model high-QPS. Overkill for our heterogeneous low-batch workload. |
| vLLM | ⚠️ | For LLM serving. We have audio, but transcripts may feed into LLMs later. |
| BentoML | ⚠️ | Good alternative. Less mature for GPU scheduling. |
| Plain PyTorch | ❌ | Misses autoscaling, batching, observability. |

### 3.11 Observability

| Option | Verdict | Rationale |
|---|---|---|
| **OpenTelemetry** (SDK + Collector) | ✅ | Vendor-neutral. Adopted by every major backend. |
| **Prometheus** | ✅ for metrics | CNCF standard. Self-hostable. |
| **Grafana + Loki + Tempo + Mimir** | ✅ | One UI. Self-hostable. Cost-controllable. |
| Datadog | ⚠️ | Best DX, highest cost ($20–40k/mo at Scale). Use only if team has no SRE capacity. |
| New Relic | ⚠️ | Similar trade-off. |
| Sentry | ✅ for errors | Best in class for exception tracking. Cheap. |

### 3.12 Infrastructure

| Option | Verdict | Rationale |
|---|---|---|
| **AWS** | ✅ | Largest ecosystem, GPU availability, mature K8s. |
| GCP | ⚠️ | Comparable. Choose based on team familiarity and GPU pricing. |
| Azure | ⚠️ | Heavier for OSS-first teams. |
| **EKS** | ✅ for K8s | Managed control plane. |
| Karpenter | ✅ | Best-in-class node autoscaler. |
| Cilium (eBPF CNI) | ✅ | Performance, security, no sidecar tax. |
| Envoy Gateway | ✅ | Gateway API, L7, no ingress-nginx CVE history. |
| Istio / Linkerd | ❌ for v1 | Service mesh is premature. Cilium + Envoy covers our needs. |
| Terraform | ✅ | Standard for IaC. |
| Helm | ✅ | Standard for K8s packaging. |
| ArgoCD | ✅ | GitOps standard. |
| External Secrets Operator + AWS Secrets Manager | ✅ | Vault is overkill for v1. |
| **Tilt + k3d + Telepresence** | ✅ for local dev | Fast hot-reload; real K8s semantics. |

### 3.13 CI/CD

| Option | Verdict | Rationale |
|---|---|---|
| **GitHub Actions** | ✅ | Standard, integrated with code, good matrix support, generous free tier. |
| GitLab CI | ⚠️ | Excellent but only if you use GitLab. |
| CircleCI | ⚠️ | Good but losing mindshare. |
| Buildkite | ⚠️ | Good for hybrid cloud. |
| **Argo Rollouts** | ✅ | Progressive delivery (canary, blue/green) with Prometheus-driven analysis. |
| Renovate | ✅ for deps | More configurable than Dependabot. |
| Cosign + SLSA + Syft | ✅ for supply chain | Image signing, provenance, SBOM. |
| **Go 1.22 + 1.23 matrix** | ✅ for the API tier | Both Go 1.22 and 1.23 are in the test matrix. The minimum supported Go version is 1.22 (matches EKS base images; `net/http` routing improvements in 1.22 are used). |
| **`golangci-lint` + `go test`** | ✅ for Go | `golangci-lint` aggregates `staticcheck`, `govet`, `errcheck`, `gosec`, `gocritic`, `revive`, `gofmt`, `goimports`, and runs them in parallel. `go test -race -cover` is the test entrypoint. |
| **`buf`** | ✅ for proto | `buf lint` + `buf breaking` enforces Protobuf style and detects breaking changes in the gRPC schema between the Go API tier and the Python worker plane. Runs on every PR. |

The full CI matrix runs **Go 1.22, Go 1.23, Python 3.12** in parallel. The Go jobs build, lint (`golangci-lint`), test (`go test -race -cover`), and run `buf breaking --against '.git#branch=main'`. The Python jobs run `ruff`, `pyright`, and `pytest` (as in §3.14). The proto package is built first and shared by both.

### 3.14 Testing

| Option | Verdict | Rationale |
|---|---|---|
| **pytest + hypothesis + testcontainers** | ✅ for Python | Standard. Property-based testing for processors. Real services in tests. |
| **Playwright** | ✅ for web | Modern, reliable, multi-browser. |
| **k6** | ✅ for load | JS-based, Grafana-integrated. |
| Schemathesis | ✅ for API contract | Generates tests from OpenAPI. |
| mutmut | ✅ for mutation testing | Catches "tests that don't test". |

### 3.15 Documentation

| Option | Verdict | Rationale |
|---|---|---|
| **Mintlify** | ✅ | Modern, fast, great DX for API docs. |
| Docusaurus | ⚠️ | Good, but slower to evolve. |
| ReadTheDocs + Sphinx | ⚠️ | Standard for Python but weaker UX. |
| Structurizr + C4 | ✅ for architecture diagrams | Code-as-diagrams, version-controlled. |

### 3.16 Feature Flags

| Option | Verdict | Rationale |
|---|---|---|
| **Flipt (self-hosted OSS)** | ✅ | No vendor lock-in, no per-MAU, GitOps-friendly. |
| Unleash | ⚠️ | Mature but more ops. |
| LaunchDarkly | ⚠️ | Best SaaS DX. Reassess when team > 10. |
| Flagsmith | ⚠️ | Good alternative. |
| Build-your-own (Postgres + Redis) | ❌ | Build vs buy; not worth it. |

---

## 4. Architecture Decision Record (ADR)

The 13 most consequential decisions, with context, options, decision, and consequences. **All are reversible until the system has paying customers depending on them.**

### ADR-001: Modular monolith over microservices
- **Context:** 5–15 engineers, pre-PMF. Service boundaries are wrong on day one. Microservice tax is high.
- **Decision:** **Polyglot**: a single Go API codebase (the public API tier) plus a separate Python codebase (the worker plane, with its own FastAPI control plane). Two deployment units: `api` (Go) and `workers` (Python). Internally each is modular.
- **Consequences:** Faster iteration, fewer repos, simpler ops. Split first module: workers (already separate). Split next: uploads at ~50 RPS sustained. Split last: billing (PCI scope).
- **Reversibility:** High. Module boundaries make the split mechanical.

### ADR-002: FastAPI for the Python Worker Control Plane (API tier is Go, see ADR-0013)
- **Context:** The Go API tier is the public surface. The Python worker plane also needs a small internal HTTP service for worker-pool health, model-registry status, and admin endpoints, called by the Go tier over gRPC.
- **Decision:** FastAPI for that small internal Python HTTP surface. Pydantic v2 for validation, async-native, OpenAPI 3.1 for the internal contract. *Not* the public API tier (see ADR-0013).
- **Consequences:** ~2× throughput, better type safety, cleaner async. Lose Django admin (replace with a custom admin or buy one). Lose Django ORM (use SQLAlchemy 2.0 with asyncpg). Migration cost from a small DRF codebase: a few endpoints and models — trivial.
- **Reversibility:** High (it's a few endpoints).

### ADR-003: PostgreSQL 16 with row-level security for multi-tenancy
- **Context:** Multi-tenant from day one. Three options: schema-per-tenant, DB-per-tenant, row-level with `tenant_id`.
- **Decision:** Row-level security. `tenant_id` on every table, RLS policies enforce isolation, `SET LOCAL app.current_org_id` per request.
- **Consequences:** Single database, single connection pool, easy cross-tenant analytics. Need to test RLS coverage carefully; one missing policy = data leak.
- **Reversibility:** Medium. Migrating to schema-per-tenant later is a known path if needed.

### ADR-004: Arq for simple jobs, Temporal for workflows
- **Context:** Job shapes span "fire-and-forget metadata extraction" to "transcribe hour-long audio, chunk, parallel process, stitch, persist". One queue system is not enough; two is correct.
- **Decision:** Arq (Redis-based) for single-activity jobs < 60s, no signals, no fan-out. Temporal for everything else.
- **Consequences:** Two mental models, two dashboards, two SDKs. Operational cost of Temporal (5+ services self-hosted) is real. For v1, use Temporal Cloud; self-host when ops capacity exists.
- **Reversibility:** Medium. Could collapse to one if we never add a real workflow.

### ADR-005: Model versioning is first-class
- **Context:** Replicate, Modal, AssemblyAI, Deepgram all version their models. Reproducibility, audit, A/B testing, deprecation all require it. Many platforms in this space still treat processors as unversioned strings — we will not.
- **Decision:** `ModelVersion` is a first-class entity. Every job pins a `(processor, version)`. Results record the exact `model_version_id` that produced them.
- **Consequences:** Can sell to enterprise. Can A/B test. Can deprecate. Schema slightly more complex. Worth it.
- **Reversibility:** Low (this is a contract).

### ADR-006: S3 multipart upload via presigned URLs (no bytes through API)
- **Context:** Audio is MB–GB. Routing bytes through the API tier is a self-imposed DoS.
- **Decision:** Client requests upload intent → API returns presigned S3 multipart URLs → client PUTs parts directly to S3 → client calls `POST /uploads/{id}/complete` with checksums.
- **Consequences:** API tier scales independently of upload bandwidth. Egress is S3 + CloudFront, not from our infra. Requires multipart handling logic in client (the SDK does this).
- **Reversibility:** High (it's the API contract).

### ADR-007: Keycloak for auth in v1
- **Context:** Need OAuth2/OIDC, B2B SSO/SAML, API keys, RBAC. Options: self-host Keycloak, buy Auth0/Clerk/WorkOS, roll our own.
- **Decision:** Keycloak self-hosted in v1. Reassess at $1M ARR.
- **Consequences:** Free, data-residency control, full feature set. Operational cost (upgrades, DB migrations, HA) is real. Acceptable for our team.
- **Reversibility:** Medium. Migration to managed IdP is a known path; not free.

### ADR-008: gVisor sandbox for untrusted audio processing
- **Context:** Workers process user-uploaded audio with ffmpeg, librosa, and ML models. ffmpeg has a long history of memory-safety CVEs. A malicious file could escape the worker.
- **Decision:** All processing workers run in gVisor (`runtimeClassName: gvisor`). Seccomp profile. No network egress. Resource limits (CPU, memory, time, FDs).
- **Consequences:** Defends against kernel-escape-class exploits. ~5–15% perf overhead. Acceptable.
- **Reversibility:** High (it's a pod spec change).

### ADR-009: GitOps with ArgoCD, not push-based deploys
- **Context:** Multiple environments (dev/staging/prod), per-region clusters, ephemeral preview envs.
- **Decision:** Git is the source of truth. ArgoCD syncs. CI builds images, signs them, updates the GitOps repo; ArgoCD picks up and deploys.
- **Consequences:** Audit trail for every deploy. Rollback = `git revert`. Preview envs are cheap. Slower feedback loop than push-based; mitigated by GitHub Actions for dev/staging.
- **Reversibility:** High (it's tooling).

### ADR-010: Self-hosted observability stack, not Datadog
- **Context:** Observability is the silent cost killer (cardinality explosion). Self-hosting is ~5× cheaper at scale.
- **Decision:** Grafana + Loki + Tempo + Mimir + Prometheus, all self-hosted on EKS. OpenTelemetry as the SDK.
- **Consequences:** Need SRE capacity to operate. Cardinality discipline must be enforced in CI. Cost is predictable and ~$1.5–2.5k/mo at Launch.
- **Reversibility:** Medium. Switching to Grafana Cloud is straightforward; switching to Datadog is not.

### ADR-011: Cost target ≤ $0.10/MAU/mo, margin ≥ 30%
- **Context:** Without a cost target, infra spend grows with usage and engineers don't notice. Without a margin target, the business dies.
- **Decision:** Track these in the dashboard. Alert on either direction (above 0.15 = problem, below 0.05 = over-provisioned).
- **Consequences:** Forces cost-aware decisions at every layer (GPU spot, S3 tiering, cardinality discipline). Aligns eng and finance.
- **Reversibility:** High (it's a policy).

### ADR-012: Monorepo with uv + pnpm workspaces
- **Context:** Code is split across Go (api), Python (workers, sdks), and TypeScript (web, sdks). Multiple repos add overhead.
- **Decision:** One repo. `apps/` for services, `packages/` for shared libraries. Python via uv workspaces; TypeScript via pnpm workspaces; TS-task-graph via Turborepo.
- **Consequences:** Atomic cross-app refactors. Single CI. Single onboarding. Repo size grows; mitigated by sparse checkouts, path-based CI filters.
- **Reversibility:** Medium (splitting a monorepo is mechanical if needed).

---

## 5. Final System Architecture

### 5.1 One-line description

A **polyglot system**: **Go API tier** (Chi + slog + sqlc on `pgx`) on PostgreSQL with RLS, S3 for media, Redis for ephemeral state, **Python worker plane** (FastAPI control plane + Arq + Temporal + Ray Serve) for audio/ML, with **gRPC + Protobuf** between tiers, Keycloak for auth, OpenTelemetry for observability, and ArgoCD-driven GitOps on EKS multi-region. Public REST surface is served by the Go API tier with OpenAPI 3.1 generated from the Go code. See ADR-0013 for the polyglot decision.

### 5.2 Component map

```
┌────────────────────────────────────────────────────────────────────┐
│  Edge                                                                │
│  CloudFront (CDN + WAF) → ALB / NLB → Envoy Gateway (Gateway API)   │
└──────────────────────────────┬─────────────────────────────────────┘
                                │
┌──────────────────────────────▼─────────────────────────────────────┐
│  API tier — Go 1.22+ (Chi + slog + sqlc/pgx, stateless, HPA on      │
│              RPS + p99). Single static binary, distroless base.      │
│  ┌─────────────┬──────────────┬─────────────┬─────────────────┐    │
│  │ Identity &  │ Uploads      │ Jobs        │ Processors /    │    │
│  │ Access      │ (intent +    │ (CRUD +     │ Models          │    │
│  │ (Keycloak   │ presign S3)  │ dispatch)   │ (registry,      │    │
│  │  bridge)    │              │             │  versions)      │    │
│  ├─────────────┼──────────────┼─────────────┼─────────────────┤    │
│  │ Results     │ Webhooks     │ Billing     │ Notifications    │    │
│  │ (query +    │ (delivery    │ (metering,  │ (SSE, email)    │    │
│  │  signed URL)│  service)    │  Stripe)    │                 │    │
│  └─────────────┴──────────────┴─────────────┴─────────────────┘    │
│                                                                       │
│  Outbox table → outbox publisher → NATS JetStream                   │
│                                                                       │
│  Public surface: REST + JSON, OpenAPI 3.1 generated from Go code.   │
└───────────────────────────────────────────────────────────────────────┘
                                │
                                │  gRPC + Protobuf  (mTLS in-cluster,
                                │   buf-enforced schema, W3C Trace
                                │   Context propagated across the
                                │   boundary — see ADR-0010/ADR-0013)
                                │
┌──────────────────────────────▼─────────────────────────────────────┐
│  Worker plane — Python 3.12 (separate deployment, separate codebase) │
│                                                                       │
│  ┌──────────────────────┐    ┌──────────────────────────────────┐   │
│  │ Arq workers           │    │ Worker control plane (FastAPI,   │   │
│  │ (Python, simple,      │    │ Python) — exposes health,        │   │
│  │  <60s jobs)           │    │ model-registry status, and       │   │
│  │ - extract-metadata    │    │ admin endpoints to the Go API    │   │
│  │ - probe               │    │ over gRPC. See ADR-0002.         │   │
│  │ - probe-thumbnail     │    │                                  │   │
│  │ - slice               │    │                                  │   │
│  │ - presign-only ops    │    │                                  │   │
│  └──────────┬────────────┘    └────────────────┬─────────────────┘   │
│             │                                    │                     │
│             └────────────┬─────────────────────┘                      │
│                          ▼                                             │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │ Temporal workers (Python) — multi-step workflows                │  │
│  │ - transcribe-long (chunk+merge)                                  │ │
│  │ - diarize+align                                                  │ │
│  │ - demucs+remix                                                   │ │
│  │ - generate-and-mix                                               │ │
│  └──────────────────────────────┬──────────────────────────────────┘ │
│                                  │                                     │
│  ┌───────────────────────────────▼───────────────────────────────┐  │
│  │ Ray Serve (GPU inference plane, Python)                         │  │
│  │ - whisper-large-v3  (A10G, batched)                            │  │
│  │ - pyannote-3.1      (A10G)                                     │  │
│  │ - demucs-htdemucs   (A10G ×2)                                  │  │
│  │ - musicgen-large    (A100-80)                                  │  │
│  │ - panns / ecapa     (CPU tiny)                                 │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                       │
│  All workers run in gVisor sandbox, no egress, resource limits.       │
└───────────────────────────────────────────────────────────────────────┘
                                │
┌──────────────────────────────▼─────────────────────────────────────┐
│  State layer                                                          │
│  ┌──────────────┐  ┌────────────┐  ┌────────────┐  ┌────────────┐   │
│  │ PostgreSQL 16 │  │ Redis      │  │ S3         │  │ Temporal   │   │
│  │ (system of    │  │ (Arq queue │  │ (uploads,  │  │ (workflow  │   │
│  │  record)      │  │  + idem +  │  │  outputs,  │  │  state)    │   │
│  │               │  │  rate lim) │  │  models)   │  │            │   │
│  └──────────────┘  └────────────┘  └────────────┘  └────────────┘   │
│                                                                       │
│  ┌──────────────┐  ┌────────────┐  ┌────────────┐                   │
│  │ Keycloak     │  │ NATS       │  │ Model      │                   │
│  │ (auth)       │  │ JetStream  │  │ Registry   │                   │
│  │              │  │ (event bus)│  │ (S3 + PG)  │                   │
│  └──────────────┘  └────────────┘  └────────────┘                   │
└───────────────────────────────────────────────────────────────────────┘
                                │
┌──────────────────────────────▼─────────────────────────────────────┐
│  Observability (always-on, same cluster)                             │
│  OpenTelemetry Collector (DaemonSet)                                 │
│    → Prometheus (metrics) + Mimir (long-term)                        │
│    → Loki (logs)                                                     │
│    → Tempo (traces)                                                  │
│    → Grafana (dashboards + alerts)                                   │
│    → Alertmanager → PagerDuty / Slack                                │
│    → Pyroscope (continuous profiling, optional)                      │
└───────────────────────────────────────────────────────────────────────┘
```

### 5.3 Module boundaries (DDD bounded contexts)

| Context | Owns | Key aggregates | Notes |
|---|---|---|---|
| **Identity & Access** | users, orgs, teams, roles, API keys, OAuth tokens | `Organization`, `User`, `ApiKey`, `Role` | Bridges to Keycloak. Owns RBAC. |
| **Uploads** | upload sessions, multipart state, checksums | `UploadSession` | Distinct from artifacts. Short-lived. |
| **Artifacts** | stored media, codec, duration, checksums | `Artifact`, `ArtifactMetadata` | Long-lived. Reference-counted. |
| **Processors & Models** | processor versions, capabilities, deprecation | `Processor`, `ModelVersion`, `Capability` | The model catalog. |
| **Jobs** | units of work, state machine, attempts, cost | `Job`, `JobAttempt`, `JobCost` | Owns the job state machine. |
| **Workflows** | multi-job orchestrations | `Workflow`, `WorkflowStep` | Thin layer over Temporal. |
| **Results** | outputs, structured result, output artifacts | `Result`, `ResultArtifact` | Could merge with Jobs. Split if queries diverge. |
| **Billing** | plans, usage, quotas, invoices | `Plan`, `Subscription`, `UsageEvent` | PCI scope; externalize to Stripe. |
| **Notifications** | webhook endpoints, deliveries, SSE channels, email | `WebhookEndpoint`, `WebhookDelivery` | Cross-cutting. |

**Key clarification (from the architect's pushback):** "Processors" is **not** a bounded context. The domain is **Processors & Models**. A processor is the *executable*; a model version is the *weight bundle*; a capability is the *what* (transcribe, classify, separate). All three live in one bounded context.

### 5.4 Job orchestration rule

```
if job is single-activity, no signals, no humans, no saga, < 60s, no fan-out:
    use ARQ
else:
    use TEMPORAL
```

| Processor | Orchestrator | Why |
|---|---|---|
| `extract-metadata` | Arq | <500ms, no fan-out |
| `probe` (ffprobe) | Arq | <2s, no dependencies |
| `slice` (ffmpeg cuts) | Arq | seconds, single pass |
| `transcribe` short (<10 min) | Temporal (1 activity) | GPU, must retry cleanly, batched with siblings |
| `transcribe` long | Temporal workflow | chunk → parallel transcribe → stitch |
| `diarize + align` | Temporal workflow | two activities, dependent |
| `demucs + remix` | Temporal workflow | multi-step output |
| `musicgen` (interactive) | Temporal | short SLA, GPU, observable |

### 5.5 Event flow (outbox + NATS)

```
1. API writes outbox row in same DB transaction as the business state change
2. Outbox publisher (Postgres LISTEN/NOTIFY + worker with FOR UPDATE SKIP LOCKED)
   reads outbox → publishes to NATS JetStream → marks row published
3. NATS subjects:
   orpheus.jobs.created
   orpheus.jobs.completed
   orpheus.jobs.failed
   orpheus.uploads.completed
   orpheus.billing.usage.recorded
4. Subscribers:
   - Webhook delivery service → customer endpoint
   - Usage metering service → billing
   - Notification service → SSE fan-out
   - Analytics consumer → data lake
```

**Why NATS JetStream:** lightweight (single binary, 30MB), durable, in-cluster, no ZooKeeper. We considered Postgres LISTEN/NOTIFY alone — too easy to lose events on restart.

---

## 6. Service Interaction Diagrams

### 6.1 Upload + transcribe (end-to-end)

```
   Client              API (Go)              Postgres    S3             Arq          Worker         Ray Serve
     │                      │                   │         │              │              │                │
     │ POST /v1/uploads     │                   │         │              │              │                │
     │ (intent: filename,   │                   │         │              │              │                │
     │  content_type, size) │                   │         │              │              │                │
     │─────────────────────►│                   │         │              │              │                │
     │                      │ INSERT upload     │         │              │              │                │
     │                      │ session, write    │         │              │              │                │
     │                      │ outbox event      │         │              │              │                │
     │                      │──────────────────►│         │              │              │                │
     │                      │ generate presigned│         │              │              │                │
     │                      │ multipart URLs    │         │              │              │                │
     │                      │───────────────────────────────────────►  │              │                │
     │ ◄────────────────────│                   │         │              │              │                │
     │ 201 {upload_id,      │                   │         │              │              │                │
     │     parts: [         │                   │         │              │              │                │
     │       {url,part_no}, │                   │         │              │              │                │
     │       ...            │                   │         │              │              │                │
     │     ]}               │                   │         │              │              │                │
     │                      │                   │         │              │              │                │
     │ PUT parts to S3 (multipart, 5+ MB each)   │         │              │              │                │
     │─────────────────────────────────────────►│         │              │              │                │
     │                      │                   │         │              │              │                │
     │ POST /v1/uploads/{id}/complete           │         │              │              │                │
     │ (parts: [{part_no, etag}, ...])          │         │              │              │                │
     │─────────────────────►│                   │         │              │              │                │
     │                      │ verify all parts  │         │              │              │                │
     │                      │ present,          │         │              │              │                │
     │                      │ INSERT artifact   │         │              │              │                │
     │                      │ row, write outbox │         │              │              │                │
     │                      │──────────────────►│         │              │              │                │
     │ ◄────────────────────│                   │         │              │              │                │
     │ 200 {artifact_id,    │                   │         │              │              │                │
     │     sha256, size,    │                   │         │              │              │                │
     │     content_type,    │                   │         │              │              │                │
     │     duration_seconds}│                   │         │              │              │                │
     │                      │                   │         │              │              │                │
     │ POST /v1/jobs        │                   │         │              │              │                │
     │ {artifact_id,        │                   │         │              │              │                │
     │  processor: "orpheus.  │                   │         │              │              │                │
     │  audio.transcribe",  │                   │         │              │              │                │
     │  version: "1.4.2",   │                   │         │              │              │                │
     │  params: {...},      │                   │         │              │              │                │
     │  idempotency_key}    │                   │         │              │              │                │
     │─────────────────────►│                   │         │              │              │                │
     │                      │ INSERT job        │         │              │              │                │
     │                      │ (status=queued)   │         │              │              │                │
     │                      │ INSERT workflow   │         │              │              │                │
     │                      │ (Temporal),       │         │              │              │                │
     │                      │ write outbox      │         │              │              │                │
     │                      │──────────────────►│         │              │              │                │
     │                      │ outbox publisher  │         │              │              │                │
     │                      │ routes to Temporal│         │              │              │                │
     │                      │ (workflow > 60s)  │         │              │              │                │
     │                      │─Temporal─────────►│         │              │              │                │
     │ ◄────────────────────│                   │         │              │              │                │
     │ 202 {job_id,         │                   │         │              │              │                │
     │     status: queued,  │                   │         │              │              │                │
     │     poll_url}        │                   │         │              │              │                │
     │                      │                   │         │              │              │                │
     │  ... time passes, worker consumes ...     │         │              │              │                │
     │                      │                   │         │              │              │                │
     │                      │   Temporal worker picks up                    │                │
     │                      │─────────────────────────────────────────────►│                │
     │                      │   probe_audio (CPU, fast)                    │                │
     │                      │   ──────────────────────────────────────►   │                │
     │                      │   decision: long → slice                     │                │
     │                      │   slice_audio (CPU)                          │                │
     │                      │   ──────────────────────────────────────►   │                │
     │                      │   parallel transcribe_chunk × N (GPU)       │                │
     │                      │   ──────────────────────────────────────────────────────────►│
     │                      │                                                  Whisper  ...  │
     │                      │   stitch_transcripts (CPU)                      │                │
     │                      │   ──────────────────────────────────────►   │                │
     │                      │   persist_result (UPDATE job + INSERT result) │                │
     │                      │───────────────────────────────────────────────►                │
     │                      │   write outbox event "job.completed"          │                │
     │                      │   outbox publisher → NATS                      │                │
     │                      │   webhook delivery service → customer          │                │
     │ ◄───────────────────────────────────────────────────────────────── POST webhook   │
     │   {event: job.completed, job_id, result_url, model_version_id}                  │
     │                      │                                                   │                │
     │ GET /v1/jobs/{id}    │                                                   │                │
     │─────────────────────►│                                                   │                │
     │                      │ SELECT job, result                               │                │
     │ ◄────────────────────│                                                   │                │
     │ 200 {status: completed, result: {segments, text, language, ...},     │                │
     │      model_version_id: "whisper-large-v3@2024-09-12@sha256:9f7d",    │                │
     │      cost_usd: 0.04}                                                   │                │
```

### 6.2 Cancellation / failure flow

```
Client POST /v1/jobs/{id}/cancel
   → API: UPDATE job SET cancel_requested=true (CAS on status IN ('queued','running'))
   → outbox event "job.cancel_requested"
   → NATS → workflow controller
   → Temporal: workflow.signal("cancel", reason)
   → workflow raises CancellationScope (saga compensation runs in reverse)
   → in-flight activities get cancelled via ctx.cancel
   → workflow writes "job.cancelled" outbox event
   → webhook delivery → customer
```

### 6.3 DLQ flow

```
Activity fails after max_retries
   → Temporal: workflow.complete_as_failed(reason)
   → API service: INSERT INTO dead_letter_jobs (job_id, reason, history, payload)
   → Alertmanager: DLQ depth > 0 → Slack #ops
   → Admin UI: list, filter, requeue (with retry counter reset)
```

---

## 7. Data Flow Diagrams

### 7.1 Audio file lifecycle

```
Upload intent  ─────►  UploadSession (Postgres)  ─────► presigned URLs
                                                              │
                                                              ▼
Multipart PUTs  ──────────────────────────────────────► S3 (raw, encrypted SSE-KMS)
                                                              │
                                                              ▼
Complete call   ─────►  Artifact (Postgres)  ◄──── verify checksums
                            │  (sha256, size, content_type, duration)
                            │  (status: ready)
                            ▼
                      Reference-counted; survives job completion
                            │
        ┌───────────────────┼───────────────────┐
        ▼                   ▼                   ▼
    Transcribe job     Classify job      Slice job
        │                   │                   │
        ▼                   ▼                   ▼
   Result (JSONB)    Result (JSONB)    Result + S3 artifact
        │                   │                   │
        └───────────────────┴───────────────────┘
                            │
                            ▼
                  Webhook delivery (signed)
                            │
                            ▼
                  Customer endpoint
                            │
                            ▼
                  Tenant-initiated delete
                            │
                            ▼
                  S3 lifecycle → S3 delete
                  Postgres row → soft-delete
                  Audit log → retained N years
```

### 7.2 Result data model

```json
{
  "job_id": "018f6e7c-9a4b-7def-8a01-...",
  "processor": "orpheus.audio.transcribe",
  "version": "1.4.2",
  "model_id": "whisper-large-v3",
  "model_version_id": "whisper-large-v3@2024-09-12@sha256:9f7d...",
  "input_artifact_id": "...",
  "input_hash": "sha256:abc...",
  "params_hash": "sha256:def...",
  "started_at": "2026-07-09T10:23:45Z",
  "completed_at": "2026-07-09T10:24:32Z",
  "duration_seconds": 47.3,
  "result": {
    "language": "en",
    "language_probability": 0.98,
    "segments": [
      {"start": 0.0, "end": 4.2, "text": "Hello and welcome", "speaker": "S1", "confidence": 0.94}
    ],
    "text": "Hello and welcome to ..."
  },
  "artifacts": [
    {"name": "transcript.json", "url": "s3://orpheus-output/.../transcript.json", "size": 12345}
  ],
  "cost": {
    "gpu_seconds": 30.0,
    "cpu_seconds": 2.5,
    "memory_gb_seconds": 0.5,
    "s3_egress_bytes": 0,
    "total_usd": 0.04
  },
  "slo": {
    "target_p95_seconds": 45.0,
    "actual_seconds": 47.3,
    "met": false
  }
}
```

### 7.3 Tenancy data flow

```
Request arrives
   → API extracts JWT (org_id from claims)
   → API SET LOCAL app.current_org_id = org_id (per-request connection)
   → All DB queries subject to RLS policy
   → Background jobs: tenant_id is in the job row, set on the worker process via a per-task context
   → S3 keys: s3://orpheus-{tenant_id}-{env}/...
   → Redis keys: orpheus:{tenant_id}:*
   → Logs: tenant_id is a structured field
   → Traces: tenant_id is a span attribute (sampled)
```

---

## 8. API Strategy

### 8.1 Public API surface

- **REST + JSON** with **OpenAPI 3.1** generated from the Go code via [`oapi-codegen`](https://github.com/oapi-codegen/oapi-codegen) (server stubs and types) and served at `/api/openapi.json`.
- **URI versioning** (`/v1/...`).
- **6-month deprecation** with `Deprecation: true` and `Sunset: <date>` headers.
- **Webhooks** for async events (HMAC-SHA256 signed, 24-attempt retry over 24h, dead-letter queryable and replayable, auto-disable after 100 DLQs).
- **SSE** primary for real-time progress; WebSocket deferred to v2 collaborative features.
- **Idempotency-Key** header on POST/PUT/PATCH; same key + different body → 409.
- **RFC 7807** (Problem Details) error responses with `request_id`, `trace_id`, `errors[]`.
- **IETF draft RateLimit headers** + per-endpoint overrides; per-key and per-org with the lower winning.

### 8.2 Internal API

- **gRPC + mTLS (SPIFFE/SPIRE)** for service-to-service where low latency matters.
- HTTP/JSON for everything else.
- Proto schema in `packages/proto`; buf for breaking-change detection in CI.

### 8.3 Endpoint inventory (representative)

```
POST   /v1/uploads                     # create upload session → presigned URLs
POST   /v1/uploads/{id}/complete       # finalize upload, probe metadata
GET    /v1/uploads/{id}                # check status
DELETE /v1/uploads/{id}                # cancel / remove

POST   /v1/artifacts/{id}/signed-url    # request signed GET URL

GET    /v1/processors                  # list available processors
GET    /v1/processors/{name}           # describe processor (versions, schemas, SLO, cost)
GET    /v1/processors/{name}/versions  # list versions

POST   /v1/jobs                        # submit job (returns 202 + Location)
GET    /v1/jobs/{id}                   # poll status / get result summary
GET    /v1/jobs/{id}/result            # full structured result
POST   /v1/jobs/{id}/cancel            # request cancellation
POST   /v1/jobs/bulk                   # bulk submit (async, returns batch_id)

POST   /v1/webhooks                    # subscribe
GET    /v1/webhooks                    # list
PATCH  /v1/webhooks/{id}               # update
DELETE /v1/webhooks/{id}               # remove
GET    /v1/webhooks/{id}/deliveries    # inspect deliveries (replay, DLQ)
POST   /v1/webhooks/{id}/deliveries/{delivery_id}/replay

GET    /v1/api-keys                    # list
POST   /v1/api-keys                    # create (returns secret once)
DELETE /v1/api-keys/{id}               # revoke

GET    /v1/usage                       # current period usage
GET    /v1/billing/invoices

GET    /v1/health                      # liveness
GET    /v1/ready                       # readiness (checks DB, Redis, S3, Temporal, Keycloak)
```

### 8.4 SDK strategy

- **Generated** from OpenAPI: Python, TypeScript, Go, Java, Ruby.
- **Thin idiomatic layer** on top of generated code (e.g., Python `orpheus.Client` returns Pydantic models, TypeScript returns Zod-validated types).
- **Semver'd independently**; support N and N-1.
- **Examples** in `/examples` directory per language.
- **Postman collection** auto-generated from OpenAPI.

### 8.5 Authentication

- **Bearer JWT** (Keycloak) for browser/web clients — PKCE flow.
- **API keys** (`ak_live_...`, `ak_test_...`) for programmatic access.
- **Client credentials** for service-to-service.
- **API key format:** `ak_{env}_{body}` where `body` is 32 random bytes base62; only the SHA-256 hash is stored.
- **Scopes:** granular (`audio:write`, `job:read`, `webhook:manage`).
- **MFA** required for admin actions.

### 8.6 Real example: webhook signature

```
POST /your/webhook
Headers:
  X-Orpheus-Event: job.completed
  X-Orpheus-Delivery-Id: del_01HXYZ...
  X-Orpheus-Signature: t=1717920000,v1=5257a869...
  X-Orpheus-Webhook-Id: wh_01HABC...
  Content-Type: application/json

Body:
  {
    "api_version": "2026-07-01",
    "event": "job.completed",
    "data": {
      "job_id": "...",
      "status": "completed",
      "result_url": "/v1/jobs/.../result",
      "model_version_id": "whisper-large-v3@..."
    }
  }

Verification (your side):
  signed_payload = f"{t}.{body}"  # body = raw bytes
  expected = hmac_sha256(signed_payload, webhook_secret)
  constant_time_compare(expected, v1_signature)
  # also verify abs(now - t) < 300 (replay protection)
```

---

## 9. Database Design

(Full design lives in `docs/db-design.md`. Summary here.)

### 9.1 Core tables (19 total)

```
identity_and_access:
  organizations, users, org_members, teams, roles, permissions,
  role_permissions, api_keys, oauth_tokens

uploads:
  upload_sessions, upload_parts

artifacts:
  artifacts, artifact_metadata

processors_models:
  processors, processor_versions, models, model_versions, capabilities

jobs:
  jobs, job_attempts, job_costs, job_results, dead_letter_jobs

workflows:
  workflows, workflow_steps

billing:
  plans, subscriptions, usage_events, invoices

notifications:
  webhook_endpoints, webhook_deliveries, notification_channels

system:
  audit_log, idempotency_keys, rate_limit_buckets
```

### 9.2 Key design choices

| Choice | Decision | Why |
|---|---|---|
| PK type | **UUID v7** | Time-ordered, indexable, no enumeration leak, distributed-safe. |
| Tenancy | **Row-level with `FORCE ROW LEVEL SECURITY`** | `SET LOCAL app.current_org_id` per request. Defense in depth. |
| Job state | **State machine in `status` column with `version` for optimistic locking** | Atomic transitions via `UPDATE ... WHERE status = 'expected' AND version = X RETURNING ...`. |
| JSONB usage | **Only for processor-specific result and config** | Core entities stay typed for invariants. GIN index on `result jsonb_path_ops`. |
| Partitioning | **Monthly partitions on `jobs`, `usage_events`, `audit_log`, `webhook_deliveries`, `notifications`** | Auto-create + detach old. |
| Soft delete | **Yes for metadata, hard delete for audio** | GDPR right-to-erasure: actual audio files are deleted from S3. |
| Concurrency | **`SELECT ... FOR UPDATE SKIP LOCKED` for queue claim** | The recommended PG-native pattern. No advisory locks needed for the queue. |
| Migrations | **pgroll (expand-and-contract) + Alembic** | Online schema changes; pre-deploy job gates the rollout. |

### 9.3 Hot query patterns (top 5)

```sql
-- 1. Job claim (worker)
UPDATE jobs SET status='running', worker_id=$1, started_at=now(), version=version+1
WHERE id = (
  SELECT id FROM jobs
  WHERE status='queued' AND cancel_requested=false
  ORDER BY priority DESC, created_at ASC
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING *;

-- 2. Dashboard (tenant-scoped, RLS-enforced)
SELECT id, processor, status, created_at, completed_at, cost_usd
FROM jobs
WHERE created_at > now() - interval '30 days'
ORDER BY created_at DESC LIMIT 50;

-- 3. Webhook delivery (replay)
SELECT * FROM webhook_deliveries
WHERE endpoint_id=$1 AND event_type=$2 AND created_at > now() - interval '7 days'
ORDER BY created_at DESC LIMIT 100;

-- 4. Idempotency check
INSERT INTO idempotency_keys (key, org_id, request_hash, response, expires_at)
VALUES ($1, $2, $3, $4, now() + interval '24 hours')
ON CONFLICT (key, org_id) DO UPDATE SET request_hash = EXCLUDED.request_hash
RETURNING (xmax = 0) AS is_new;

-- 5. Usage rollup (hourly)
INSERT INTO usage_events_hourly (org_id, hour, processor, gpu_seconds, cpu_seconds, ...)
SELECT $1, date_trunc('hour', now()), processor, sum(gpu_seconds), sum(cpu_seconds), ...
FROM job_costs
WHERE tenant_id=$1 AND created_at > $2
GROUP BY 1,2,3
ON CONFLICT (org_id, hour, processor) DO UPDATE SET ...;
```

### 9.4 Read scaling

- **PgBouncer** in transaction mode; `max_connections=200` on PG; ~6× multiplexing.
- **Read replica** for dashboard and analytics queries; write path stays on primary.
- **Read-your-writes** consistency: API request writes go to primary; client gets a Redis tag indicating the write; subsequent reads check the tag and route to primary for N seconds.

### 9.5 Observability

- `pg_stat_statements` for top-N slow queries; weekly review.
- `pg_locks` for blocked-query alerts.
- `pg_stat_replication` for replica lag.
- **CDC** via `wal2json` → Kafka/Iceberg for analytics in v2.

### 9.6 Backup & DR

- **WAL-G**: nightly base backup + continuous WAL archiving to S3.
- **Cross-region** copy of backups.
- **PITR** target: 5 minutes.
- **Quarterly restore drill**.

---

## 10. Infrastructure Design

(Full design lives in `docs/infra-design.md`. Summary here.)

### 10.1 Cloud & topology

- **Cloud:** AWS (single cloud, multi-region).
- **Regions:** `us-east-1` (primary, active), `eu-west-1` and `ap-southeast-1` (warm standby).
- **Per region:** 3 AZs, EKS cluster, managed RDS, ElastiCache, MSK (Kafka for v2 CDC).
- **Multi-cloud:** deferred until >$5M ARR.

### 10.2 Compute

| Tier | Language / runtime | Per-pod target (CPU / memory) | Instance | Pricing | Use |
|---|---|---|---|---|---|
| **API** | **Go 1.22+ (static binary, no GIL, no runtime tax)** | **500m CPU request / 1 CPU limit · 128 MiB request / 256 MiB limit** | c7g.2xlarge (Graviton, 8 vCPU / 16 GiB) | 1-yr Savings Plan | Stateless Go API pods. No GIL ⇒ predictable scaling under load. ~7× fewer pods than a Python/uvicorn tier at the same RPS target. |
| API (control plane) | Python 3.12 + FastAPI | 250m CPU · 256 MiB | c7g.large | 1-yr Savings Plan | Small internal worker control plane (health, model registry, admin). |
| CPU workers | Python 3.12 | 1–4 CPU · 1–4 GiB (per job) | c7g.4xlarge (Graviton) | Spot (80%) + on-demand | ffmpeg, librosa, Arq jobs. |
| GPU workers (T4) | Python 3.12 + CUDA | 1 CPU · 4 GiB host + 1× T4 (16 GiB) | g4dn.xlarge | Spot 60–70% | Whisper small, classification. |
| GPU workers (A10G) | Python 3.12 + CUDA | 2 CPU · 8 GiB host + 1× A10G (24 GiB) | g5.xlarge | Spot + on-demand fallback | Whisper large, pyannote, Demucs. |
| GPU workers (A100-80) | Python 3.12 + CUDA | 8 CPU · 32 GiB host + 1× A100-80 (80 GiB) | p4d.24xlarge | On-demand | MusicGen, fine-tuning. |

**Why the Go API tier targets are tight:** with no GIL and no per-interpreted-frame overhead, a Go pod doing nothing but request handling, JSON, and a Postgres round-trip runs comfortably in 256 MiB at 1 vCPU. Setting a generous request/limit would waste the per-pod-density advantage that drives the cost math in §3.1. The HPA scales on RPS + p99 (see §12.3), not on memory, so over-sizing pods also slows scale-out.

### 10.3 Kubernetes

- **CNI:** Cilium (eBPF) — performance, no sidecar, network policy.
- **Ingress:** Envoy Gateway (Gateway API) — no ingress-nginx CVE history.
- **Service mesh:** none in v1; revisit at >50 services.
- **Sandbox:** gVisor (`runtimeClassName: gvisor`) on all worker namespaces.
- **Node autoscaling:** Karpenter — consolidates, picks instance type, scales to spot.
- **Pod autoscaling:** HPA on API (RPS, p99 latency); KEDA on workers (queue depth, GPU utilization).
- **Go API runtime characteristics (no GIL, small image, fast start).** The Go API tier ships as a single statically-linked binary on a distroless base — the container image is ~12 MiB (versus ~180 MiB for a Python/uvicorn image), pod start is sub-second (versus ~2.5 s for Python), and the memory footprint is ~10× smaller. In practice this means: (a) faster Karpenter consolidation and lower bin-packing waste, (b) faster HPA scale-out under burst load, (c) PR preview environments (per-PR ephemeral cluster, see §16.3) come up in seconds, (d) a much smaller supply-chain attack surface. No GIL means HPA on CPU/RPS is a meaningful signal — there is no hidden serialization point that a metric won't see.

### 10.4 Networking

- **VPC** with public + private subnets across 3 AZs.
- **VPC endpoints** for S3, ECR, Secrets Manager, CloudWatch (avoid NAT cost).
- **Network policies:** default deny in worker namespaces; explicit allow per egress proxy.
- **mTLS** for in-cluster service-to-service (SPIFFE/SPIRE) — v2.

### 10.5 Storage

- **S3** for media, with versioning + cross-region replication.
- **Lifecycle:** Standard (0–30d) → IA (30–90d) → Glacier IR (90–365d) → Glacier Deep Archive (365d+).
- **Intelligent-Tiering** for unpredictable access.
- **EBS gp3** for node-local scratch (encrypted).

### 10.6 Secrets

- **AWS Secrets Manager** for cloud secrets; **External Secrets Operator** syncs to K8s.
- **Per-tenant secrets** (e.g., customer's own cloud creds for "BYO storage") in a dedicated secret with tenant-scoped IAM.
- **Vault** deferred until we need dynamic per-tenant DB creds.

### 10.7 Local dev

- **Tilt** for orchestration; **k3d** for in-Docker K8s; **Telepresence** for fast service-mesh-free intercepts.
- **Rancher Desktop** as the K8s runtime on macOS/Windows.
- **Devcontainer** for VS Code.
- Full local stack up in ≤ 30 minutes; hot-reload for both Python and TS.

### 10.8 Deploy topology

```
dev cluster (1 region, 1 AZ) ── ephemeral preview envs per PR
staging cluster (1 region, 3 AZ) ── mirror of prod topology
prod-us-east-1 (3 AZ) ── active
prod-eu-west-1 (3 AZ) ── warm (scales up on Route53 health check fail)
prod-ap-southeast-1 (3 AZ) ── warm
```

### 10.9 Cost engineering

- Karpenter + Graviton + spot for ~70% compute savings.
- GPU spot mix 80% / on-demand 20%.
- CloudFront in front of S3 artifacts (60–80% cache hit rate).
- Cardinality discipline on observability labels (no `tenant_id`).
- Dev/staging scale-to-zero nights + weekends.
- **Target: ≤ $0.10/MAU/mo infra at 1M MAU; ≥ 30% gross margin.**

---

## 11. Security Architecture

(Full design lives in `docs/security-architecture.md`. Summary here.)

### 11.1 Threat model (STRIDE)

- **Spoofing:** JWT validation (RS256/ES256), JWKS rotation, key pinning.
- **Tampering:** S3 SSE-KMS with tenant-scoped CMK, upload checksum verification, signed URLs.
- **Repudiation:** Append-only `audit_events` mirrored to S3 Object Lock (Compliance mode).
- **Information disclosure:** RLS + app-level tenant checks; PII redaction in logs; S3 block-public-access.
- **Denial of service:** Rate limits, per-tenant GPU bulkheads, request size caps, request timeouts.
- **Elevation of privilege:** OPA/Rego for authz; Keycloak for authn; defense in depth.

### 11.2 Authentication

- **OAuth2/OIDC** flows: Authorization Code + PKCE (web), Client Credentials (service), Device Code (CLI).
- **JWT**: RS256 → ES256, JWKS endpoint, key rotation every 30 days, kid header.
- **API keys**: `ak_live_<32 bytes base62>`, SHA-256 hashed with Argon2id for storage, prefix-scoped.
- **MFA** required for admin and high-privilege actions.
- **Break-glass access** with time-bound audit log entry.

### 11.3 Authorization

- **OPA/Rego** as the policy engine (context-aware: time, IP, recent MFA, resource ownership).
- **Postgres RLS** as defense-in-depth safety net.
- **RBAC model:** Organization → Team → User → Role → Permission.
- **Externalized policy** in `packages/policies`; OPA bundles loaded per service.
- **Decision audit** in `audit_events` (subject, action, resource, decision, reason, context).

### 11.4 Worker isolation

- **gVisor** runtime class on all worker pods.
- **Default-deny egress** NetworkPolicy; allow-listed egress proxy per worker type (only for processors that need to call external APIs, with customer opt-in).
- **Resource limits:** CPU, memory, ephemeral storage, FDs, PIDs.
- **Read-only root filesystem**; writable scratch on emptyDir with size cap.
- **seccomp profile** (custom, denies ptrace, mount, etc.).
- **No privileged containers, no host paths, no host network.**
- **Per-tenant VRAM cap** on shared GPU pools.

### 11.5 Data protection

- **At rest:** S3 SSE-KMS with tenant-scoped CMK (paid+enterprise); RDS encryption; EBS encryption; Redis at-rest encryption.
- **In transit:** TLS 1.3 everywhere; mTLS for in-cluster.
- **PII detection** in transcripts (regex + small model) with redaction in logs and webhook payloads.
- **Data classification:** public / internal / confidential / regulated; field-level encryption for regulated.
- **Right-to-erasure** flow: tenant-initiated delete → 24h hard delete from S3 + soft delete from PG; audit-logged.

### 11.6 Supply chain

- **Distroless** base images (or Chainguard).
- **Cosign** keyless image signing; **SLSA L3** provenance; **Syft** SBOM.
- **Trivy + Grype** in CI; patch SLAs (24h critical, 7d high, 30d medium).
- **Renovate** for automated dependency updates.

### 11.7 Compliance roadmap

- **SOC 2 Type II** — Year 1: Type I. Year 2: Type II. Continuous controls monitoring from day 1.
- **GDPR** — DPO appointed, data processing agreement, sub-processor list public, EU region option, right-to-erasure flow.
- **HIPAA** — conditional tier (Business Associate Agreement + dedicated tenant). Not v1.
- **Pen test** annually; **bug bounty** 6 months post-launch.

### 11.8 Incident response

- **Sev-1 to Sev-4** definitions, IC/Security Lead/Comms/Scribe roles.
- **5 phases:** detect → contain → eradicate → recover → learn.
- **WORM forensic evidence locker** (S3 Object Lock).
- **Customer notification templates** per scenario.
- **Tabletop** 4×/year: credential leak, cross-tenant leak, sandbox escape, vendor ransomware.

---

## 12. Performance & Scalability Strategy

### 12.1 SLOs

| Service | Availability | Latency p95 | Latency p99 |
|---|---|---|---|
| API | 99.9% | 200 ms | 500 ms |
| Job submission | 99.9% | 300 ms | 800 ms |
| Job result query | 99.9% | 150 ms | 400 ms |
| Webhook delivery | 99.95% | 5 s | 30 s |
| Processing (per processor) | 99.5% | per manifest | per manifest |

### 12.2 Per-processor SLOs (sample)

| Processor | Input | p50 | p95 | p99 |
|---|---|---|---|---|
| extract-metadata | 50 MB | 200 ms | 500 ms | 1 s |
| transcribe (Whisper large) | 10 min | 18 s | 30 s | 60 s |
| transcribe (Whisper large) | 60 min | 90 s | 180 s | 300 s |
| diarize (pyannote 3.1) | 10 min | 25 s | 45 s | 90 s |
| demucs (htdemucs) | 4 min stereo | 35 s | 60 s | 120 s |
| musicgen-large (30s) | text prompt | 8 s | 15 s | 30 s |

### 12.3 Scaling levers

| Layer | Strategy | Trigger |
|---|---|---|
| API | HPA on RPS + p99 latency | 70% CPU or p99 > 200ms |
| CPU workers | KEDA on queue depth | depth > N per worker |
| GPU workers | KEDA on queue depth + GPU util | either > threshold |
| Postgres | Vertical scale + read replicas | connections > 80% |
| Redis | Vertical scale + eviction tuning | memory > 80% |
| S3 | Auto-scales; lifecycle tiering | storage class transitions |
| Temporal | Worker pool scales with task queue depth | backlog > N per worker |

### 12.4 Caching strategy

| What | Where | TTL | Invalidation |
|---|---|---|---|
| Auth token validation | Redis | 5 min | key rotation |
| User/org/role | In-process LRU + Redis | 60 s / 5 min | explicit on write |
| Job status (in-flight) | Redis | 60 s | on transition |
| Idempotency keys | Redis + Postgres | 24 h | TTL |
| Rate limit counters | Redis (Lua) | sliding window | continuous |
| S3 presigned URL | Redis | 5 min | key rotation |
| CDN (artifacts) | CloudFront | 1 h (configurable) | purge on delete |

### 12.5 Expected bottlenecks & mitigations

| Bottleneck | At scale | Mitigation |
|---|---|---|
| GPU pool exhaustion | 1M MAU peak | spot 80%, on-demand 20%, graceful degradation to third-party API |
| Postgres connection limit | 5M MAU | PgBouncer transaction mode, vertical scale, read replicas |
| S3 upload bandwidth | 10M MAU | multipart parallel, CloudFront in front of read |
| Webhook delivery throughput | 100k jobs/min | NATS fan-out, multiple delivery workers per priority lane |
| Redis memory | 1B events/day | eviction tuning, separate Redis for cache vs queue |
| Egress cost | Hyper scale | CloudFront cache 90%+, multi-region keep-in-region |
| Model cold start | Every deploy | pre-warmed model snapshots, min replicas ≥ 1 per active model |
| Temporal history | 10M workflows/mo | namespace sharding, archival to S3 |

### 12.6 Streaming & chunking

- **VAD-driven** chunking (silero-vad) for transcription of long speech.
- **30s chunks, 5s overlap** for ASR; **60s non-overlapping** for music.
- **Streaming inference** for live transcription (WebRTC → Opus → 200ms frames) in v2.

### 12.7 Caching & dedup

- **Content addressing** on intake (sha256 of first 64KB + last 64KB + size + content_type).
- **Result cache** per `(tenant, content_hash, processor@version, params_hash)`, default 30d.
- **Negative cache** for known-invalid files.

---

## 13. Observability Plan

### 13.1 Three pillars

| Pillar | Tool | Sample | Retention |
|---|---|---|---|
| Metrics | Prometheus + Mimir | 100% (counters, gauges), 10% (histograms tail) | 30d hot, 1y cold |
| Logs | Loki | 100% errors, 10% info, 0% debug | 7d hot, 30d warm, 1y cold |
| Traces | Tempo | 100% errors, 100% slow (p95), 1% healthy | 30d |

### 13.2 Cardinality discipline (in CI)

**Banned labels in Prometheus:** `tenant_id`, `user_id`, `job_id`, `audio_uri`, anything unbounded.

Allowed high-cardinality dimensions: `tenant_tier` (5 values), `processor` (10s), `version` (10s), `region` (3), `status` (5).

**CI rule:** Spectral-like lint on dashboards + PromQL analyzer to flag high-cardinality queries.

### 13.3 Key dashboards

- **API health:** RED (rate, errors, duration) per endpoint.
- **Worker health:** queue depth, job duration p50/p95/p99, success rate, GPU utilization.
- **Job lifecycle:** funnel from `queued` → `running` → `completed`/`failed`, broken down by processor.
- **Per-tenant:** jobs/min, cost today/MTD, failure rate, queue depth.
- **Cost:** $ / 1k jobs, GPU-min, egress, S3 storage.
- **Business:** MRR, paid tenants, top 10 tenants by usage.

### 13.4 SLO tracking

- **Multi-window burn-rate** alerts (Google SRE workbook).
- **Error budget** remaining visible in Grafana.
- **SLO violations** page on-call.

### 13.5 Alert routing

- **Sev-1** (SLO violation, data loss, security incident) → page on-call immediately.
- **Sev-2** (degradation, single-service impact) → Slack #oncall, no page.
- **Sev-3** (warning) → Slack #alerts.
- **Sev-4** (informational) → daily digest.

### 13.6 Continuous profiling

- **Pyroscope** for Go (api) and Python (workers) — flame graphs in Grafana.
- **Pyroscope** for TypeScript (web).
- **Nsys / nvidia-smi** snapshots ad-hoc for GPU regressions.

### 13.7 Synthetic monitoring

- **Canary job** every 5 minutes: upload → transcribe → fetch result. SLA breach → page.
- **Synthetic upload + download** every 5 minutes from multiple regions.
- **Synthetic webhook delivery** to a known-good endpoint.

---

## 14. Testing Strategy

### 14.1 Test pyramid

```
                          ┌─────────────┐
                          │   E2E (5)   │  Playwright + k6
                          ├─────────────┤
                       ┌──┤Integration(50)├──┐  testcontainers, schemathesis
                       │  └─────────────┘  │
                    ┌──┤   Contract (20)   ├──┐  pact, schemathesis
                    │  └──────────────────┘  │
                 ┌──┤     Unit (500+)       ├──┐  pytest, hypothesis
                 │  └──────────────────────┘  │
              ───┴────────────────────────────┴───
```

### 14.2 Unit tests

- **`go test`** for all Go (the public API tier).
- **pytest** for all Python (worker plane, contracts); **vitest** for TS (web).
- **hypothesis** for property-based testing of processors (e.g., "any valid MP3 returns a result with `duration_seconds > 0`").
- **Coverage target:** 80%+ on critical paths (processors, auth, billing); 60%+ overall.
- **Mutation testing** with `mutmut` on critical paths; gate promotion if mutation score < 70%.

### 14.3 Integration tests

- **testcontainers-python** spins up ephemeral Postgres, Redis, MinIO, Keycloak, Temporal per test.
- **Database isolation** via per-test schema; cleanup between runs.
- **No test shares state** with another test.
- **S3 mocking** via MinIO; no moto.
- **Temporal testing** via `temporalio.testing`.

### 14.4 Contract tests

- **Schemathesis** generates adversarial tests from OpenAPI.
- **pact** for provider/consumer contract between API and internal services.
- **buf** for proto breaking-change detection in CI.

### 14.5 E2E tests

- **Playwright** for the admin web dashboard.
- **k6** for load tests (job submission flood, polling storm, webhook delivery burst).

### 14.6 Performance regression tests

- **Pytest-benchmark** for CPU-bound processors.
- **GPU inference benchmark** in CI on every model bump; export Perfetto trace; gate promotion if p95 regresses >15%.
- **API latency benchmark** in CI on every PR (using k6 in CI).

### 14.7 Pre-merge checks

- Lint (ruff, biome)
- Type-check (pyright strict, tsc strict with `noUncheckedIndexedAccess`)
- Unit + integration tests
- Contract tests
- Coverage gate
- OpenAPI diff (oasdiff)
- Proto diff (buf)
- Security scan (Trivy, bandit, semgrep)
- Performance benchmark (API only, full GPU in nightly)

---

## 15. Deployment Strategy

### 15.1 Branch & release model

- **Trunk-based** with short-lived feature branches (≤ 2 days).
- **Feature flags** for risky changes (Flipt, OSS, GitOps).
- **Conventional commits** for changelog generation.
- **Semantic versioning** for public API; **CalVer** for infra images.

### 15.2 Progressive delivery

- **PR preview environments** auto-deployed per PR; cleaned up on merge/close.
- **Dev** auto-deploys on merge to `main`.
- **Staging** auto-deploys on green CI.
- **Production** requires manual approval (or auto-promote if all SLO checks pass for 10 min).
- **Argo Rollouts** canary: 5% → 25% → 50% → 100% with Prometheus-driven analysis (error rate, p99 latency, SLO burn rate).
- **Blue/green** for workers (atomic swap, drain old).

### 15.3 Rollback

- **API:** Argo Rollouts abort → previous revision.
- **Workers:** drain old, start new; if unhealthy, scale old back up.
- **Database:** migrations are forward-only (expand-and-contract); "rollback" is a forward migration that undoes the previous change.
- **Feature flag kill switch** for instant disable.

### 15.4 Migrations

- **Two-phase DB migration:**
  1. Phase 1: expand — add new column/table; deploy code that writes both.
  2. Phase 2: contract — drop old column/table in a separate deploy.
- **pgroll** for online schema changes; CI gate ensures no destructive migration in a single deploy.
- **Pre-deploy job** validates the migration (dry-run, row-count check).

### 15.5 Disaster recovery

- **RPO:** 5 minutes (PITR via WAL-G).
- **RTO:** 30 minutes (regional failover via Route53 health check + Argo Rollouts in warm region).
- **Quarterly game days:** simulate DB failure, region failure, S3 outage.
- **Documented runbooks** for each scenario.

---

## 16. CI/CD Pipeline Design

### 16.1 Pipeline (GitHub Actions)

```
PR opened/updated:
  ├─ lint (ruff, biome, prettier)
  ├─ type-check (pyright, tsc)
  ├─ unit-tests (go test, pytest, vitest)
  ├─ contract-tests (schemathesis, buf)
  ├─ coverage (gate 80%/60%)
  ├─ security-scan (trivy, bandit, semgrep, gitleaks)
  ├─ openapi-diff (oasdiff)
  ├─ perf-bench (API only, k6 in CI)
  ├─ build images (BuildKit, multi-stage, cosign sign, syft SBOM)
  └─ deploy preview env (per PR)

Merge to main:
  ├─ re-run full suite
  ├─ build + sign + push to ECR (with provenance)
  ├─ update GitOps repo (image tag bump)
  └─ ArgoCD syncs to dev cluster

Tag v* (release):
  ├─ full suite + perf bench + mutation test
  ├─ build release images
  ├─ publish SDKs to PyPI/npm/Maven
  └─ trigger Argo Rollouts to staging then prod (canary)
```

### 16.2 Caching

- `actions/cache` for uv cache, pnpm store, Playwright browsers.
- **BuildKit remote cache** (ECR-backed) for Docker layer reuse.
- **Turborepo remote cache** (Vercel or self-hosted) for TS tasks.

### 16.3 Preview environments

- One per PR; auto-deployed after CI passes.
- **Postgres + Redis + MinIO** per preview env (cheap: spot).
- **Tear down** on PR close.
- **URL** posted as PR comment.

### 16.4 Required status checks

- All checks above must pass.
- **CODEOWNERS** for sensitive areas (auth, billing, migrations).
- **Branch protection** on `main`: no direct push, require PR, require review.

### 16.5 Release cadence

- **API:** weekly (or on-demand for security fixes).
- **Workers:** weekly, aligned with API.
- **SDKs:** auto-generated on API release; published within 30 minutes.
- **Infra:** ad-hoc, reviewed by 2 SREs.

---

## 17. Implementation Roadmap

**Build order designed to maximize learning and revenue at each phase, not to be exhaustive.** Each phase is independently shippable and testable.

### Phase 0 — Foundation (Weeks 1–2)

**Objective:** Bootstrap the repo and dev workflow. Deploy a "hello world" polyglot 2-process service (one Go process for the public API, one Python process for the worker control plane) to a managed environment.

**Deliverables:**
- Monorepo skeleton (apps/, packages/, infra/, docs/).
- uv + pnpm workspaces; pyproject.toml; tsconfig; `go.mod`; pre-commit; ruff; biome; pyright; tsc; `golangci-lint`.
- Tiltfile for local dev; docker-compose for minimum stack (Postgres, Redis, MinIO).
- Terraform for one AWS account, one VPC, one EKS cluster (dev only).
- Hello-world Go API process with `/health` and `/ready` endpoints (Chi + slog), served via distroless image.
- Hello-world Python worker control plane (FastAPI, `/health`, `/ready`).
- OpenAPI auto-generation from the Go code, served at `/api/docs`.
- GitHub Actions CI (Go 1.22/1.23 + Python 3.12 matrix; lint, test, build, push; `buf` once `packages/proto` is added in Phase 1).
- Renovate config.
- ADR template + first 5 ADRs.

**Dependencies:** None.

**Risks:** Tooling friction (two languages, two toolchains in the monorepo). Mitigation: pair-program the first setup; document the "day 1" runbook; the Tiltfile keeps the dev loop single-command.

**Success criteria:** A new engineer can `git clone` → `tilt up` → `curl localhost:8080/health` returning 200 within **30 minutes**.

---

### Phase 1 — Core API & Auth (Weeks 3–6)

**Objective:** Real auth, real DB, real S3. Multi-tenant from day one.

**Deliverables:**
- PostgreSQL 16 (RDS, single AZ for now) with pgvector (future-proofing), pg_partman, pg_cron.
- Migrations via Alembic (Python) **or** `goose` (Go) — schema lives in one place, both sides read it.
- RLS policies on every table.
- Keycloak (deployed in-cluster, single replica) + JWT validation middleware in the Go API.
- **Go API** (`POST /v1/uploads` (intent), `POST /v1/uploads/{id}/complete`, `GET /v1/uploads/{id}`).
- S3 via presigned multipart URLs.
- Magic-byte + format probe on intake; size cap (1 GB).
- Idempotency key middleware.
- Per-tenant rate limit (Redis token bucket).
- Audit log table + middleware.
- Outbox table + publisher.
- NATS JetStream (single node).
- Webhook delivery service (HMAC signed, retry with backoff, DLQ) — lives in the Go API tier for v1.
- Basic admin endpoints: `GET /v1/api-keys`, `POST /v1/api-keys`.
- **Proto package** in `packages/proto` with the first service definitions (jobs, uploads); `buf lint` + `buf breaking` in CI.
- OpenAPI published; first SDK generated (Python + TypeScript).
- **Helm chart for the Go API** (separate chart for the Python worker control plane in Phase 2+).
- ArgoCD syncing dev/staging.

**Dependencies:** Phase 0.

**Risks:** Keycloak HA, RLS gotchas, idempotency edge cases.

**Success criteria:** End-to-end upload flow works; tenant A cannot see tenant B's uploads; webhook delivery has 99% success within 5 min.

---

### Phase 2 — Jobs & Arq (Weeks 7–10)

**Objective:** Async processing for the original use case (metadata + transcode). Single-activity, no workflows.

**Deliverables:**
- Arq workers (separate deployment).
- Processor plugin model: manifest, base class, registry, hot-reload via Redis pubsub.
- `extract-metadata` processor (port of current orpheus logic).
- `probe` processor (ffprobe).
- `slice` processor (ffmpeg).
- `convert-to-wav` processor (ffmpeg, no longer a stub).
- Job state machine: queued → running → completed/failed.
- `POST /v1/jobs`, `GET /v1/jobs/{id}`, `POST /v1/jobs/{id}/cancel`.
- Dead-letter table + alerting + requeue UI.
- Retry policy per processor.
- Per-tenant concurrency limits.
- Cost attribution per job (CPU-seconds, memory-gb-seconds, egress).
- Result JSONB with GIN index.
- Cleanup scheduled job (correct version, idempotent, observable).
- Grafana dashboards: API health, worker health, queue depth.
- **Helm chart for the Python worker control plane** (separate from the Go API chart added in Phase 1). The Go API talks to this service over gRPC for worker-pool health and model-registry status (see §5.2 and ADR-0002).

**Dependencies:** Phase 1.

**Risks:** Job state machine races, retry policy tuning, cost attribution accuracy.

**Success criteria:** Submit 1k jobs/min for an hour; zero stuck rows; p99 job latency matches manifest SLO.

---

### Phase 3 — Observability & SRE (Weeks 11–13)

**Objective:** Make the system operable. SLOs, alerts, on-call runbooks.

**Deliverables:**
- OpenTelemetry SDK in API + workers.
- OTel Collector (DaemonSet) → Prometheus + Loki + Tempo.
- Grafana dashboards (10+): API, workers, queues, DB, cost, per-tenant.
- SLO definitions and burn-rate alerts.
- Alertmanager → PagerDuty + Slack.
- Synthetic canary job every 5 min.
- Continuous profiling (Pyroscope).
- On-call runbooks (10 scenarios).
- Chaos engineering drill 1: kill a worker pod mid-job.
- Quarterly DR drill: restore from backup.

**Dependencies:** Phase 2.

**Risks:** Alert fatigue, dashboard sprawl.

**Success criteria:** First 3am page is solved in <30 min using a runbook; SLOs visible on a wall dashboard.

---

### Phase 4 — First Workflow: Transcribe-Long (Weeks 14–17)

**Objective:** Introduce Temporal for the first multi-step workflow. Validate the orchestration model.

**Deliverables:**
- Temporal Cloud (managed) account, or self-hosted in dev.
- Temporal worker pool.
- Workflow: `TranscribeLongWorkflow` (probe → slice → parallel transcribe → stitch → persist).
- Idempotency keys for activities.
- Saga compensation on cancel.
- GPU worker pool (A10G) with gVisor sandbox.
- Model registry (S3-backed) with checksum verification.
- `transcribe` processor (Whisper large-v3 via faster-whisper / CTranslate2).
- `diarize` processor (pyannote 3.1, gated on tenant HF token).
- Result alignment (diarize + transcript).
- Per-tenant GPU bulkhead.
- 80/20 spot/on-demand GPU mix.

**Dependencies:** Phase 3.

**Risks:** Temporal operational cost, GPU cold start, gVisor perf overhead.

**Success criteria:** Transcribe a 60-min podcast in <3 min p95; 99% of jobs succeed; cost per job < $0.05.

---

### Phase 5 — Production Hardening (Weeks 18–22)

**Objective:** Multi-region, HA, security audit, SOC 2 prep.

**Deliverables:**
- Multi-region active-passive (us-east-1 + eu-west-1).
- Postgres read replica; cross-region backup.
- Redis with cluster mode (or fail-over to managed KeyDB).
- Keycloak HA (2 replicas + DB).
- API KEYS with hashed storage.
- OPA/Rego for authz (alongside RLS).
- WAF rules (rate, geo, signature).
- gVisor enforced on all worker namespaces via admission controller.
- Supply chain: cosign keyless signing, SLSA L3, Syft SBOM, Trivy in CI.
- External Secrets Operator + AWS Secrets Manager.
- VPC endpoints for S3, ECR, Secrets, CW.
- SOC 2 Type I readiness: controls documented, evidence collection, pen test.
- Per-PR preview environments.
- Disaster recovery runbook tested.

**Dependencies:** Phase 4.

**Risks:** Cross-region latency, secrets sprawl, compliance overhead.

**Success criteria:** 99.9% API SLO for a month; SOC 2 Type I report; chaos drill passes.

---

### Phase 6 — Scale & Polish (Weeks 23–30)

**Objective:** Hit 100k MAU. Validate cost model. Polish UX.

**Deliverables:**
- Ray Serve deployment for GPU inference (replaces naive Ray actors).
- Dynamic batching on Whisper (5–10× throughput).
- Per-tenant GPU pool isolation (MIG for A100).
- Result cache (content-addressed).
- Web UI: admin dashboard (Next.js) with job history, usage, cost, webhooks.
- Web UI: docs site (Mintlify).
- OpenAPI linting + oasdiff in CI.
- SDK releases: Python, TypeScript, Go.
- Cost dashboard per tenant; usage-based billing rollup.
- Customer onboarding flow: signup → API key → first job.
- Migration to Temporal Cloud if self-hosting cost > $2k/mo.
- Real customer validation (5 design partners).

**Dependencies:** Phase 5.

**Risks:** SDK ergonomics, customer support overhead, cost surprise.

**Success criteria:** 5 paying customers; ≤ $0.15/MAU/mo; ≥ 30% gross margin.

---

### Phase 7 — Marketplace & BYO Model (Months 8–12, v2)

**Objective:** Open the platform. Third-party processors + tenant models.

**Deliverables:**
- Processor marketplace UI: discover, install, configure.
- Publisher CLI: `orpheus processor publish`.
- Three trust classes: `orpheus`, `verified:<partner>`, `community:<user>`.
- `community` runs in stricter sandbox (gVisor + seccomp + no network + ephemeral).
- BYO model upload (max 50 GB, HuggingFace import).
- LoRA fine-tuning per tenant (dedicated A100-80, max $200/run).
- Model marketplace for fine-tuned models.
- Federated cost reporting (per-publisher revenue share).
- Marketplace moderation queue.

**Dependencies:** Phase 6.

**Risks:** Sandbox escape, marketplace abuse, support cost.

**Success criteria:** 10 third-party processors published; 5 tenants with custom models.

---

### Phase 8 — Streaming & Realtime (Year 2, v2)

**Objective:** Real-time transcription. Up-market.

**Deliverables:**
- WebRTC ingress (LiveKit or mediasoup).
- Streaming ASR service (long-lived Ray Serve deployment, pre-warmed).
- WebSocket API (in addition to REST + SSE).
- SLA: p95 partial-transcript latency 800ms, p95 final 1.5s.
- Enterprise tier with dedicated GPU pools.
- Custom contracts.

**Dependencies:** Phase 7.

**Risks:** GPU cost explosion, real-time SLA.

**Success criteria:** 1 enterprise customer at $50k/yr.

---

## 18. Risk Analysis

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Team can't operate K8s | High (v1) | High | Phase 0–3 use managed services; K8s only in Phase 4. |
| Temporal upgrade breaks in-flight workflows | Medium | High | Pin Temporal version; test upgrade in staging; keep ≥ 1 minor version behind. |
| GPU cost surprise | High | High | Spot mix, cost dashboard, per-tenant caps, model distillation in v2. |
| RLS bypass via missed policy | Medium | Critical | Per-table test that asserts cross-tenant queries return 0 rows; CI lint. |
| Worker sandbox escape | Low | Critical | gVisor + seccomp + no egress + AV scan; chaos drill; bug bounty. |
| Vendor lock-in (S3, Keycloak) | Medium | Medium | Abstract via boto3; MinIO in dev; Keycloak is OSS and portable. |
| Cold start on first GPU job | High | Medium | Pre-warmed model snapshots, min replicas ≥ 1 per active model. |
| Webhook delivery silently failing | Medium | High | DLQ depth alert, customer-side endpoint tester, replay tool. |
| Idempotency key collision across tenants | Low | Medium | Keys are scoped `(key, org_id)`. |
| Cost attribution drift | Medium | Medium | Daily reconciliation against cloud billing; tenant-facing cost dashboard. |
| GDPR right-to-erasure breaks S3 | Low | High | Erasure flow tested in staging; 24h SLA on tenant-initiated deletes. |
| OAuth2 misconfiguration | Medium | High | Use mature library (Authlib); key rotation automated; threat-model-driven config review. |
| Supply chain attack (dependency) | Medium | High | Renovate auto-update, Trivy in CI, SBOM, pin transitive deps for critical packages. |
| Keycloak becomes unmaintained | Low | High | OSS; can self-host indefinitely; migration path to Auth0/WorkOS exists. |
| Schema migration breaks production | Medium | High | Expand-and-contract; pre-deploy validation job; CI gate. |
| Test coverage gap on critical path | Medium | High | Mutation testing on auth, billing, RLS; gate promotion. |
| PII in logs | Medium | High | PII redaction middleware; structured logging only; quarterly PII audit. |
| Multi-region failover doesn't actually work | Medium | High | Quarterly game day; documented runbook; auto-promotion tested. |

---

## 19. Cost Considerations

(Full analysis lives in `docs/cost-analysis.md`. Summary here.)

### 19.1 Cost model at scale

| Scale | MAU | API calls/mo | GPU-hr/mo | Egress | **Total $/mo** | $/MAU |
|---|---|---|---|---|---|---|
| Beta | 1k | 10M | 600 | 0.5 TB | $5k | $5.00 |
| Launch | 10k | 100M | 6,000 | 4 TB | $15.5k | $1.55 |
| Growth | 100k | 500M | 30,000 | 18 TB | $55k | $0.55 |
| Scale | 1M | 1B | 60,000 | 35 TB | $105k | $0.105 |
| Hyper | 10M | 10B | 600,000 | 300 TB | $950k | $0.095 |

### 19.2 Cost composition at Scale

| Category | $/mo | % |
|---|---|---|
| Compute (API + CPU workers) | 18,000 | 17% |
| GPU (spot mix) | 15,750 | 15% |
| Storage (S3 + Postgres + EBS) | 3,500 | 3% |
| Egress + NAT | 4,500 | 4% |
| Observability (self-hosted) | 3,000 | 3% |
| Third-party | 1,000 | 1% |
| Non-prod | 2,000 | 2% |
| Buffer | 5,000 | 5% |
| **Total** | **$52,750** | **100%** |

### 19.3 Top 10 cost levers (impact × effort)

1. **Karpenter + Graviton + spot** for all non-DB compute — saves ~$30k/yr.
2. **GPU spot mix 80/20** — saves ~$190k/yr at Scale.
3. **S3 lifecycle tiering** — saves ~$80k/yr at Scale.
4. **CloudFront in front of artifacts** — saves ~$200k/yr at Scale.
5. **VPC endpoints (no NAT processing)** — saves ~$3k/yr.
6. **Cardinality discipline on observability** — saves ~$15k/yr.
7. **Self-hosted Grafana vs managed** — saves ~$60k–300k/yr.
8. **Postgres partitioning + cold archive** — saves ~$20k/yr.
9. **Dev/staging scale-to-zero** — saves ~$25k/yr.
10. **Tiered log retention** — saves ~$50k/yr.

### 19.4 Cost attribution

```python
# Per-job cost formula
Cost(job) = (
    api_calls * $0.000003
  + cpu_seconds * $0.0000028
  + gpu_seconds * $0.00012
  + storage_gb_days * $0.00078
  + egress_gb * $0.06
  + webhook_deliveries * $0.00005
)
```

### 19.5 Pricing strategy

- **Free:** 10 jobs/mo, 1 GB storage.
- **Pay-as-you-go:** 1.4× markup on cost.
- **Startup:** 1.3× + commit discount.
- **Enterprise:** 1.2× + commit, custom SLA, dedicated GPU option.

**Margin floor: 30%.** Below 1.2× markup is dangerous; above 1.5× is uncompetitive.

### 19.6 Cost engineering process

- **Monthly cost review** (FinOps lead + eng leads).
- **Cost in every PR** (CI step flags new expensive resources).
- **Cost in every design doc** (estimated $ at 1M/10M/100M users).
- **Anomaly detection** (daily cost > 1.5× 7-day rolling average → Slack; > 2× 30-day → page).
- **Tagging enforcement** (OPA/Kyverno: untagged resources denied).
- **Quarterly right-sizing review** (Karpenter recommendations applied).

---

## 20. Future Enhancements

### 20.1 Year 1

- **Model marketplace v1** — first-party + verified partner processors.
- **BYO model** (community sandbox).
- **LoRA fine-tuning** per tenant.
- **Webhooks v2** — event filtering, batch delivery, retry policies.
- **Multi-cloud** (GCP failover).
- **Streaming inference** (real-time transcription).
- **Per-tenant SLOs** and dedicated GPU pools.
- **Advanced RBAC** — custom roles, resource-level permissions.

### 20.2 Year 2

- **Audio understanding pipeline** — transcribe → summarize → translate → caption.
- **Music generation** — MusicGen + Suno integration.
- **Voice cloning / TTS** (with consent + license gating).
- **Speaker identification** (cross-job memory).
- **Workflow templates** — first-class multi-step recipes users can fork.
- **Federated identity** — SAML SSO for enterprise.
- **Audit log streaming** to SIEM (Splunk, Datadog).
- **Multi-region active-active** with conflict resolution.

### 20.3 Year 3+

- **On-device inference** (TFLite, CoreML, ONNX Runtime) for privacy-sensitive tenants.
- **Confidential computing** (Azure Confidential VMs, AWS Nitro Enclaves) for regulated industries.
- **Cross-tenant model collaboration** — federated learning across tenant datasets.
- **AI agent platform** — processors that can call other processors, with planning.
- **Carbon-aware scheduling** — route jobs to regions with cleaner grid at any given moment.

---

## Appendix A — Sub-agent Reports

The following specialist planning agents contributed to this document:

1. **Software Architect** — validated the modular monolith, pushed back on Temporal+Arq split and over-engineering, surfaced ModelVersion as the biggest miss.
2. **Infrastructure & DevOps** — recommended AWS, Cilium eBPF, Envoy Gateway, gVisor, Argo Rollouts, Tilt + k3d + Telepresence for local dev.
3. **Database Architect** — designed the 19-table schema with UUID v7 PKs, row-level RLS, partitioning, FOR UPDATE SKIP LOCKED, pgroll, CDC via wal2json.
4. **Security Engineer** — recommended Keycloak + OPA/Rego, gVisor over Firecracker, cosign/SLSA, SOC 2 roadmap, IR playbooks.
5. **AI/ML Systems Engineer** — designed the processor plugin model, ModelVersion registry, Ray Serve, GPU pool topology, VAD chunking, content-addressed cache.
6. **API Designer** — REST + OpenAPI, URI versioning, RFC 7807 errors, idempotency, HMAC-signed webhooks, IETF rate-limit headers, generated SDKs.
7. **Developer Experience Engineer** — uv + pnpm workspaces, Tilt local dev, Renovate, Flipt, Mintlify, Graphite, quantified DX targets.
8. **Cost Optimization Analyst** — Karpenter + Graviton + spot, GPU spot mix, S3 tiering, CloudFront, cardinality discipline, ≤$0.10/MAU target.

## Appendix B — Open Questions

These should be resolved by the team before Phase 2 begins:

1. **Cloud:** AWS only, or AWS primary + GCP warm? (Default: AWS only.)
2. **Auth:** Keycloak self-hosted, or WorkOS for enterprise SSO from day 1? (Default: Keycloak.)
3. **Temporal:** Self-host from day 1, or start with Temporal Cloud? (Default: Temporal Cloud in Phase 4, evaluate self-host at $1M ARR.)
4. **Frontend framework:** Next.js (default), or Remix? (Default: Next.js.)
5. **B2B vs B2C first?** (Default: B2B — the design assumes paid tenants, not viral consumer traffic.)
6. **Single-region or multi-region at launch?** (Default: single region, multi-AZ. Multi-region in Phase 5.)
7. **Open source the core?** (Default: no, but write a public roadmap and architecture.)
8. **Sponsorship / grant strategy?** (Out of scope for this document.)

## Appendix C — Glossary

- **Orpheus** — the product and brand. The name is a reference to the figure from Greek myth whose music could charm all living things; here it is repurposed for a platform that processes audio with the precision and soul those myths promise.
- **Artifact** — a piece of media (audio, JSON transcript, image) stored in S3 with metadata.
- **Capability** — a high-level operation (transcribe, classify, separate, generate).
- **Job** — a single request to run a processor on an input artifact.
- **Model** — a named ML model (e.g., "whisper-large-v3").
- **ModelVersion** — a specific, content-addressed bundle of weights for a model. Pinned to a job for reproducibility.
- **Processor** — the executable implementation of a capability for a specific model version.
- **Tenant** — an organization; the boundary for isolation, billing, rate limits.
- **Workflow** — a multi-step job that may span days and involve human approval.

## Appendix D — Reading Order

If you are new to this document, read in this order:
1. §1 Executive Summary
2. §2 Business Problem Analysis
3. §4 ADRs (decisions, not details)
4. §5 Final System Architecture (the shape)
5. §8 API Strategy (the surface)
6. §17 Implementation Roadmap (the order)
7. §9 Database Design
8. §10 Infrastructure Design
9. §11 Security Architecture
10. §12 Performance & Scalability Strategy
11. The rest, as needed.

---

**End of design document.**

This document is the **target architecture**. The **roadmap (§17) is the build order**. We start small (Phase 0 is 2 engineers and 2 weeks) and grow into this design over 12 months. The cost target ($0.10/MAU), the SLOs (99.9% API), and the margin (30%) are the North Star metrics that every phase should advance.
