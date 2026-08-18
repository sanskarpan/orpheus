# Orpheus — Product Requirements (PRD Index)

This directory holds engineer-ready PRDs for the next wave of Orpheus features.
Each PRD is scoped to fit the **existing** system as documented in
[`../architecture/PRODUCTION_DESIGN.md`](../architecture/PRODUCTION_DESIGN.md)
and the shipped API surface in
[`../../apps/api/internal/handlers/openapi.json`](../../apps/api/internal/handlers/openapi.json).

## System context every PRD assumes

- **Multi-tenant, org-scoped.** Every request carries an org (JWT `org_id` claim or
  API-key org). Postgres enforces isolation with `FORCE ROW LEVEL SECURITY` and
  `SET LOCAL app.current_org_id` per request. **Any new table is org-scoped and RLS-covered.**
- **Async job model.** `POST /v1/jobs` → `202` with `poll_url`; state machine
  `queued → running → completed/failed`; results in `jobs.result` (JSONB) plus
  optional output artifacts. Bulk via `POST /v1/jobs/bulk`.
- **Artifacts & uploads.** Media lives in S3 (SSE-KMS, key prefix per tenant/env);
  bytes never transit the API tier. Uploads use presigned multipart; artifacts are
  reference-counted and served via short-lived signed GET URLs.
- **Events.** Transactional **outbox → NATS JetStream** fans out to the webhook
  delivery service (HMAC-SHA256 signed, retry/backoff, `exhausted` on give-up),
  usage metering, and SSE. Webhook deliveries are queryable and replayable.
- **Cross-cutting conventions.** Idempotency-Key on writes; RFC 7807 Problem
  Details errors with `request_id`/`trace_id`; IETF RateLimit headers; audit log
  on privileged actions; UUID v7 PKs.
- **Reproducibility.** Every job pins a `(processor, version)` → `model_version_id`;
  same input + same params + same model = same output (basis for the dedup cache).

Divergences from the target design that PRDs must respect (see
[`../IMPLEMENTATION_STATUS.md`](../IMPLEMENTATION_STATUS.md)): the job bus is **NATS
JetStream** (not Arq), migrations are **goose**, `transcribe-long` is a **DB-tracked
workflow** (not Temporal), and GPU work now runs on **Modal** services
(`infra/modal/`) called from the workers (the original "CPU-only" note is superseded
as of v0.2.0); the DLQ is currently a **status (`exhausted`/`dead_letter`)** rather
than a dedicated table/UI.

## Index — original design PRDs (v0.1.0 line)

| # | PRD | One-liner |
|---|-----|-----------|
| 01 | [Idempotent job dedup by content hash](01-content-hash-dedup.md) | Content-addressed result cache: identical `(input, params, model)` returns the prior result for free. |
| 02 | [Signed, expiring artifact bundles / zip export](02-artifact-bundles.md) | Package multiple artifacts/results into one signed, expiring downloadable zip. |
| 03 | [Webhook tester + delivery replay UI](03-webhook-tester-replay.md) | Self-serve test-fire, delivery inspection, and replay for webhook debugging. |
| 04 | [Language auto-detect, translation & summarization](04-translate-summarize.md) | Detect language, translate transcripts, and LLM-summarize as first-class processors. |
| 05 | [Diarization, word timestamps & subtitle export](05-diarization-subtitles.md) | Speaker labels, word-level timestamps, and SRT/VTT export. |
| 06 | [Batch/callback API + presigned push to tenant S3](06-batch-callback-tenant-s3.md) | Async batches with completion callbacks and result push to the tenant's own S3. |
| 07 | [Per-tenant usage analytics + budget alerts](07-usage-analytics-budgets.md) | Time-series usage, cost breakdowns, budgets, and threshold alerts. |
| 08 | [PII redaction in transcripts and logs](08-pii-redaction.md) | Detect and redact PII in transcript output and in operational logs. |
| 09 | [Resumable uploads + URL ingest](09-resumable-url-ingest.md) | Resume interrupted multipart uploads; ingest audio by fetching a URL. |
| 10 | [GDPR erasure endpoint](10-gdpr-erasure.md) | Tenant-initiated hard delete with verifiable S3 purge and audit proof. |

## Conventions used across PRDs

- **Endpoints** extend the existing `/v1` surface and reuse `Problem`, `Job`,
  `Artifact`, `WebhookEvent`, and idempotency semantics rather than inventing new ones.
- **Data-model changes** name concrete tables/columns and state RLS + partitioning
  expectations. Migrations are goose; JSONB only for heterogeneous payloads.
- **Security sections** are mandatory and always cover tenant isolation, abuse/DoS,
  and audit.
- **Sizing:** each PRD is intentionally reviewable in one sitting.

---

## Epic PRDs — remaining backlog (`prd-NN-*.md`, added 2026-08)

These 16 epic PRDs cover **every unchecked and 🟡-partial item** in
[`../../FEATURES-AND-ISSUES.md`](../../FEATURES-AND-ISSUES.md). Each follows a shared
template (Summary · Motivation & goals · Current state in Orpheus (file:line) ·
Proposed design · Rollout/phases · Verification · Dependencies/risks · Effort).

| # | PRD | Scope |
|---|-----|-------|
| 01 | [`prd-01-asr-quality.md`](prd-01-asr-quality.md) | Forced alignment (WhisperX), ITN/smart formatting, VAD-segmented long-file chunking, code-switching |
| 02 | [`prd-02-realtime-streaming.md`](prd-02-realtime-streaming.md) | Sub-300 ms partials, semantic turn detection, in-stream diarization, eager end-of-turn, interim confidence, realtime PII |
| 03 | [`prd-03-audio-intelligence-plus.md`](prd-03-audio-intelligence-plus.md) | PII upgrade (Presidio/LLM + audio beep), emotion, audio-event detection, profanity/moderation, auto-chapters, multichannel, speaker enrollment |
| 04 | [`prd-04-audio-enhancement.md`](prd-04-audio-enhancement.md) | Noise suppression, background-voice cancellation, echo/de-reverb, voice isolation, accent conversion, telephony denoise |
| 05 | [`prd-05-voice-agent.md`](prd-05-voice-agent.md) | Barge-in, backchannel, active-listening, voicemail detection, SIP/RTP/DTMF ingestion, full-duplex |
| 06 | [`prd-06-meeting-intelligence.md`](prd-06-meeting-intelligence.md) | Meeting bot, live notes/action items, cross-meeting search, ask-AI, highlight reels, collaboration, conversation-intel, CRM, audio-edit-by-text, overdub/TTS |
| 07 | [`prd-07-dictation-flow.md`](prd-07-dictation-flow.md) | LLM cleanup pass (dual output), backtrack, command/transform, context-conditioned, tone/prompt presets, romanized, flow latency |
| 08 | [`prd-08-developer-surface.md`](prd-08-developer-surface.md) | OpenAI/Deepgram-compatible endpoint, client SDKs, MCP server, callback URL, processor SDK |
| 09 | [`prd-09-cost-billing.md`](prd-09-cost-billing.md) | LLM token-cost pass-through, real-cost cache savings, billing↔metering coupling, cost dashboards |
| 10 | [`prd-10-infra-scaling.md`](prd-10-infra-scaling.md) | Inference batching, in-app autoscaling, dynamic concurrency, model-registry wiring, multi-model routing |
| 11 | [`prd-11-security-compliance.md`](prd-11-security-compliance.md) | Keycloak/OIDC, real IdP, rate-limiter policy, SOC2/HIPAA/SSO/SCIM, WS CheckOrigin, secrets mgmt, marketplace sandbox, ZDR |
| 12 | [`prd-12-data-lifecycle.md`](prd-12-data-lifecycle.md) | Transcript store/search, semantic search/KB, data residency, retention/TTL, streaming artifact delivery |
| 13 | [`prd-13-observability.md`](prd-13-observability.md) | Per-model/GPU metrics, cost dashboards, SLA, autoscaling consumer, streaming/ListDeliveries tests, GPU soak |
| 14 | [`prd-14-model-deployment.md`](prd-14-model-deployment.md) | On-device/edge artifacts, 1000+ langs (MMS), speech-to-speech, human tier, BYO training, forced-align-to-text, self-host/on-prem |
| 15 | [`prd-15-frontier-niche.md`](prd-15-frontier-niche.md) | Realtime voiceprint ID, realtime audio-event, realtime audio PII, emotion/acoustic-scene, ambient injection, AI QA scoring |
| 16 | [`prd-16-captions-and-local-dev.md`](prd-16-captions-and-local-dev.md) | Caption styling + burn-in; streaming server in `make dev` |

### Coverage map — checklist → PRD

**Part A (issues):** A1 → 01 · A2 → 02 (+ `make dev` in 16) · A3 → 03 · A4 → 09 · A5 → 10 · A6 → 13 · A7 → 11 · A8 → 12 · A9 → 08 · A10 → 13 · A11 → 06 / 04 / 07 / 16.

**Part B (features):**
- **P0** → 01 (#298 forced align, #299 ITN), 08 (#303 SDKs); all others shipped.
- **P1 Realtime** → 02 (#306–312).
- **P1 Audio intelligence** → 03 (#313 PII, #320 enrollment, #322 multichannel, #323 profanity), 01 (#321 code-switch).
- **P1 Platform/infra** → 10 (#325 batching, #326 autoscaling), 08 (#327 OpenAI-compat, #330 MCP), 11 (#328 HIPAA), 12 (#329 residency), 14 (#335 custom training).
- **P2 Differentiators** → 14 (#334 self-host/on-prem), 11 (#432 marketplace sandbox).
- **P2 Dictation flow** → 07 (#338–344, #376 prompt modes).
- **P2 Audio enhancement** → 04 (#345–350).
- **P2 Voice-agent** → 05 (#351–356).
- **P2 Meeting/media** → 06 (#357–367).
- **P2 Model/deployment** → 03 (#368 emotion, #369 audio-event), 14 (#370 s2s, #371 on-device, #372 MMS, #373 human tier, #374 forced-align-to-text, #375 ZDR).
- **P3 Frontier/niche** → 15 (#377–385; #381/#382/#385 flagged as app-UX, not platform scope).

Every checklist item resolves to at least one PRD above; the app-UX-only P3 rows
(Talon-style OS control, watched-folders) are called out as out-of-platform-scope
rather than built.
