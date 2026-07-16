# PRD 01 — Idempotent job dedup by content hash (content-addressed result cache)

**Status:** Draft · **Owner:** Jobs · **Reviewers:** Platform, Billing
**Related:** PRODUCTION_DESIGN §2.6(4), §7.2, §9.2 · builds on existing Idempotency-Key middleware

## 1. Problem & motivation

Orpheus already guarantees reproducibility: a job pins `(processor, version)` →
`model_version_id`, so the same input + params + model always yields the same result.
Today we still *recompute* that result every time. For batch re-processing, retried
pipelines, and duplicate uploads of the same file, tenants pay (in latency and GPU $)
for output they've already produced. The design doc calls out content-addressed dedup as
a 10–30% storage/compute saving. `Idempotency-Key` protects against duplicate *requests*
within a 24h window; this feature protects against duplicate *computation* forever, keyed
on content rather than a client-supplied token.

## 2. Goals / non-goals

**Goals**
- On job submit, if an identical `(input_hash, params_hash, model_version_id)` result exists
  and is still valid, return that result instead of enqueueing work.
- Cache is org-scoped by default; opt-in cross-org sharing is explicitly out of scope for v1.
- Deterministic cache key derivation, observable hit rate, and per-job `cache: hit|miss`.

**Non-goals**
- Cross-tenant/global dedup (privacy + billing complexity; revisit in v2).
- Deduping *non-deterministic* processors (e.g. `musicgen` with a random seed) — those are
  flagged `deterministic=false` and always miss.
- Partial/streaming result caching.

## 3. User stories

- As an API consumer re-running a pipeline, I want identical jobs to return instantly so I
  don't pay twice.
- As a tenant admin, I want to see my cache hit rate and estimated savings.
- As an operator, I want to purge cache entries for a specific `model_version_id` when a
  model is found defective.

## 4. Proposed API / UX

Reuse `POST /v1/jobs`. Add optional request field and response fields:

```jsonc
// POST /v1/jobs  (request, additive)
{ "artifact_id": "...", "processor": {...}, "params": {...},
  "cache": "auto" }          // auto (default) | bypass | only

// 202 / 200 response gains:
{ "id": "...", "status": "completed", "cache": "hit",
  "cached_from_job_id": "018f..." , "result": {...} }
```

- `cache=auto`: check cache; on hit return `200` with the prior result (new `job` row,
  `status=completed`, `cache=hit`, zero cost, points at `cached_from_job_id`).
- `cache=bypass`: force recompute, still *populate* the cache.
- `cache=only`: return `409` (Problem `type=cache-miss`) if not cached — useful for cheap probes.
- New: `DELETE /v1/cache?model_version_id=...` (admin scope) to invalidate entries.
- New: `GET /v1/cache/stats` → `{ hits, misses, hit_rate, est_savings_usd, period }`.

## 5. Data-model changes

New org-scoped, RLS-covered table (goose migration):

```
job_result_cache(
  id uuid pk default uuidv7,
  org_id uuid not null,
  cache_key bytea not null,             -- sha256(input_hash || params_hash || model_version_id)
  input_hash text not null,             -- artifacts.sha256
  params_hash text not null,            -- sha256 of canonicalized params JSON
  model_version_id text not null,
  source_job_id uuid not null,          -- job that produced the cached result
  result jsonb not null,
  output_artifact_ids uuid[] not null default '{}',
  hit_count int not null default 0,
  created_at timestamptz, last_hit_at timestamptz,
  expires_at timestamptz                -- null = follows artifact retention
)
unique (org_id, cache_key)
```

- `params_hash` = sha256 over params with **canonical JSON** (sorted keys, normalized numbers).
- Output artifacts are **reference-counted**, not copied; cache entry pins refs so GDPR/erasure
  and retention see the extra reference (see PRD 10).
- Populated by the worker on `persist_result` in the same tx as `job_results`.

## 6. Edge cases & security

- **Tenant isolation:** `cache_key` is namespaced by `org_id` and RLS-enforced; a hit can never
  cross orgs even on identical content.
- **Deletion coherence:** if a source artifact/result is erased, its cache entries are cascaded
  (via ref-count → 0) so a hit never resurrects deleted data.
- **Non-determinism:** processor manifest carries `deterministic` + `cacheable`; if false, always miss.
- **Poisoning:** cache is only written by trusted workers on successful jobs, never by clients.
- **Params drift:** canonicalization must be versioned (`params_hash_algo=v1`) so a hashing change
  doesn't silently reuse stale entries.
- **Billing:** cache hits record `cost_usd=0` but still emit a `usage_event` (`kind=cache_hit`) so
  savings are measurable and rate limits still apply (abuse guard).

## 7. Metrics / SLAs

- `job_cache_hit_ratio` (target: report, not gate), `job_cache_lookup_latency_p99 < 15ms`.
- Cache-hit job returns end-to-end `< 200ms p95` (DB round trip only).
- Dashboard: hit rate, est. GPU-seconds saved, est. USD saved per org.

## 8. Rollout plan

1. Ship table + write path (populate cache; no reads) — safe, no behavior change.
2. Enable reads behind a per-org feature flag (Flipt), default off.
3. Default `cache=auto` on globally after a week of clean hit/miss telemetry.
4. Add invalidation + stats endpoints.

## 9. Open questions

- Should `cache=hit` create a real `job` row or a lightweight synthetic response? (Leaning:
  real row for auditability + consistent webhooks.)
- Do cache-hit jobs fire `job.completed` webhooks? (Proposed: yes, with `cache: "hit"` in payload.)
- Default TTL vs. "follow artifact retention" — pick one default per plan tier.
