# PRD 06 — Batch/callback API + presigned result push to a tenant's own S3

**Status:** Draft · **Owner:** Jobs + Notifications · **Reviewers:** Platform, Security
**Related:** PRODUCTION_DESIGN §8.3 (`POST /v1/jobs/bulk`), §8.6 (signed webhooks),
§10.6 (per-tenant secrets / BYO storage creds) · existing `BulkCreateJobsRequest`/`BulkJobsResponse`

## 1. Problem & motivation

`POST /v1/jobs/bulk` exists but is fire-and-forget: submit N jobs, get back accepted/rejected,
then poll each. High-volume tenants (transcribing thousands of files nightly) want (a) a **batch**
they can track as one unit with an aggregate completion signal, and (b) results delivered to
**their own S3 bucket** instead of pulling each artifact via signed URL. This is a standard
enterprise integration pattern (mirrors AssemblyAI/Deepgram batch + delivery). The design doc
already anticipates BYO storage and per-tenant creds.

## 2. Goals / non-goals

**Goals**
- First-class **batch** object: submit many jobs, track aggregate progress, get one
  `batch.completed` callback when all terminal.
- **Result push** to a tenant-configured S3 destination (their bucket) via a role they grant us,
  writing result JSON + output artifacts on each job completion.
- Reuse existing webhook signing for the batch-level callback.

**Non-goals**
- Non-S3 destinations (GCS/Azure/HTTP) in v1 (abstraction allows later).
- Streaming batch results; pushes are per-job on completion.
- Replacing per-job webhooks (batches complement them).

## 3. User stories

- As a data platform, I want to submit 5,000 transcribe jobs as one batch and get a single
  callback when they're all done, with a manifest.
- As a tenant, I want each transcript written directly to `s3://my-bucket/orpheus/{job_id}.json`
  so my downstream pipeline reads from my own storage.
- As security, I want Orpheus to write to my bucket using a scoped role I control and can revoke.

## 4. Proposed API / UX

```jsonc
// Create a tracked batch (extends the bulk concept)
POST /v1/batches            (Idempotency-Key honored → 202 + poll_url)
{ "name": "nightly-2026-07-15",
  "jobs": [ { "artifact_id": "...", "processor": {...}, "params": {...} }, ... ],
  "callback": { "webhook_id": "wh_..." },           // signed batch.completed
  "delivery": { "destination_id": "dst_..." } }     // optional result push target

GET /v1/batches/{id}        // status, counts {queued,running,completed,failed}, manifest_url
GET /v1/batches/{id}/jobs   // paginated child jobs

// Tenant S3 destination config
POST   /v1/destinations
{ "type": "s3", "bucket": "my-bucket", "prefix": "orpheus/",
  "region": "us-east-1", "role_arn": "arn:aws:iam::TENANT:role/OrpheusWriter",
  "external_id": "auto-generated" }
GET    /v1/destinations        // list
POST   /v1/destinations/{id}/verify   // dry-run: assume role, write+delete a probe object
DELETE /v1/destinations/{id}
```

On each child completion, if a destination is set, the worker writes `result.json` + artifacts
under `{prefix}{job_id}/`. On all-terminal, emit signed `batch.completed` with a manifest
(S3 keys or Orpheus artifact IDs, per-job status).

## 5. Data-model changes

New org-scoped, RLS-covered tables (goose):

```
batches( id, org_id, name, status, job_count, completed_count, failed_count,
         callback_webhook_id, destination_id, manifest_s3_key, created_at, updated_at )
-- child jobs reference batch via jobs.batch_id (nullable FK, added column)

delivery_destinations( id, org_id, type, bucket, prefix, region,
         role_arn, external_id, verified_at, last_error, created_at )
```

- **Cross-account creds are not stored as long-lived keys.** We store `role_arn` + a generated
  `external_id`; Orpheus assumes the role via STS at push time (per §10.6). No tenant secret keys
  in our DB.
- `batches` and child-job aggregation update via the existing outbox/event path on each terminal job.

## 6. Edge cases & security

- **Tenant isolation:** batch + destination are org-scoped/RLS; a batch may only reference the
  org's artifacts and its own destination.
- **Cross-account write security:** require `sts:AssumeRole` with a unique per-destination
  `external_id` (confused-deputy defense); scope the trust policy to Orpheus's account only;
  `verify` proves write+delete before first real push.
- **SSRF/exfil:** destination is S3-only, role-based; we never take arbitrary URLs. Bucket writes
  are prefixed and cannot escape `{prefix}`.
- **Partial failure:** a failed push does not fail the job; it retries with backoff and, on
  exhaustion, marks the child `delivery_failed` and includes it in the manifest (never silently drops).
- **Abuse/DoS:** batch size and per-org concurrent batches are capped; child jobs share the org's
  concurrency + rate limits.
- **Audit:** destination create/verify/delete and role assumption are audited.

## 7. Metrics / SLAs

- `batch_completion_latency`, `result_push_success_ratio > 99%`, `push_retry_count`.
- `batch.completed` callback fired within `< 60s` of the last child reaching terminal.

## 8. Rollout plan

1. Ship `batches` + `jobs.batch_id` + aggregate tracking + `batch.completed` webhook.
2. Ship `delivery_destinations` + STS assume-role verify (no push yet).
3. Enable result push on completion; start with result JSON, then artifacts.
4. Add manifest generation + per-job `delivery_failed` handling.

## 9. Open questions

- Do we push output *artifacts* (could be large) or only result JSON + signed URLs by default?
- KMS: write to tenant bucket with tenant-managed KMS key (they grant `kms:GenerateDataKey`)?
- Backpressure if a tenant bucket is misconfigured mid-batch — pause vs. keep-retrying policy.
