# PRD 09 — Multipart resumable uploads + URL-ingest (fetch audio from a URL)

**Status:** Draft · **Owner:** Uploads · **Reviewers:** Platform, Security
**Related:** PRODUCTION_DESIGN §6.1, ADR-006 (presigned multipart), §7.1 lifecycle ·
existing `POST /v1/uploads`, `POST /v1/uploads/{id}/complete`, `GET /v1/uploads/{id}` (1 GB cap)

## 1. Problem & motivation

Uploads already use presigned S3 multipart (bytes bypass the API tier), and `Part`/`CompletedPart`
are in the schema. But two real gaps hurt reliability and DX: (1) if a large upload is interrupted,
there's no first-class way to see which parts landed and **resume** — clients re-upload from
scratch; and (2) many customers already host audio somewhere (their CMS, a signed URL, a public
podcast feed) and want Orpheus to **fetch it** rather than round-trip bytes through their own
machine. Both reduce failed uploads and time-to-first-job.

## 2. Goals / non-goals

**Goals**
- **Resumable multipart:** query which parts are uploaded, get fresh presigned URLs for the
  missing parts, and complete — without restarting.
- **URL ingest:** `POST /v1/uploads/url` accepts a source URL; a worker fetches it into S3 as a
  normal artifact, with the same probe/validation as multipart uploads.
- Both paths converge on the existing `artifacts` model and downstream jobs.

**Non-goals**
- Client-side chunking logic (the SDK owns that; we expose the primitives).
- Ingesting from tenant private buckets in v1 (that's the inverse of PRD 06; revisit later).
- Torrent/streaming sources.

## 3. User stories

- As an SDK user on a flaky connection, I want to resume a 900 MB upload after a drop instead of
  starting over.
- As a podcast tool, I want to submit `https://cdn.example.com/ep42.mp3` and get an artifact_id back.
- As security, I want URL fetches to be SSRF-safe and size/type-validated.

## 4. Proposed API / UX

Resumable (extend existing upload session):

```jsonc
GET  /v1/uploads/{id}/parts
// → { uploaded: [ {part_no, etag, size} ], missing: [4,5,6], part_size_bytes }

POST /v1/uploads/{id}/parts:refresh
{ "part_numbers": [4,5,6] }
// → { parts: [ {part_no, url, expires_at} ] }   // fresh presigned PUT URLs
// then existing POST /v1/uploads/{id}/complete with the full CompletedPart list
```

URL ingest:

```jsonc
POST /v1/uploads/url             (Idempotency-Key honored → 202 + poll_url)
{ "url": "https://cdn.example.com/ep42.mp3",
  "filename": "ep42.mp3", "content_type": "audio/mpeg",
  "expected_sha256": "..." }     // optional integrity check
// → { upload_id, status: "fetching", poll_url: "/v1/uploads/{id}" }
GET /v1/uploads/{id}             // status: fetching → ready | failed; on ready → artifact_id
```

On completion, emit existing `upload.completed` (URL-ingest sets `source=url`).

## 5. Data-model changes

Mostly additive to existing upload tables (org-scoped, RLS):

- `upload_sessions`: add `source text default 'multipart'` (`multipart` | `url`),
  `source_url text`, `fetch_status text`, `fetch_error text`, `bytes_fetched bigint`.
- `upload_parts` already tracks parts; ensure `etag`/`size`/`uploaded_at` present so
  `GET .../parts` can report `uploaded`/`missing` (backed by S3 `ListParts` as source of truth).
- URL fetch is a new `orpheus.ingest.url` worker job → reuses job orchestration, retries, and the
  same probe path (`extract-metadata`/`probe`) that multipart completion triggers.

## 6. Edge cases & security

- **SSRF (critical):** reuse the webhook SSRF-safe fetcher — resolve DNS, **block private/link-local/
  metadata (169.254.169.254) ranges**, re-validate on redirect, cap redirects, allowlist schemes
  (`https` only; `http` behind a flag). Fetch runs in a sandboxed worker with no cluster egress except
  via the egress proxy.
- **Size/type abuse:** enforce the 1 GB cap by honoring `Content-Length` *and* hard-stopping the
  stream at the cap; validate magic bytes/content-type before marking `ready` (closes the current
  async-validation gap); reject on mismatch.
- **Tenant isolation:** upload session + resulting artifact org-scoped/RLS; S3 key under tenant prefix.
- **Resumable auth:** `parts:refresh` only issues URLs for *missing* parts of an incomplete session
  owned by the org; presigned URLs are short-lived.
- **Integrity:** if `expected_sha256` set, verify post-fetch and fail on mismatch (prevents
  fetching a swapped/poisoned resource).
- **Abuse/DoS:** URL ingest counts against rate limits + budgets; per-org concurrent-fetch cap;
  egress from our infra is metered.

## 7. Metrics / SLAs

- `upload_resume_rate` (resumed vs. restarted), `url_ingest_success_ratio > 99%`.
- `url_ingest_fetch_latency_p95` (scales with size), SSRF-block counter (should be low but non-zero).

## 8. Rollout plan

1. Ship `GET /v1/uploads/{id}/parts` + `:refresh` (resumable) — additive, low risk.
2. Ship URL ingest behind a flag, `https`-only, with SSRF guard + synchronous validation.
3. Add `expected_sha256` integrity + per-org concurrency caps.
4. Enable URL ingest by default per plan.

## 9. Open questions

- Do we cache/dedupe identical source URLs across a single org (tie to PRD 01)?
- Auth for protected source URLs — allow a caller-supplied `Authorization` header we forward? (Risky;
  proposed: no in v1, signed URLs only.)
- Should URL ingest and the fetched artifact share a single job, or ingest-then-separate-processing?
