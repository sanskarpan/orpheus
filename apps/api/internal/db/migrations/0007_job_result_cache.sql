-- +goose Up
-- ============================================================================
-- 0007_job_result_cache.sql — content-addressed result cache (PRD 01).
--
-- Reproducibility already holds: (processor, version) pins a model_version_id,
-- so identical input + params + model yields an identical result. This table
-- lets an identical job return that prior result instead of recomputing it.
--
-- Cache key = sha256(input_hash || 0x00 || params_hash || 0x00 || model_version_id)
--   input_hash  = artifacts.sha256 of the job's input
--   params_hash = sha256 of the canonicalized params JSON (sorted keys)
-- The key is computed once, in the API (Go), and stored on the job as
-- `cache_meta` so the worker can populate the cache with the exact same key
-- (no cross-language canonicalization risk).
--
-- Cache is org-scoped and RLS-forced: a hit can never cross tenants even on
-- byte-identical content.
-- ============================================================================

-- Processors opt out of caching when non-deterministic (e.g. random-seed
-- generation). Default true; the catalog seeds false for such processors.
ALTER TABLE processor_versions
    ADD COLUMN cacheable boolean NOT NULL DEFAULT true;

-- Cache bookkeeping on the job row.
ALTER TABLE jobs ADD COLUMN cache_hit          boolean NOT NULL DEFAULT false;
ALTER TABLE jobs ADD COLUMN cached_from_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL;
-- {ck: cache_key hex, ih: input_hash, ph: params_hash, mv: model_version_id}
-- Present only for a cacheable job that should populate the cache on success.
ALTER TABLE jobs ADD COLUMN cache_meta         jsonb;

CREATE TABLE job_result_cache (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    cache_key        bytea       NOT NULL,
    input_hash       text        NOT NULL,
    params_hash      text        NOT NULL,
    model_version_id text        NOT NULL,
    source_job_id    uuid        NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    result           jsonb       NOT NULL,
    hit_count        integer     NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_hit_at      timestamptz,
    CONSTRAINT job_result_cache_org_key_uniq UNIQUE (org_id, cache_key)
);

-- Invalidation by model_version_id (operator purge of a defective model).
CREATE INDEX job_result_cache_org_model_idx
    ON job_result_cache (org_id, model_version_id);

ALTER TABLE job_result_cache ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_result_cache FORCE  ROW LEVEL SECURITY;

CREATE POLICY job_result_cache_tenant_select ON job_result_cache
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY job_result_cache_tenant_insert ON job_result_cache
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY job_result_cache_tenant_update ON job_result_cache
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY job_result_cache_tenant_delete ON job_result_cache
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP POLICY IF EXISTS job_result_cache_tenant_select ON job_result_cache;
DROP POLICY IF EXISTS job_result_cache_tenant_insert ON job_result_cache;
DROP POLICY IF EXISTS job_result_cache_tenant_update ON job_result_cache;
DROP POLICY IF EXISTS job_result_cache_tenant_delete ON job_result_cache;
DROP INDEX IF EXISTS job_result_cache_org_model_idx;
DROP TABLE IF EXISTS job_result_cache;
ALTER TABLE jobs DROP COLUMN IF EXISTS cache_meta;
ALTER TABLE jobs DROP COLUMN IF EXISTS cached_from_job_id;
ALTER TABLE jobs DROP COLUMN IF EXISTS cache_hit;
ALTER TABLE processor_versions DROP COLUMN IF EXISTS cacheable;
