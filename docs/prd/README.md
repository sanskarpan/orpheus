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
workflow** (not Temporal), workers are **CPU-only** today, and the DLQ is currently a
**status (`exhausted`)** rather than a dedicated table/UI.

## Index

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
- **Sizing:** each PRD is intentionally ~1–2 pages and reviewable in one sitting.
