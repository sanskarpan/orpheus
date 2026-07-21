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

## Phase 1 — Core API & Auth ✅ (100%)

| Item | Status | Notes |
|------|--------|-------|
| Postgres 16 + RLS on every table | ✅ | `FORCE ROW LEVEL SECURITY` on all tenant tables; load-bearing (tested with a non-superuser role). |
| Migrations | ✅ | goose (not Alembic); currently through `0016_erasure.sql`. |
| Keycloak JWT validation | ✅ | Verifier + middleware; Keycloak itself is docker-compose only (no HA deploy — that's Phase 5). |
| Upload endpoints + S3 presigned multipart | ✅ | Create/Complete/List/Get; 1 GB cap enforced at Create (`uploads.go:144`). |
| **Synchronous** upload validation | ✅ | Content-type allow-list at Create (415) **and** authoritative magic-byte sniff at Complete (`audiosniff.go`), which deletes the object on mismatch. Async probe job still runs after. |
| AV / malware scan | ✅ | Always-on built-in EICAR `SignatureScanner` + optional `clamd` (INSTREAM) behind `CLAMAV_ADDR`, chained; wired into upload-complete (infected → 422 + object deleted). `avscan` package. |
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

## Phase 2 — Jobs & async processing ✅ (~98%)

| Item | Status | Notes |
|------|--------|-------|
| Async workers | ✅ | NATS JetStream consumer (not Arq). |
| Processor registry | 🟡 | `register_processor` decorator + registry; per-processor metadata (timeout, max_retries, cost, tier, i/o schema) lives in the DB `processors` catalog rather than an in-code manifest. No hot-reload. |
| `extract-metadata` / `probe` / `slice` processors | ✅ | Implemented + tested. |
| `convert-to-wav` | ✅ | Standalone processor: transcodes a source artifact to 16 kHz mono WAV, writes a new `audio/wav` artifact (deterministic id); catalog-seeded (0017) so it's API-submittable. |
| Job state machine (queued→running→completed/failed) | ✅ | Plus `dead_letter` and `canceled`. |
| `POST /v1/jobs`, `GET /v1/jobs/{id}`, cancel | ✅ | Cancel is `DELETE /v1/jobs/{id}` (spec says `POST .../cancel`). |
| Bulk create | ✅ | `POST /v1/jobs/bulk`. |
| **Dead-letter queue** + requeue + event | ✅ | `dead_letter` status (migration 0005), worker `term()`s on exhaustion and emits `job.dead_letter` (worker.py:237), `POST /v1/jobs/{id}/requeue` resets and re-enqueues. No dedicated alert rule yet beyond the metric. |
| Retry policy per processor + orchestration | ✅ | Per-job `attempts`/`max_retries`; exponential backoff capped at 60s via JetStream `nak(delay=...)`. |
| Per-tenant concurrency limits | ✅ | `per_org_concurrency` (default 8); worker defers with `nak` when an org is at capacity. Worker-side only. |
| Cost attribution per job | ✅ | Computed from wall-clock × `cost_usd_per_second` at completion; aggregated into `usage_rollup_hourly` (PRD 07). Per-processor `cost_per_job_usd` column still unused. |
| Cleanup / retention job | ✅ | See Phase 1 retention sweeper. |
| Grafana dashboards | ✅ | `monitoring/grafana/dashboards/workers-queue.json` (+ 3 others). |
| Direct queue-depth metric | ✅ | `orpheus_jetstream_pending_messages` gauge (stream, consumer), polled from `consumer_info().num_pending` every `queue_depth_poll_seconds`. |
| Worker Helm chart | ✅ | `infra/helm/orpheus-worker` with KEDA `scaledobject`. |

## Phase 3 — Observability & SRE 🟡 (~55%)

| Item | Status | Notes |
|------|--------|-------|
| OpenTelemetry SDK (API + workers) | ✅ | Tracing wired both tiers; `otelhttp` auto-instrumentation; spans on outbox + job processing. |
| Prometheus `/metrics` | ✅ | API + worker registries; documented metric set. |
| OTel Collector → Prometheus/Loki/Tempo | ✅ | `monitoring/otel-collector/config.yaml` with all three pipelines + `docker-compose.observability.yml`. |
| Grafana dashboards | 🟡 | 4 provisioned (api, workers-queue, database-rls, cost-usage); roadmap target was 10+. |
| SLO definitions + burn-rate alerts | ✅ | `docs/SLOs.md` (4 SLOs) + `monitoring/prometheus/alerts.yml` (multi-window burn-rate alerts). |
| On-call runbooks | 🟡 | 6 in `docs/runbooks/` (added canary), cross-linked from alerts; target was 10. |
| Alertmanager → PagerDuty/Slack | ✅ | `monitoring/alertmanager/alertmanager.yml` — page→PagerDuty+Slack, ticket→Slack, inhibition; wired into Prometheus + compose (#214). Secrets via `*_file`. |
| Synthetic canary (5 min) | ✅ | `cmd/canary` probes `/health` + `/ready`, exports `orpheus_canary_*`; `CanaryDown`/`CanaryAbsent` alerts + k8s Deployment (#213). |
| Direct queue-depth alert | ✅ | `JetStreamPendingHigh` on the `orpheus_jetstream_pending_messages` gauge (#213). |
| Continuous profiling (Pyroscope) | ❌ | Not present. |
| Chaos drills / DR drill | ❌ | Not present (overlaps Phase 5). |

## Phase 4 — Transcribe-Long workflow 🟡 (~70%)

| Item | Status | Notes |
|------|--------|-------|
| `transcribe` processor (faster-whisper) | ✅ | Chunked with segment-offset adjustment; params validated. |
| **Temporal** `TranscribeLongWorkflow` | ✅ | Real `apps/workflows/` module (temporalio): probe → plan chunks → bounded-parallel transcribe → stitch → persist. Stitch merges chunk **segments onto an absolute timeline** (not just concatenated text) (#216). |
| Saga compensation on cancel | ✅ | Reverse-order artifact cleanup on cancel/failure; unit + replay tested (`test_transcribe_long.py`). |
| `workflows` table + endpoints | ✅ | DB-tracked status alongside Temporal (migration 0003, `handlers/workflows.go`). |
| Idempotency for activities | 🟡 | `persist` is CAS-idempotent; probe/stitch read-only; no dedicated activity idempotency-key table. |
| `diarize` processor + alignment | ✅ | PRD 05 (#187): pyannote `diarize` (stub fallback) + transcript/word alignment + word-level timestamps + SRT/VTT export. |
| API → Temporal trigger wiring | 🟡 | Go API still creates the DB-tracked path; the Temporal worker exists but is not yet started from the API on every request. |
| GPU worker pool + gVisor sandbox | ❌ | CPU only (needs GPU hardware + a k8s admission controller to implement/test). |
| Model registry (S3, checksums) | ✅ | `model_registry` table + `ModelRegistry.register/resolve` with sha256 verification on load (tamper rejected) (#215). Wiring processors to source weights through it is a follow-up. |

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

## Phase 5 — Production Hardening 🟡 (~35%)

| Item | Status | Notes |
|------|--------|-------|
| API keys with Argon2id | ✅ | Load-bearing; hash+verify with DoS-guard cache. |
| RLS as authz layer | ✅ | Every table FORCE RLS (this is the primary tenant-isolation control). |
| Supply chain (cosign/SLSA/SBOM/Trivy) | ✅ | CI Trivy fs+secret scan + Syft SBOM (#218); release cosign keyless sign + SBOM attest + Trivy image scan. |
| WAF rules | ✅ | `terraform/modules/waf`: per-IP rate limit + AWS managed rule sets + geo-block (#222, `terraform validate`). |
| VPC endpoints | ✅ | `terraform/modules/vpc-endpoints`: S3 gateway + ECR/Secrets/Logs interface (#222). |
| gVisor sandbox | 🟡 | RuntimeClass + worker Helm `runtimeClassName` (opt-in) (#222); **not yet enforced** by an admission controller. |
| Redis failover | 🟡 | ElastiCache `automatic_failover_enabled` for 2+ nodes; not cluster (sharded) mode. |
| Postgres HA / read replica | 🟡 | RDS Multi-AZ standby; no read replica, no cross-region backup. |
| Single-region Terraform (EKS/VPC/RDS/ElastiCache/S3) | ✅ | `infra/terraform/` modules provision one region. |
| Multi-region active-passive | ❌ | Single region only (needs a rollout to a second region). |
| OPA/Rego authz | ❌ | RLS only. |
| External Secrets Operator + AWS Secrets Manager | 🟡 | Referenced in Helm comments; not deployed. |
| SOC 2 Type I readiness | ❌ | No control mapping/evidence. |
| Per-PR preview environments | ❌ | Not in CI. |
| DR runbook tested | ❌ | No DR runbook. |

See `infra/HARDENING.md` for the validated-vs-infra-bound ledger.

## Phase 6 — Scale & Polish 🟡 (~35%)

| Item | Status | Notes |
|------|--------|-------|
| Result cache (content-addressed) | ✅ | PRD 01 (#183); RLS-scoped `job_result_cache`. |
| OpenAPI linting + oasdiff in CI | ✅ | Redocly lint + oasdiff breaking-change gate (#219). |
| Usage-based billing rollup (schema + rollup) | 🟡 | PRD 07 (#190) ships `usage_rollup_hourly`/`budgets`/alerts + computes cost; no invoice/Stripe pipeline. |
| SDKs (Python, TypeScript) | 🟡 | Exist (v0.2.0) but not published; no CI release; no Go SDK. |
| Admin dashboard (Next.js) | 🟡 | `apps/web` is an explicit **scaffold** (`0.0.0-scaffold`, build scripts error out); not wired, not in CI. |
| Ray Serve GPU inference / dynamic batching / MIG | ❌ | In-process CPU whisper only (needs GPU hardware). |
| Docs site (Mintlify) | ❌ | Markdown ADRs/design docs only. |
| Customer onboarding flow | ❌ | No signup/trial provisioning. |
| Temporal Cloud migration | ❌ | Self-hosted Temporal only. |
| Design-partner validation | ❌ | Not started. |

## Phase 7 — Marketplace & BYO Model 🟡 (~20%)

| Item | Status | Notes |
|------|--------|-------|
| Trust classes (first_party/verified/community) | ✅ | `processors.trust_class` + `publisher` (#221). |
| Moderation queue (submit → review → promote) | ✅ | `marketplace_submissions` + `/v1/marketplace/*`; approval promotes into the catalog (#221). |
| Publisher CLI | ✅ | `cmd/orpheus-publish` submits a community processor (#221). |
| Community sandbox (gVisor + no-egress) | 🟡 | RuntimeClass + NetworkPolicy config (#222); not enforced. |
| Marketplace discovery UI | ❌ | API only; no UI. |
| BYO-model upload / HuggingFace import | ❌ | Needs the model-registry ingest path + storage quota. |
| LoRA fine-tuning per tenant | ❌ | Needs GPU (A100) infra. |
| Federated cost reporting (revenue share) | ❌ | Not started. |

## Phase 8 — Streaming & Realtime 🟡 (~30%)

| Item | Status | Notes |
|------|--------|-------|
| WebSocket streaming ASR | ✅ | `apps/workers/src/orpheus_workers/streaming.py` — standalone FastAPI on :8082, `StreamSession` state machine, partial/final frames, offline faster-whisper; unit + e2e tested. |
| Session REST API + `streaming_sessions` persistence | ✅ | `0019_streaming_sessions.sql` + `/v1/streaming/sessions` create/get/list/finalize (persists transcript + billable audio + cost) (#220). |
| WebRTC ingress (LiveKit/mediasoup) | ❌ | Needs media/SFU infra. |
| SLA latency instrumentation (p95 partial) | ❌ | No latency metrics. |
| Enterprise tier / dedicated GPU pools / custom contracts | ❌ | Not started. |

---

## Summary — genuinely remaining gaps (by phase)

Verified 2026-07-17. Most of the roadmap's *core capabilities* exist; the gaps are
now specific.

- **Phase 1 (100%)** — complete. AV/malware scan now active (built-in EICAR +
  optional clamd).
- **Phase 2 (100%)** — complete (manifest + catalog-sync + NATS hot-reload, #212).
- **Phase 3 (~55%)** — Alertmanager (#214), synthetic canary + queue-depth
  alert (#213). Remaining: Pyroscope, more dashboards/runbooks, chaos/DR (cluster).
- **Phase 4 (~70%)** — model registry + checksums (#215), richer stitch (#216).
  Remaining: GPU pool + gVisor (hardware), API→Temporal trigger wiring (server).
- **Phase 5 (~35%)** — supply-chain CI (#218), WAF + VPC-endpoints + gVisor
  RuntimeClass config (#222). Remaining: multi-region, OPA/Rego, ESO, SOC 2,
  gVisor enforcement, preview envs, DR.
- **Phase 6 (~35%)** — oasdiff/lint in CI (#219). Remaining: Ray Serve/batching/MIG
  (GPU); admin UI; Mintlify; SDK publish + Go SDK; cost invoicing; onboarding.
- **Phase 7 (~20%)** — trust classes + moderation + publisher CLI (#221), community
  sandbox config (#222). Remaining: discovery UI, BYO-model, LoRA (GPU), revenue share.
- **Phase 8 (~30%)** — streaming session REST API + persistence (#220). Remaining:
  WebRTC ingress (media infra), SLA instrumentation, enterprise tier.

> Items marked "needs hardware/cluster/server" are delivered as validated
> config/design where possible (see `infra/HARDENING.md`) but cannot be
> exercised end-to-end in this environment.
