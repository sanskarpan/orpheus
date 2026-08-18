# PRD: Data, Storage & Lifecycle

**Status:** Proposed · **Priority:** P1 · **Epic:** Data Platform · **Related issues:** #433 #434 #435

## 1. Summary

Give Orpheus a first-class **data layer** for the transcripts it produces: a durable, org-scoped **transcript store with a search/retrieval surface** (#433); **semantic search + a knowledge base** over that store via embeddings (#434); **data-residency / region selection** so a tenant's audio, transcripts, and derived data stay in a chosen region (#435); **retention policies with per-tenant TTLs** that a sweeper enforces; and **streaming / transcoded artifact delivery** to complement today's signed-URL-only download.

Today a transcript exists only inside `jobs.result` JSONB and is reachable only by knowing the job id. This PRD makes transcripts a queryable corpus (the substrate `prd-06` chat/search and `prd-14` privacy modes build on), while staying inside the existing FORCE-RLS multi-tenant model, the outbox/metering rails, and the goose migration flow.

## 2. Motivation & goals

- **Retrieval.** Customers cannot list or search their transcripts — only re-fetch a job by id. A transcript store + search unlocks meeting intelligence, analytics, and BYO-app use cases.
- **Meaning, not just keywords.** #434 wants "find where we discussed pricing objections" across thousands of transcripts — needs embeddings.
- **Compliance & sales.** Enterprise buyers require data residency (#435) and defensible retention/deletion. We already have GDPR erasure (`handlers/erasure.go`) and an abandoned-state sweeper (`internal/retention/sweeper.go`); retention TTLs generalize that.
- **Delivery.** Large media served only as a signed GET URL forces full downloads; streaming/transcoded delivery (range requests, HLS) improves UX and cost.

**Goals**
- `transcripts` table + `GET /v1/transcripts`, `GET /v1/transcripts/{id}`, `POST /v1/transcripts/search`.
- Semantic index (Postgres `pgvector` first) with hybrid keyword+vector ranking.
- Per-tenant retention policies + a retention sweeper extension that expires transcripts/artifacts on TTL, honoring erasure/legal-hold.
- Region routing for storage + compute consistent with RLS.
- Streaming artifact delivery (HTTP range + optional HLS) alongside signed URLs.

**Non-goals**
- The chat/RAG product surface (that's `prd-06` `chat.answer`); this PRD provides the retrieval primitive it calls.
- A brand-new datastore migration off Postgres — we extend the current stack.

## 3. Current state in Orpheus (file:line, patterns to build on)

- **Transcripts are only in `jobs.result`.** `handlers/jobs.go:76` `Result json.RawMessage`; produced by `processors/transcribe.py:22`. No list/search endpoint, no dedicated table.
- **RLS multi-tenancy.** `internal/db/db.go:81` `set_config('app.current_org_id', $1, true)` per tx; every table is FORCE RLS. New tables must follow suit (see `README.md` "System context").
- **Migrations.** goose, `apps/api/internal/db/migrations/`, latest `0020_marketplace.sql` → new files `0021+`.
- **Retention sweeper exists.** `internal/retention/sweeper.go` — service-role background loop, `FOR UPDATE SKIP LOCKED`, currently expires abandoned upload sessions + idempotency keys. Aborts S3 multipart (`MultipartAborter`). This is the exact skeleton for TTL retention.
- **Erasure.** `handlers/erasure.go` + migration `0016_erasure.sql` — tombstone/erase flow; retention must not delete anything under legal hold and must coordinate with erasure.
- **Artifact delivery is signed-URL only.** `handlers/artifacts.go:100` `GetSignedURL` → `S3.Presigner().PresignGetObject` (`artifacts.go:133`); no range/streaming/transcode path. Tenant-S3 push exists (`internal/delivery/delivery.go`, `s3_static`/`s3_sts`).
- **Storage.** `internal/storage/s3`; per-tenant key prefixes, SSE-KMS. Bytes never transit the API tier.
- **Content cache** proves stable canonicalization + org-scoped keys (`handlers/cache.go:31`) — reused conceptually for transcript dedup/versioning.

## 4. Proposed design

### 4.1 Transcript store (#433) — migration `0021_transcripts.sql`
```sql
CREATE TABLE transcripts (
  id            uuid PRIMARY KEY,           -- uuid7
  org_id        uuid NOT NULL,
  source_job_id uuid NOT NULL,              -- the transcribe/diarize job
  artifact_id   uuid,                       -- source media
  meeting_id    uuid,                       -- nullable (prd-06)
  language      text,
  duration_seconds double precision,
  segments      jsonb NOT NULL,             -- [{start,end,text,speaker,words[]}]
  text          text NOT NULL,              -- flattened, for FTS
  fts           tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED,
  region        text NOT NULL DEFAULT 'default',
  created_at    timestamptz NOT NULL DEFAULT now(),
  retain_until  timestamptz                 -- set from policy; NULL = keep
);
-- FORCE RLS + policy USING (org_id = current_setting('app.current_org_id')::uuid)
CREATE INDEX ON transcripts USING GIN (fts);
```
A small **worker hook**: the transcribe/diarize/meeting workflow, on completion, upserts a `transcripts` row from `jobs.result` (idempotent on `source_job_id`). Reuses the existing worker DB (service role) write pattern (`model_registry.py:65`).

**Failure modes & durability.** The upsert is idempotent on `source_job_id`, so a retried or duplicated job produces exactly one row; a transient DB error retries with backoff and, on exhaustion, is captured to the outbox so the transcript is never silently lost even though the job itself completed. A malformed `jobs.result` (missing segments) fails the hook loudly rather than writing a half-row. **Multi-tenant security:** every write carries the owning `org_id` and lands under the FORCE-RLS policy (`db.go:81`); the service-role writer sets `app.current_org_id` per tx so a worker cannot cross-write another tenant. **Scale:** `segments`/`text` can be large — bound row size and, above a threshold, keep the full segment payload in `jobs.result`/S3 and store a truncated `text` for FTS with a pointer, so the table stays index-friendly. **Backward-compatible on-wire:** existing `GET /v1/jobs/{id}` result shape is unchanged; the transcript store is purely additive. **Observability:** emit `orpheus_transcripts_upserted_total{status}` and hook latency.

**API** (`handlers/transcripts.go`, wired in `server.go` `v1Routes`, scope `transcripts:read`/`write`):
- `GET /v1/transcripts?language=&meeting_id=&created_after=` — paginated list.
- `GET /v1/transcripts/{id}` — full record; `?format=segments|text|srt|vtt` reuses `export.subtitles` builders.
- `POST /v1/transcripts/search` — hybrid search (below).

### 4.2 Semantic search / knowledge base (#434)
- **Embedding column via pgvector.** Add `pgvector` extension; chunk segments (~30–60s windows) into `transcript_chunks(id, transcript_id, org_id, start, end, text, embedding vector(N))` with an IVFFlat/HNSW index. Chunking + embedding is a new worker processor `index.embed` (`register_processor`), run after a transcript lands. Embeddings via the provider-agnostic layer — add an `embed()` task to `llm.py` (mirrors `summarize`/`translate`), default served by a Modal embedding service (`infra/modal/orpheus_embed.py`, same shared-secret/scale-to-zero pattern as `orpheus_llm.py`).
- **Hybrid ranking.** `POST /v1/transcripts/search {query, k, scope}` runs FTS (`fts @@ plainto_tsquery`) **and** vector `<->` nearest-neighbor, fuses with reciprocal-rank fusion, returns chunks with `{transcript_id, meeting_id, start, end, snippet, score}`. This is the retrieval primitive `prd-06`'s `chat.answer` calls.
- **Decision — Postgres+pgvector.** Keeps RLS, backups, region routing, and erasure in one system (an external vector DB would need its own tenant isolation + residency story). Retrieval sits behind a Go `Retriever` interface so a managed vector DB can be swapped in later if scale demands (>tens of millions of chunks); this is a production-grade extension seam, not a deferred phase.
- **Failure modes & degradation.** If the embedding service (Modal `orpheus_embed.py`) is down or a chunk fails to embed, the `index.embed` processor retries with backoff and leaves the transcript **keyword-searchable** in the meantime — search degrades to FTS-only rather than returning nothing, and a backfill job re-embeds the gaps. Search with `mode=hybrid` transparently falls back to keyword ranking for any transcript whose embeddings are not yet present.
- **Scale, concurrency & cost metering.** Embedding runs on the same shared-secret, scale-to-zero Modal pattern as `orpheus_llm.py` with `max_containers`/`@modal.concurrent` bounding GPU fan-in and standing cost; embeddings are **cost-metered per token/GPU-second** exactly like `summarize`/`translate`, attributed to the owning org. The IVFFlat/HNSW index is built with bounded memory; chunking is batched to keep embedding calls efficient.
- **Multi-tenant security & isolation.** `transcript_chunks` carries `org_id` under FORCE-RLS, and every vector query runs inside the tenant context, so ANN search can never surface another tenant's chunk. Re-embedding on model change is pinned to a `model_version_id` so results are reproducible and auditable.
- **Backward-compatible on-wire.** `POST /v1/transcripts/search` accepts `mode` with `hybrid` as default; existing keyword-only callers are unaffected, and the response shape is additive.

### 4.3 Data residency / region selection (#435)
- **Region as a first-class tenant attribute.** Add `orgs.region` (or a `tenant_regions` map) with allowed values (e.g. `us`, `eu`, `ap`). Every region-bound row (`transcripts`, `transcript_chunks`, artifacts) carries `region`.
- **Storage routing.** S3 bucket/prefix selection keys off the tenant region (extend `internal/storage/s3` to hold a region→bucket map). Presign and delivery use the region's bucket.
- **Compute routing.** Modal services deployed per region (or region-pinned functions); the worker picks the endpoint by the job's tenant region. LLM/embed calls route to the in-region Modal deployment.
- **Enforcement.** A cross-region read is denied at the handler (region check) in addition to RLS; region is immutable per tenant after provisioning (a residency guarantee), changeable only via an admin migration job that physically moves data.
- **Failure modes & GDPR.** If the in-region Modal endpoint or bucket is unavailable, the job fails and retries **in-region** — it must never spill to another region as a fallback (residency over availability); this is the technical control behind an EU-only data-residency commitment. Region routing is resolved from the tenant record under RLS, so it cannot be client-overridden. **Observability:** emit `orpheus_region_routed_total{region}` and `orpheus_cross_region_denied_total` so any residency violation attempt is visible and alertable.

### 4.4 Retention policies / per-tenant TTLs
- **Policy table** `retention_policies(org_id, subject (transcript|artifact|job_result|meeting), ttl_days, legal_hold bool)`. On transcript/artifact creation, `retain_until = now() + ttl`.
- **Sweeper extension.** Generalize `internal/retention/sweeper.go`: add a pass that claims rows with `retain_until < now()` (FOR UPDATE SKIP LOCKED), deletes/tombstones them, and aborts/deletes the backing S3 objects (reuse the `MultipartAborter`-style abstraction, extended to `DeleteObject`). Skips anything under `legal_hold` or with an open erasure/erasure-hold (`handlers/erasure.go`). Emits an outbox `retention.expired` event.
- Service-role, region-aware (sweep each region's store), idempotent, batched — matching the current sweeper's concurrency-safe design.
- **Failure modes & safety.** Deletion is two-phase: the DB row is tombstoned first, then the backing S3 object is deleted; an S3 delete failure leaves the tombstone and re-queues the object delete so nothing is orphaned and no live pointer survives. A single object's delete failure never blocks the batch (`SKIP LOCKED` + per-row error capture). The sweep is bounded per interval (backpressure — it never tries to delete the entire backlog in one pass) so a large expiry wave cannot overwhelm S3 or the DB. **Legal-hold/erasure interplay** is single-sourced: retention re-checks `legal_hold` and open erasure holds inside the claiming tx, so it can never race GDPR erasure or delete a held record. **Cost:** expiring data reclaims storage spend; emit `orpheus_retention_expired_total{subject}` and `orpheus_retention_sweep_errors_total`.

Search request/response:
```jsonc
POST /v1/transcripts/search
{ "query": "pricing objection", "k": 20,
  "mode": "hybrid",                 // keyword | semantic | hybrid (default)
  "scope": { "meeting_ids": [...] } | { "all": true },
  "filters": { "language": "en", "created_after": "2026-01-01T00:00:00Z" } }
// → { "results": [
//      { "transcript_id": "tr_018f...", "meeting_id": "mtg_018f...",
//        "start": 1287.4, "end": 1319.0, "snippet": "...they pushed back on price...",
//        "score": 0.83 } ], "next_cursor": null }
```

### 4.5 Streaming / transcoded artifact delivery
- **Range delivery.** New `GET /v1/artifacts/{id}/stream` that returns a signed URL supporting HTTP `Range` (S3 already supports range GET), or proxies range requests for players that can't hit S3 directly. Complements `GetSignedURL` (`artifacts.go:100`) rather than replacing it.
- **Transcoded/HLS.** Optional `transcode` processor (ffmpeg, reusing the `ffmpeg` helpers used by transcribe) produces HLS/mp4 renditions stored as derived artifacts; `stream` endpoint serves the playlist. Gated by tenant opt-in (storage cost).
- **Failure modes, scale & security.** The range/stream path issues a **short-TTL signed URL scoped to the requesting org's key prefix** so streaming inherits the same tenant isolation as `GetSignedURL` (`artifacts.go:100`); bytes still never transit the API tier. A missing/expired artifact returns a clean 404/410 rather than a broken stream. Transcoding is a bounded, cost-metered worker job (GPU/CPU-seconds attributed to the org) with `max_containers` limiting concurrency; a transcode failure leaves the original signed-URL download intact (graceful degradation — streaming is additive, never removing the existing path). Renditions carry the tenant `region` (§4.3). **Observability:** emit `orpheus_stream_requests_total{outcome}` and transcode job metrics.

### 4.6 User stories
- As a BYO-app developer, I want `GET /v1/transcripts?created_after=...` so I can sync new transcripts into my own product without tracking job ids (#433).
- As an analyst, I want semantic search over 10k calls to find every mention of a competitor by meaning, not exact words (#434).
- As an EU enterprise buyer, I want a guarantee my audio and transcripts never leave `eu-central` (#435).
- As a compliance officer, I want transcripts auto-deleted after 90 days unless under legal hold.

## 5. Rollout / milestones

Ordering only — each milestone is production-quality and shippable on its own (full RLS, failure
handling, metering, observability), not a reduced-scope prototype. The complete §4 scope is in
scope.

- **M1 — Transcript store & retrieval.** `transcripts` table (FORCE-RLS) + idempotent worker
  upsert hook (retry + outbox-on-failure) + `GET /v1/transcripts` and `{id}` + keyword (FTS)
  search. Immediate value, no new infra; ships with upsert/RLS metrics.
- **M2 — Semantic search & knowledge base.** pgvector + `index.embed` processor + `embed()` in
  `llm.py` + Modal `orpheus_embed.py` + hybrid `POST /v1/transcripts/search`, with FTS-fallback
  degradation, per-token cost metering, `model_version_id`-pinned re-index, and the `Retriever`
  seam.
- **M3 — Retention & lifecycle.** Per-tenant retention policies + sweeper extension with two-phase
  tombstone→delete, single-sourced legal-hold/erasure coordination, bounded per-interval sweep,
  and expiry metrics.
- **M4 — Residency & delivery.** Data residency (region column + in-region storage/compute routing,
  no cross-region spill) and streaming/transcoded delivery (range + optional HLS) with org-scoped
  signed URLs and graceful fallback to signed-URL download.

## 6. Verification / acceptance criteria

End-to-end against a real API + worker + pgvector + Modal `orpheus_embed`/S3 (not unit-only). Each
item covers a positive path, a negative/failure path, and multi-tenant isolation where relevant.

- **Transcript store (idempotency + isolation + durability):** every completed transcribe/diarize
  job produces exactly one `transcripts` row (idempotent on `source_job_id` under re-run); org A
  cannot list/read org B's transcripts (RLS test à la `internal/db/db_rls_test.go`); a simulated
  DB error during the hook lands the transcript via outbox rather than losing it.
- **Search (quality + degradation):** keyword search returns expected hits; hybrid search
  out-ranks pure FTS on a paraphrase query set (semantic recall measurably higher); with the
  embedding service forced down, `mode=hybrid` still returns FTS results and a later backfill
  re-embeds the gap. Vector queries never cross tenants.
- **Retention (deletion + safety):** a transcript past `retain_until` (no legal hold) is deleted
  from Postgres **and** S3 within one sweep interval; a legal-held or erasure-pending row is never
  touched even when expired; an injected S3-delete failure leaves the tombstone and re-queues the
  object delete (no orphan, no live pointer); `orpheus_retention_expired_total` reflects the sweep.
- **Residency (guarantee + no-spill):** a tenant pinned to `eu` never has bytes written to a
  non-EU bucket; a cross-region read is rejected before the query runs
  (`orpheus_cross_region_denied_total` increments); with the in-region endpoint down the job
  retries in-region and never spills cross-region.
- **Delivery (range + fallback + isolation):** `GET /v1/artifacts/{id}/stream` serves partial
  content for a `Range` request via an org-scoped signed URL; an HLS rendition plays in a standard
  player; a failed transcode still leaves the original signed-URL download working; a missing
  artifact returns 404/410 cleanly.
- **Cost metering:** embedding and transcode jobs record per-token/GPU-second cost attributed to
  the owning org, consistent with the existing metering rails.
- **Swappability:** retrieval sits behind the `Retriever` interface so the vector backend is
  replaceable without handler changes (asserted by a stub backend in tests).

## 7. Dependencies, risks, open questions

- **pgvector at scale.** IVFFlat/HNSW recall vs latency on tens of millions of chunks; the `Retriever` seam is the mitigation.
- **Embedding cost/consistency.** Re-embedding on model change needs a `model_version_id`-pinned re-index job (reuse reproducibility pinning). Which embedding model/dim? (open)
- **Residency is expensive.** Per-region Modal + S3 + Postgres (or partitioning) multiplies infra; sequence behind clear enterprise demand.
- **Erasure/retention interplay.** Must be single-sourced so retention never races GDPR erasure or legal hold.
- **Open:** one Postgres per region vs region-partitioned single cluster? Do we transcode eagerly or on first stream?

## 8. Effort

Each milestone ships production-grade (RLS, failure handling, metering, observability), not a
prototype.

- M1 transcript store + FTS (+ idempotent/outbox hook + metrics): ~2.5 wk.
- M2 pgvector + embed processor + hybrid search (+ FTS-fallback + metering + re-index): ~4 wk.
- M3 retention policies + sweeper (+ two-phase delete + hold coordination): ~2 wk (skeleton exists).
- M4 residency + streaming delivery (+ no-spill enforcement + org-scoped signed URLs): ~4–6 wk
  (residency is the long pole).

Order-of-magnitude: **~2.5–3.5 months** total; M1+M2 (the retrieval substrate `prd-06`/`prd-14`
need) is ~6–7 weeks.
