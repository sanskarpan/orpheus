# PRD 02 — Signed, expiring artifact download bundles / zip export

**Status:** Draft · **Owner:** Artifacts · **Reviewers:** Platform, Security
**Related:** PRODUCTION_DESIGN §7.1, §8.3 (`/v1/artifacts/{id}/signed-url`), §10.5 (S3 lifecycle)

## 1. Problem & motivation

Today a client can request a single short-lived signed GET URL per artifact
(`GET /v1/artifacts/{id}/signed-url`). Real workflows produce **many** outputs — a
transcribe-long job yields `transcript.json`, `transcript.srt`, per-chunk artifacts; a
demucs job yields several stems. Consumers currently must enumerate jobs, collect artifact
IDs, and issue N signed-URL calls, then zip client-side. This is slow, error-prone, and
leaks the internal artifact graph. Tenants want "give me one link to download everything for
this job / these jobs," and the link must be signed and time-boxed like every other download.

## 2. Goals / non-goals

**Goals**
- Create a **bundle** from a set of artifacts (or all outputs of one/more jobs) and get a
  single signed, expiring download URL for a `.zip`.
- Bundles are async (zipping is a job) with webhook/poll completion, since bundles can be large.
- Downloads never transit the API tier — the signed URL points at S3.

**Non-goals**
- Streaming/partial zip download or resumable bundle downloads (client uses HTTP range on S3).
- Tar/other formats in v1 (zip only).
- Bundling raw *input* uploads by default (opt-in, permission-gated).

## 3. User stories

- As a data engineer, I want one link to download all outputs of a job so my pipeline fetches once.
- As a support user, I want to export the last 50 transcripts as a zip for an offline audit.
- As a security reviewer, I want bundle links to expire and be revocable.

## 4. Proposed API / UX

```jsonc
// POST /v1/bundles           (Idempotency-Key honored; returns 202 + poll_url)
{ "name": "job-123-outputs",
  "sources": [ { "job_id": "018f..." }, { "artifact_id": "018e..." } ],
  "include_result_json": true,      // also embed jobs[].result as result.json
  "ttl_seconds": 3600 }             // clamped to plan max (e.g. 24h)

// 202
{ "id": "bnd_018f...", "status": "building", "poll_url": "/v1/bundles/bnd_018f..." }

GET    /v1/bundles/{id}       // status: building|ready|failed|expired; size_bytes, artifact_count
GET    /v1/bundles/{id}/download   // 302 → signed S3 URL (or 409 if not ready)
DELETE /v1/bundles/{id}       // revoke: delete zip from S3, mark revoked
GET    /v1/bundles           // list (org-scoped)
```

On completion, emit `bundle.ready` / `bundle.failed` webhook events (new `WebhookEvent`
enum members) carrying `download_url` and `expires_at`.

## 5. Data-model changes

New org-scoped, RLS-covered tables (goose):

```
bundles(
  id uuid pk, org_id uuid not null, name text, status text,   -- building|ready|failed|expired|revoked
  s3_key text, size_bytes bigint, artifact_count int,
  include_result_json bool, expires_at timestamptz,
  created_by uuid, created_at, updated_at )

bundle_items(
  bundle_id uuid, artifact_id uuid, path_in_zip text,
  primary key (bundle_id, artifact_id) )
```

- Bundle zips are written to `s3://orpheus-{tenant}-{env}/bundles/{id}.zip` (tenant-scoped prefix,
  SSE-KMS), lifecycle-expired shortly after `expires_at` (belt-and-suspenders with app expiry).
- Bundling is a new `orpheus.export.bundle` processor/job so it reuses job orchestration, retries,
  cost attribution, and observability.

## 6. Edge cases & security

- **Tenant isolation:** every source `job_id`/`artifact_id` is resolved under RLS; a bundle can only
  contain artifacts the requesting org owns. Cross-org refs → `403`/`404` (indistinguishable).
- **Signed & expiring:** download is a presigned S3 GET with `ttl_seconds` (clamped); `DELETE`
  hard-deletes the zip so a leaked-but-revoked link 404s at S3.
- **Zip-bomb / size abuse:** enforce max total uncompressed size and max item count per plan;
  reject at build time with Problem `type=bundle-too-large`.
- **Path traversal:** `path_in_zip` is server-derived and sanitized; clients cannot set arbitrary paths.
- **Include-inputs:** requires `artifact:read` on inputs and a distinct scope; audited.
- **Retention/erasure:** bundle holds artifact references; PRD 10 erasure must also purge bundle zips
  that contain erased artifacts.

## 7. Metrics / SLAs

- `bundle_build_seconds_p95` (target `< 60s` for ≤ 1 GB), `bundle_build_failure_ratio < 1%`.
- Track bundle egress bytes for billing (`usage_event kind=bundle_egress`).

## 8. Rollout plan

1. Ship `bundles` tables + `export.bundle` processor + create/status endpoints (build only).
2. Add `/download` (signed URL) + `DELETE` revoke + list.
3. Add webhook events + lifecycle S3 rule for auto-expiry.
4. Enforce plan-tier size/TTL caps; enable by default.

## 9. Open questions

- Deduplicate bundle zips via PRD 01 cache key over the sorted artifact set?
- Should `ttl_seconds` default differ by plan, and is 24h a hard ceiling for all tiers?
- Do we let bundles reference *another* bundle (nesting)? (Proposed: no.)
