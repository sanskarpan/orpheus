# Implementation status

Feature-by-feature status against the roadmap in
[`architecture/PRODUCTION_DESIGN.md`](architecture/PRODUCTION_DESIGN.md) §17.
Legend: ✅ done · 🟡 partial · ❌ not started.

> **Audited 2026-07-17** against the actual codebase (not from memory). A prior
> version of this doc badly *undersold* the implementation — it missed the
> `apps/workflows/` Temporal module, the `monitoring/` observability stack, the
> `streaming.py` WebSocket service, the published-shape SDKs, and the `infra/`
> Helm/Terraform/ArgoCD tree. The numbers below are evidence-based with file
> references.

> Note: the roadmap named specific technologies (Arq, Alembic). The
> implementation made pragmatic substitutions — **NATS JetStream** for the job
> bus (instead of Arq) and **goose** for migrations. Temporal *is* now used for
> the transcribe-long workflow (`apps/workflows/`), alongside a DB-tracked
> `workflows` table for status. Substitutions count as "done" where the
> capability exists and are flagged where they diverge.

## Phase 0 — Foundation ✅
Monorepo (uv + pnpm), docker-compose stack, golangci-lint/ruff/pyright, CI,
ADRs, distroless API image. Done.

## Phase 1 — Core API & Auth ✅ (~95%)

| Item | Status | Notes |
|------|--------|-------|
| Postgres 16 + RLS on every table | ✅ | `FORCE ROW LEVEL SECURITY` on all tenant tables; load-bearing (tested with a non-superuser role). |
| Migrations | ✅ | goose (not Alembic); currently through `0016_erasure.sql`. |
| Keycloak JWT validation | ✅ | Verifier + middleware; Keycloak itself is docker-compose only (no HA deploy — that's Phase 5). |
| Upload endpoints + S3 presigned multipart | ✅ | Create/Complete/List/Get; 1 GB cap enforced at Create (`uploads.go:144`). |
| **Synchronous** upload validation | ✅ | Content-type allow-list at Create (415) **and** authoritative magic-byte sniff at Complete (`audiosniff.go`), which deletes the object on mismatch. Async probe job still runs after. |
| AV / malware scan | ❌ | `AVScanner` interface is wired into upload-complete but the scanner is `nil` (no active scan). Only real remaining Phase 1 gap. |
| Idempotency key middleware | ✅ | Reserve-before-execute; method+path+body scoped; true-concurrency test. |
| Per-tenant rate limit | ✅ | Redis sliding window, atomic Lua, fail-closed option. |
| Audit log + middleware | ✅ | Writes under RLS; middleware + handler-level records. |
| Outbox table + publisher | ✅ | Service-role tx drain to NATS; OTel spans per publish. |
| NATS JetStream | ✅ | Single node. |
| Webhook delivery (HMAC, retry/backoff) | ✅ | SSRF-safe; retry/backoff/exhaust; tester + delivery replay (PRD 03). |
| API-key endpoints | ✅ | Create/List/Revoke; Argon2id + verification cache; scoped. |
| Proto package (jobs, uploads) + buf | ✅ | `buf lint`/`breaking` in CI. |
| OpenAPI published | ✅ | Served at `/api/docs`; validated in CI. |
| Client SDKs (Python + TypeScript) | ✅ | Hand-written, publishable clients in `packages/sdk-python` (498 LOC) + `packages/sdk-typescript` (356 LOC). Not yet *published* to PyPI/npm (that's a Phase 6 release step); no Go SDK. |
| Retention sweeper | ✅ | Expires abandoned pending uploads (aborts S3 multipart) + deletes expired idempotency keys (`retention/sweeper.go`). |
| Helm charts (API + worker) | ✅ | `infra/helm/orpheus-api` + `orpheus-worker` — real templates (deployment/service/hpa/configmap/scaledobject). |
| ArgoCD dev/staging sync | ✅ | `infra/argocd/` ApplicationSet + per-env Applications + project. |

## Phase 2 — Jobs & async processing ✅ (~90%)

| Item | Status | Notes |
|------|--------|-------|
| Async workers | ✅ | NATS JetStream consumer (not Arq). |
| Processor registry | 🟡 | `register_processor` decorator + registry; per-processor metadata (timeout, max_retries, cost, tier, i/o schema) lives in the DB `processors` catalog rather than an in-code manifest. No hot-reload. |
| `extract-metadata` / `probe` / `slice` processors | ✅ | Implemented + tested. |
| `convert-to-wav` | 🟡 | Exists as an inline ffmpeg helper used by transcribe/diarize; the `convert-to-wav` job-type enum value is unused (no standalone processor). |
| Job state machine (queued→running→completed/failed) | ✅ | Plus `dead_letter` and `canceled`. |
| `POST /v1/jobs`, `GET /v1/jobs/{id}`, cancel | ✅ | Cancel is `DELETE /v1/jobs/{id}` (spec says `POST .../cancel`). |
| Bulk create | ✅ | `POST /v1/jobs/bulk`. |
| **Dead-letter queue** + requeue + event | ✅ | `dead_letter` status (migration 0005), worker `term()`s on exhaustion and emits `job.dead_letter` (worker.py:237), `POST /v1/jobs/{id}/requeue` resets and re-enqueues. No dedicated alert rule yet beyond the metric. |
| Retry policy per processor + orchestration | ✅ | Per-job `attempts`/`max_retries`; exponential backoff capped at 60s via JetStream `nak(delay=...)`. |
| Per-tenant concurrency limits | ✅ | `per_org_concurrency` (default 8); worker defers with `nak` when an org is at capacity. Worker-side only. |
| Cost attribution per job | ✅ | Computed from wall-clock × `cost_usd_per_second` at completion; aggregated into `usage_rollup_hourly` (PRD 07). Per-processor `cost_per_job_usd` column still unused. |
| Cleanup / retention job | ✅ | See Phase 1 retention sweeper. |
| Grafana dashboards | ✅ | `monitoring/grafana/dashboards/workers-queue.json` (+ 3 others). |
| Direct queue-depth metric | 🟡 | Dashboards infer backlog from `rate(submitted) - rate(processed)`; no direct NATS `num_pending` gauge. |
| Worker Helm chart | ✅ | `infra/helm/orpheus-worker` with KEDA `scaledobject`. |

## Phase 3 — Observability & SRE 🟡 (~40%)

| Item | Status | Notes |
|------|--------|-------|
| OpenTelemetry SDK (API + workers) | ✅ | Tracing wired both tiers; `otelhttp` auto-instrumentation; spans on outbox + job processing. |
| Prometheus `/metrics` | ✅ | API + worker registries; documented metric set. |
| OTel Collector → Prometheus/Loki/Tempo | ✅ | `monitoring/otel-collector/config.yaml` with all three pipelines + `docker-compose.observability.yml`. |
| Grafana dashboards | 🟡 | 4 provisioned (api, workers-queue, database-rls, cost-usage); roadmap target was 10+. |
| SLO definitions + burn-rate alerts | ✅ | `docs/SLOs.md` (4 SLOs) + `monitoring/prometheus/alerts.yml` (multi-window burn-rate alerts). |
| On-call runbooks | 🟡 | 5 in `docs/runbooks/`, cross-linked from alerts; target was 10. |
| Alertmanager → PagerDuty/Slack | ❌ | Prometheus `alerting.alertmanagers` targets list is empty; no Alertmanager deployed. |
| Synthetic canary (5 min) | ❌ | No canary/CronJob. |
| Continuous profiling (Pyroscope) | ❌ | Not present. |
| Chaos drills / DR drill | ❌ | Not present (overlaps Phase 5). |

## Phase 4 — Transcribe-Long workflow 🟡 (~60%)

| Item | Status | Notes |
|------|--------|-------|
| `transcribe` processor (faster-whisper) | ✅ | Chunked with segment-offset adjustment; params validated. |
| **Temporal** `TranscribeLongWorkflow` | ✅ | Real `apps/workflows/` module (temporalio): probe → plan chunks → bounded-parallel transcribe → stitch → persist. |
| Saga compensation on cancel | ✅ | Reverse-order artifact cleanup on cancel/failure; unit + replay tested (`test_transcribe_long.py`). |
| `workflows` table + endpoints | ✅ | DB-tracked status alongside Temporal (migration 0003, `handlers/workflows.go`). |
| Idempotency for activities | 🟡 | `persist` is CAS-idempotent; probe/stitch read-only; no dedicated activity idempotency-key table. |
| `diarize` processor + alignment | ✅ | PRD 05 (#187): pyannote `diarize` (stub fallback) + transcript/word alignment + word-level timestamps + SRT/VTT export. |
| API → Temporal trigger wiring | 🟡 | Go API still creates the DB-tracked path; the Temporal worker exists but is not yet started from the API on every request. |
| GPU worker pool + gVisor sandbox | ❌ | CPU only. |
| Model registry (S3, checksums) | ❌ | Model downloaded by faster-whisper; no registry/checksum. |

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
| 07 | Per-tenant usage analytics + budgets + cost rollup | #190 | Phase 6 "usage-based billing rollup" 🟡; Phase 2 cost attribution ✅ |
| 08 | PII redaction in transcripts + logs | #191 | new |
| 09 | Resumable multipart uploads + SSRF-safe URL ingest | #192 | Phase 1 uploads ↑ |
| 10 | GDPR erasure (hard delete + verifiable S3 purge) | #193 | Phase 5 data-lifecycle compliance ↑ |

Comprehensive cross-feature e2e: #188 (PRD 01–05) and #194 (PRD 06–10).

## Phase 5 — Production Hardening 🟡 (~15%)

| Item | Status | Notes |
|------|--------|-------|
| API keys with Argon2id | ✅ | Load-bearing; hash+verify with DoS-guard cache. |
| RLS as authz layer | ✅ | Every table FORCE RLS (this is the primary tenant-isolation control). |
| Redis failover | 🟡 | ElastiCache `automatic_failover_enabled` for 2+ nodes; not cluster (sharded) mode. |
| Postgres HA / read replica | 🟡 | RDS Multi-AZ standby; no read replica, no cross-region backup. |
| Single-region Terraform (EKS/VPC/RDS/ElastiCache/S3) | ✅ | `infra/terraform/` modules provision one region. |
| Multi-region active-passive | ❌ | Single region only. |
| OPA/Rego authz | ❌ | RLS only. |
| WAF rules | ❌ | Rate-limit is in the Go API, not a WAF. |
| gVisor enforced via admission controller | ❌ | ADR-0008 only; no `runtimeClassName` in Helm. |
| Supply chain (cosign/SLSA/SBOM/Trivy) | ❌ | CI runs `pip-audit`; no image signing/SBOM/scan. |
| External Secrets Operator + AWS Secrets Manager | 🟡 | Referenced in Helm comments; not deployed. |
| VPC endpoints | ❌ | No `aws_vpc_endpoint` resources. |
| SOC 2 Type I readiness | ❌ | No control mapping/evidence. |
| Per-PR preview environments | ❌ | Not in CI. |
| DR runbook tested | ❌ | No DR runbook. |

## Phase 6 — Scale & Polish 🟡 (~25%)

| Item | Status | Notes |
|------|--------|-------|
| Result cache (content-addressed) | ✅ | PRD 01 (#183); RLS-scoped `job_result_cache`. |
| Usage-based billing rollup (schema + rollup) | 🟡 | PRD 07 (#190) ships `usage_rollup_hourly`/`budgets`/alerts + computes cost; no invoice/Stripe pipeline. |
| SDKs (Python, TypeScript) | 🟡 | Exist (v0.2.0) but not published; no CI release; no Go SDK. |
| Admin dashboard (Next.js) | 🟡 | `apps/web` is an explicit **scaffold** (`0.0.0-scaffold`, build scripts error out); not wired, not in CI. |
| Ray Serve GPU inference / dynamic batching / MIG | ❌ | In-process CPU whisper only. |
| Docs site (Mintlify) | ❌ | Markdown ADRs/design docs only. |
| OpenAPI linting + oasdiff in CI | ❌ | Runtime spec validation only; no lint/diff. |
| Customer onboarding flow | ❌ | No signup/trial provisioning. |
| Temporal Cloud migration | ❌ | Self-hosted Temporal only. |
| Design-partner validation | ❌ | Not started. |

## Phase 7 — Marketplace & BYO Model ❌ (0%)
Genuinely untouched — no schema, no code, no design doc. Marketplace UI, publisher
CLI, trust classes, community sandbox, BYO-model upload, LoRA fine-tuning,
federated cost reporting, moderation queue all not started.

## Phase 8 — Streaming & Realtime 🟡 (~15%)

| Item | Status | Notes |
|------|--------|-------|
| WebSocket streaming ASR | ✅ | `apps/workers/src/orpheus_workers/streaming.py` — standalone FastAPI on :8082, `StreamSession` state machine, partial/final frames, offline faster-whisper; unit + e2e tested. |
| WebRTC ingress (LiveKit/mediasoup) | ❌ | Not present. |
| Session REST API + `streaming_sessions` persistence | ❌ | No API endpoints, no DB table, no result persistence to jobs. |
| SLA latency instrumentation (p95 partial) | ❌ | No latency metrics. |
| Enterprise tier / dedicated GPU pools / custom contracts | ❌ | Not started. |

---

## Summary — genuinely remaining gaps (by phase)

Verified 2026-07-17. Most of the roadmap's *core capabilities* exist; the gaps are
now specific.

- **Phase 1 (~95%)** — only: **active AV/malware scan** (interface is `nil`).
- **Phase 2 (~90%)** — **`convert-to-wav` standalone processor**; **direct
  queue-depth gauge**; (optional) in-code processor manifest/hot-reload.
- **Phase 3 (~40%)** — Alertmanager→PagerDuty/Slack wiring; synthetic canary;
  Pyroscope; more dashboards/runbooks; chaos/DR.
- **Phase 4 (~60%)** — GPU pool + gVisor; model registry + checksums; wire the Go
  API to actually start the Temporal workflow; richer stitch/alignment.
- **Phase 5 (~15%)** — multi-region, WAF, gVisor-enforce, supply-chain
  (cosign/SLSA/SBOM/Trivy), ESO, VPC endpoints, SOC 2, preview envs, DR.
- **Phase 6 (~25%)** — Ray Serve + dynamic batching + MIG; real admin UI; Mintlify;
  oasdiff/lint; SDK publish + Go SDK; cost invoicing; onboarding; Temporal Cloud.
- **Phase 7 (0%)** — entire marketplace / BYO-model surface (greenfield).
- **Phase 8 (~15%)** — WebRTC ingress; streaming session REST API + persistence;
  SLA instrumentation; enterprise tier.
