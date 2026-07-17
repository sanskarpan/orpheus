# Implementation status

Feature-by-feature status against the roadmap in
[`architecture/PRODUCTION_DESIGN.md`](architecture/PRODUCTION_DESIGN.md) §17.
Legend: ✅ done · 🟡 partial · ❌ not started.

> Note: the roadmap named some specific technologies (Arq, Temporal, Alembic,
> Helm/ArgoCD). The implementation made pragmatic substitutions — **NATS
> JetStream** for the job bus (instead of Arq), **goose** for migrations, and a
> **DB-tracked workflow** for transcribe-long (instead of Temporal). Those count
> as "done" where the capability exists, and are flagged where they diverge.

## Phase 0 — Foundation ✅
Monorepo (uv + pnpm), docker-compose stack, golangci-lint/ruff/pyright, CI,
ADRs, distroless API image. Done.

## Phase 1 — Core API & Auth 🟡 (~85%)

| Item | Status | Notes |
|------|--------|-------|
| Postgres 16 + RLS on every table | ✅ | `FORCE ROW LEVEL SECURITY` on all tenant tables; load-bearing (tested with a non-superuser role). |
| Migrations | ✅ | goose (not Alembic). |
| Keycloak JWT validation | ✅ | Verifier + middleware; Keycloak itself is docker-compose only (no HA deploy). |
| Upload endpoints + S3 presigned multipart | ✅ | Create/Complete/List/Get; 1 GB cap. |
| Magic-byte / format probe on intake | 🟡 | Content type is stored and probed **asynchronously** (probe job); it is not a synchronous gate at upload, and there's no AV scan. |
| Idempotency key middleware | ✅ | Reserve-before-execute; method+path+body scoped. |
| Per-tenant rate limit | ✅ | Redis sliding window, atomic Lua, fail-closed option. |
| Audit log + middleware | ✅ | Writes under RLS; middleware + handler-level records. |
| Outbox table + publisher | ✅ | Service-role tx drain to NATS. |
| NATS JetStream | ✅ | Single node. |
| Webhook delivery (HMAC, retry/backoff) | ✅ | SSRF-safe; retry/backoff/exhaust. **DLQ is a status (`exhausted`)**, not a dedicated table/UI. |
| API-key endpoints | ✅ | Create/List/Revoke; Argon2id + verification cache; scoped. |
| Proto package (jobs, uploads) + buf | ✅ | `buf lint`/`breaking` in CI. |
| OpenAPI published | ✅ | Served at `/api/docs`. |
| Generated SDKs (Python + TypeScript) | ❌ | Only internal gRPC stubs exist; no published client SDKs. |
| Helm chart for the API | ❌ | `infra/` is a placeholder. |
| ArgoCD dev/staging sync | ❌ | Not started. |

## Phase 2 — Jobs & async processing 🟡 (~65%)

| Item | Status | Notes |
|------|--------|-------|
| Async workers | ✅ | NATS JetStream consumer (not Arq). |
| Processor registry | 🟡 | `register_processor` decorator + registry; **no manifest / hot-reload / SLO metadata**. |
| `extract-metadata` / `probe` / `slice` processors | ✅ | Implemented + tested. |
| `convert-to-wav` | 🟡 | Exists as a helper used by transcribe; not a standalone processor. |
| Job state machine (queued→running→completed/failed) | ✅ | |
| `POST /v1/jobs`, `GET /v1/jobs/{id}`, cancel | ✅ | Cancel is `DELETE /v1/jobs/{id}` (spec says `POST .../cancel`). |
| Bulk create | ✅ | `POST /v1/jobs/bulk`. |
| Dead-letter table + requeue UI + alerting | ❌ | Only `failed`/`exhausted` statuses; no DLQ table or UI. |
| Retry policy per processor | 🟡 | `max_retries` column exists; webhook deliveries retry, but job-level retry orchestration is minimal. |
| Per-tenant concurrency limits | ❌ | Worker has a global concurrency knob only. |
| Cost attribution per job | 🟡 | `cost_usd` column exists; not computed. |
| Cleanup scheduled job | ❌ | Not implemented in orpheus (the adkil prototype had a broken one). |
| Grafana dashboards / queue-depth | ❌ | `/metrics` is exposed; no dashboards. |
| Worker Helm chart | ❌ | |

## Phase 3 — Observability & SRE 🟡 (~20%)

| Item | Status | Notes |
|------|--------|-------|
| OpenTelemetry SDK (API + workers) | ✅ | Tracing wired in both tiers. |
| Prometheus `/metrics` | ✅ | Per-instance registry. |
| OTel Collector → Prometheus/Loki/Tempo | ❌ | |
| Grafana dashboards, SLOs, burn-rate alerts | ❌ | |
| Alertmanager → PagerDuty/Slack | ❌ | |
| Synthetic canary, Pyroscope, runbooks, chaos drills | ❌ | |

## Phase 4 — Transcribe-Long workflow 🟡 (~35%)

| Item | Status | Notes |
|------|--------|-------|
| `transcribe` processor (faster-whisper) | ✅ | Chunked; params validated. |
| `transcribe-long` workflow endpoint + tracking | 🟡 | `workflows` table + endpoints exist; it's a **DB-tracked** flow, not Temporal, and stitch/saga are minimal. |
| Temporal + saga compensation | ❌ | |
| GPU worker pool + gVisor sandbox | ❌ | CPU only today. |
| Model registry (S3, checksums) | ❌ | Model is downloaded by faster-whisper. |
| `diarize` processor + alignment | ✅ | PRD 05 (#187): pyannote `diarize` + transcript/word alignment + word-level timestamps + SRT/VTT subtitle export. |

## PRD wave (2026-07) — feature expansion ✅

A wave of tenant-facing feature PRDs (`docs/prd/`) shipped on top of the existing
system. Each is org-scoped + RLS-forced, wired into the API and OpenAPI, emits
subscribable webhook events, and was verified end-to-end against real
Postgres + MinIO + NATS. Tracked as issues #195–#204.

| PRD | Feature | PR | Touches roadmap |
|-----|---------|----|-----------------|
| 01 | Content-addressed job result cache | #183 | Phase 6 "Result cache (content-addressed)" ✅ |
| 02 | Signed, expiring artifact download bundles / zip export | #184 | new |
| 03 | Webhook endpoint tester + delivery replay | #185 | Phase 1 webhook delivery ↑ |
| 04 | Language detect + translate + LLM summarize | #186 | new |
| 05 | Diarization + word timestamps + SRT/VTT export | #187 | Phase 4 diarize/alignment ✅ |
| 06 | Batch/callback API + presigned push to tenant S3 | #189 | new |
| 07 | Per-tenant usage analytics + budgets + cost rollup | #190 | Phase 6 "usage-based billing rollup" ✅; Phase 2 cost attribution ↑ |
| 08 | PII redaction in transcripts + logs | #191 | new |
| 09 | Resumable multipart uploads + SSRF-safe URL ingest | #192 | Phase 1 uploads ↑ |
| 10 | GDPR erasure (hard delete + verifiable S3 purge) | #193 | Phase 5 data-lifecycle compliance ↑ |

Comprehensive cross-feature e2e: #188 (PRD 01–05) and #194 (PRD 06–10).

## Phases 5–8 — ❌ not started (unchanged by the PRD wave)
Production hardening (multi-region/HA/SOC2), scale/polish (Ray Serve, web UI,
billing rollup, SDK releases), marketplace / BYO model, streaming/realtime.
The PRD wave delivered a few Phase 6 line items (result cache, usage rollup) and
a Phase 5 compliance path (GDPR erasure), but the infrastructure bulk of 5–8
remains not started.

---

## Summary — what is NOT implemented yet

Highest-value gaps, roughly in priority order:

1. **Synchronous upload validation** (magic-byte gate + AV scan) — today intake
   validation is async.
2. **Dead-letter queue** with a requeue path + alerting (only `exhausted`/`failed`
   statuses exist).
3. **Cost attribution** (compute `cost_usd` from CPU/GPU-seconds) — a per-tenant
   **usage rollup + budgets + cost analytics** now exists (PRD 07 / #190); computing
   `cost_usd` from actual CPU/GPU-seconds is still open.
4. **Per-tenant concurrency limits** and per-processor retry orchestration.
5. **Cleanup / retention job** (expire old uploads/artifacts/idempotency keys).
6. **Observability stack** — dashboards, SLOs, alerting, runbooks (OTel/metrics
   are emitted but nothing consumes them).
7. **Client SDKs** (Python/TypeScript) generated from OpenAPI/proto.
8. **Deploy tooling** — Helm charts, Terraform, ArgoCD (infra/ is a placeholder).
9. **Workflow engine** (Temporal) + saga compensation for multi-step flows.
10. **GPU inference** + model registry. (`diarize` + alignment now shipped on CPU —
    PRD 05 / #187.)
11. **Web UI** (admin dashboard, docs site) and **billing**.
12. **Streaming / realtime** transcription (WebRTC + WebSocket).
