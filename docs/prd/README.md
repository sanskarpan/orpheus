# Orpheus — Product Requirements (PRD Index)

This directory holds engineer-ready PRDs for Orpheus. Each PRD is scoped to fit the **existing**
system as documented in [`../architecture/PRODUCTION_DESIGN.md`](../architecture/PRODUCTION_DESIGN.md)
and the shipped API surface in
[`../../apps/api/internal/handlers/openapi.json`](../../apps/api/internal/handlers/openapi.json).

> **Renumbered 2026-08-20.** This directory previously had two colliding numbering sequences (an
> unprefixed `01`–`10` "v0.1.0 line" and a `prd-`-prefixed `01`–`16` "epic backlog" line added
> 2026-08) that shared the same numbers under different filenames. Both are now one flat
> `prd-01`…`prd-28` sequence: the original 10 kept their numbers (`prd-01`…`prd-10`), the 16 epic
> PRDs shifted by a flat `+10` (`prd-11`…`prd-26`, old→new = old+10), and two new personal-path
> PRDs were appended as `prd-27`/`prd-28`. **The status column below is verified against actual
> code as of 2026-08-20, not against this doc's own prior "remaining backlog" framing — several
> entries were significantly more (or, in a few cases, less) complete than that framing implied.**
> See `git log -- docs/prd/` for the rename commit if you need the old→new mapping.

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
[`../IMPLEMENTATION_STATUS.md`](../IMPLEMENTATION_STATUS.md) — note: that doc is dated 2026-07-17
and is itself stale against several PRDs below, same as this index was before this renumbering):
the job bus is **NATS JetStream** (not Arq), migrations are **goose**, `transcribe-long` is a
**DB-tracked workflow** (not Temporal), and GPU work now runs on **Modal** services (`infra/modal/`)
called from the workers; the DLQ is currently a **status** (`exhausted`/`dead_letter`) rather than a
dedicated table/UI.

## Unified index — status verified 2026-08-20

| # | PRD | Scope | % done | What's left |
|---|-----|-------|---|---|
| 01 | [Idempotent job dedup by content hash](prd-01-content-hash-dedup.md) | Content-addressed result cache | **~95%** | Usage-event billing record on cache hits, ref-counted artifact array, per-org gating, named metrics — all observability/edge-case. |
| 02 | [Signed, expiring artifact bundles / zip export](prd-02-artifact-bundles.md) | Package artifacts/results into a signed, expiring zip | **~85%** | No zip-bomb size ceiling, no distinct raw-input scope, no size metrics, no dedup-by-source-set. |
| 03 | [Webhook tester + delivery replay UI](prd-03-webhook-tester-replay.md) | Self-serve test-fire, delivery inspection, replay | **~90%** | No `X-Orpheus-Test` marker header, no response-body secret scrubbing, no separate HEAD ping. |
| 04 | [Language auto-detect, translation & summarization](prd-04-translate-summarize.md) | Detect/translate/summarize as first-class processors | **~85%** | Missing per-org `allow_external_llm` governance flag (PRD calls it a hard requirement); no long-transcript chunking. |
| 05 | [Diarization, word timestamps & subtitle export](prd-05-diarization-subtitles.md) | Speaker labels, word timestamps, SRT/VTT | **~90%** | No inline `diarize=true` param on transcribe (only via chaining); no duration-based DoS cap. |
| 06 | [Batch/callback API + presigned push to tenant S3](prd-06-batch-callback-tenant-s3.md) | Async batches, completion callbacks, push to tenant's own S3 | **~85%** | No retry/backoff on push failure; only `result.json` pushed, not artifacts; no per-org concurrent-batch cap. |
| 07 | [Per-tenant usage analytics + budget alerts](prd-07-usage-analytics-budgets.md) | Time-series usage, cost breakdowns, budgets, alerts | **~80%** | No email delivery of budget alerts (webhook only); no `api_key` dimension on timeseries/budget scope. |
| 08 | [PII redaction in transcripts and logs](prd-08-pii-redaction.md) | Detect and redact PII in output and operational logs | **~70%** | Go API has **no log redaction at all** (Python worker only), despite the PRD calling this table-stakes; no CI lint; un-redact mapping not KMS-encrypted. |
| 09 | [Resumable uploads + URL ingest](prd-09-resumable-url-ingest.md) | Resume interrupted uploads; ingest audio by URL | **~90%** | No per-org concurrent-fetch cap; http-allow flag is global env, not per-org/plan. |
| 10 | [GDPR erasure endpoint](prd-10-gdpr-erasure.md) | Tenant-initiated hard delete with verifiable S3 purge | **~75%** | `scope=subject` erasure unimplemented (the PRD's flagship example); advertised certificate download URL 404s; no MFA gate; no in-flight job cancellation. |
| 11 | [ASR quality completion](prd-11-asr-quality.md) | Forced alignment (WhisperX), ITN, VAD-segmented chunking, code-switching | **~80%** | All 4 headline features work; gap is entirely production-hardening (dropped `gpu_seconds`/`model_version_id` on align path, no retry/timeout config, zero metrics). |
| 12 | [Realtime streaming enhancements](prd-12-realtime-streaming.md) | Sub-300ms partials, semantic turn detection, in-stream diarization, eager endpoint, interim confidence, realtime PII | **~75%** | All 6 headline features work and are tested; gap is hardening — no metrics, no session/GPU caps, two protocol-shape deviations. |
| 13 | [Audio intelligence completion](prd-13-audio-intelligence-plus.md) | PII upgrade, emotion, audio-event detection, moderation, auto-chapters, multichannel, speaker enrollment | **~90%** | All 7 capabilities implemented and tested; `gpu_seconds` dropped on SenseVoice Modal path breaks cost metering; chapters/moderate wrongly marked non-cacheable. |
| 14 | [Audio enhancement (Krisp-class)](prd-14-audio-enhancement.md) | Noise suppression, background-voice cancellation, echo/de-reverb, isolation, accent conversion, telephony denoise | **~55%** | Denoise (M1) solid; accent-convert and echo/dereverb are documented non-implementations even in "production" code; no Modal-failure fallback; realtime mode is 0%. |
| 15 | [Voice-agent / conversational infrastructure](prd-15-voice-agent.md) | Barge-in, backchannel, active-listening, voicemail detection, telephony ingestion, full-duplex S2S | **~85%** | Learned addressee/backchannel classifier (M3) not started (heuristic-only); native WebRTC/SIP-RTP ingress never built (Twilio bridge substitutes). |
| 16 | [Meeting & media intelligence](prd-16-meeting-intelligence.md) | Meeting bot, live notes/action items, cross-meeting search, ask-AI, highlight reels, CRM, audio-edit-by-text, overdub | **~30-35%** | Headline feature — meeting-bot ingestion — is **0%**, along with the entire meetings/notes/comments data model and Go API. Processor-level substance exists (transcript search/ask, highlights, dub) but with no delivery/control-plane layer. |
| 17 | [Dictation "Flow" layer](prd-17-dictation-flow.md) | LLM cleanup pass, backtrack, command/transform, context-conditioned, tone presets | **~0%** | Nothing built — no `text.cleanup`, no `text.command`, no streaming flow hook, no tests. Genuinely unstarted. |
| 18 | [Developer surface](prd-18-developer-surface.md) | OpenAI/Deepgram-compatible endpoint, client SDKs, MCP server, processor SDK | **~12%** | Compat endpoints: 0%. MCP server: 0%. Processor SDK: 0%. Hand-written SDKs exist but unpublished/ungenerated. |
| 19 | [Cost/billing completion](prd-19-cost-billing.md) | LLM token-cost pass-through, real cache-savings basis, invoice line items, cost dashboards | **~5%** | All 4 proposed changes have zero code/schema footprint beyond pre-existing baseline infra. |
| 20 | [Infra scaling](prd-20-infra-scaling.md) | Inference batching, autoscaling, dynamic concurrency, model-registry wiring, multi-model routing | **~5%** | All 5 sub-features unbuilt. Model registry module is fully built + tested but genuinely unwired to any caller. |
| 21 | [Security & compliance hardening](prd-21-security-compliance.md) | Keycloak/OIDC, real IdP, rate-limiter policy, SOC2/HIPAA/SSO/SCIM, secrets mgmt | **~10%** | Mostly unbuilt (no org_id claim mapper, no real IdP for dashboard, no SCIM/SAML, WS `CheckOrigin` still wide open, no secrets manager). One real win: rate-limiter fail-closed is forced in prod and tested. |
| 22 | [Data, storage & lifecycle](prd-22-data-lifecycle.md) | Transcript store/search, semantic KB, data residency, retention/TTL, streaming delivery | **~15%** | No canonical `transcripts` table/API, no pgvector, no data residency, no per-tenant retention policy. A job-level search/RAG pipeline exists but not as the spec'd REST surface. |
| 23 | [Observability & ops](prd-23-observability.md) | Per-model/GPU metrics, cost dashboards, SLA, autoscaling consumer, GPU soak | **~35%** | SLA/canary/burn-rate alerting is real and working (previously undersold). Model-labeled metrics, GPU exporter, cost endpoint, and most of autoscaling-per-spec are unbuilt. |
| 24 | [Model & deployment differentiators](prd-24-model-deployment.md) | On-device/edge artifacts, MMS (1000+ langs), speech-to-speech, human tier, BYO training, self-host | **~8-10%** | Essentially unbuilt — no model-selection routing, no edge artifacts, no MMS, no S2S translation, no human-review tier, no BYO models. |
| 25 | [Frontier/niche realtime capabilities](prd-25-frontier-niche.md) | Realtime voiceprint ID, audio-event, audio PII, emotion/acoustic-scene, AI QA scoring | **~30-35%** | Batch substrate for all 5 exists (reused from PRDs 13/16/18) — but the PRD's actual claim is *realtime* delivery over the live WS relay, and that live wiring is ~0%. |
| 26 | [Caption styling + burn-in; local-dev streaming](prd-26-captions-and-local-dev.md) | Styled/burned-in captions; streaming server in `make dev` | **~25-30%** | Caption burn-in: 0%. Local-dev streaming: 0% (this PRD's own "current state" claim that `make dev` starts the full stack is inaccurate — it only starts the Go API). |
| 27 | [Harness Voice Adapter](prd-27-harness-voice-adapter.md) *(personal-path — see scope note in file)* | External orchestrator: ASR turn-events in, agentic harness reasoning, sentence-pipelined TTS out | **0% (proposed)** | Not started. Implementation is a standalone repo (e.g. `alfred-voice-bridge`), not `orpheus` — only this spec is indexed here. |
| 28 | [Low-Latency Streaming Synthesis Service](prd-28-streaming-tts-service.md) *(personal-path — see scope note in file)* | Always-on TTS, chunked sentence-level output, real cancellation | **0% (proposed)** | Not started. Companion to #27, same standalone-repo scope. |

## Conventions used across PRDs

- **Endpoints** extend the existing `/v1` surface and reuse `Problem`, `Job`,
  `Artifact`, `WebhookEvent`, and idempotency semantics rather than inventing new ones.
- **Data-model changes** name concrete tables/columns and state RLS + partitioning
  expectations. Migrations are goose; JSONB only for heterogeneous payloads.
- **Security sections** are mandatory and always cover tenant isolation, abuse/DoS,
  and audit.
- **Sizing:** each PRD is intentionally reviewable in one sitting.
- **`prd-27`/`prd-28` are the exception** to the multi-tenant/RLS conventions above — they're
  personal-path infrastructure for the Claude Code harness, not Orpheus SaaS features. See the
  scope note at the top of each file.

---

## Coverage map — checklist → PRD

Numbers below are the **new** `prd-NN` numbers (old epic-PRD number + 10) against
[`../../FEATURES-AND-ISSUES.md`](../../FEATURES-AND-ISSUES.md).

**Part A (issues):** A1 → 11 · A2 → 12 (+ `make dev` in 26) · A3 → 13 · A4 → 19 · A5 → 20 · A6 → 23 · A7 → 21 · A8 → 22 · A9 → 18 · A10 → 23 · A11 → 16 / 14 / 17 / 26.

**Part B (features):**
- **P0** → 11 (#298 forced align, #299 ITN), 18 (#303 SDKs); all others shipped.
- **P1 Realtime** → 12 (#306–312).
- **P1 Audio intelligence** → 13 (#313 PII, #320 enrollment, #322 multichannel, #323 profanity), 11 (#321 code-switch).
- **P1 Platform/infra** → 20 (#325 batching, #326 autoscaling), 18 (#327 OpenAI-compat, #330 MCP), 21 (#328 HIPAA), 22 (#329 residency), 24 (#335 custom training).
- **P2 Differentiators** → 24 (#334 self-host/on-prem), 21 (#432 marketplace sandbox).
- **P2 Dictation flow** → 17 (#338–344, #376 prompt modes).
- **P2 Audio enhancement** → 14 (#345–350).
- **P2 Voice-agent** → 15 (#351–356).
- **P2 Meeting/media** → 16 (#357–367).
- **P2 Model/deployment** → 13 (#368 emotion, #369 audio-event), 24 (#370 s2s, #371 on-device, #372 MMS, #373 human tier, #374 forced-align-to-text, #375 ZDR).
- **P3 Frontier/niche** → 25 (#377–385; #381/#382/#385 flagged as app-UX, not platform scope).

Every checklist item resolves to at least one PRD above; the app-UX-only P3 rows
(Talon-style OS control, watched-folders) are called out as out-of-platform-scope
rather than built.
