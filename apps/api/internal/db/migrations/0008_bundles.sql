-- +goose Up
-- ============================================================================
-- 0008_bundles.sql — signed, expiring artifact download bundles (PRD 02).
--
-- A bundle collects a set of artifacts (and optionally the result JSON of
-- source jobs) into a single .zip in S3, downloadable via one signed,
-- expiring URL. Zipping runs as a job (the export.bundle processor), so it
-- reuses job orchestration, retries, and cost attribution. Both tables are
-- org-scoped and RLS-forced.
--
-- status: building → ready | failed  (and → expired | revoked later)
-- ============================================================================
CREATE TABLE bundles (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name                text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT 'building',
    s3_bucket           text,
    s3_key              text,
    size_bytes          bigint      NOT NULL DEFAULT 0,
    artifact_count      integer     NOT NULL DEFAULT 0,
    include_result_json boolean     NOT NULL DEFAULT true,
    -- Result JSON of source jobs to embed (path_in_zip -> result), collected
    -- at create time so the worker needs no cross-tenant job read.
    result_docs         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    job_id              uuid REFERENCES jobs(id) ON DELETE SET NULL,
    error               text,
    expires_at          timestamptz,
    created_by          uuid,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT bundles_status_chk
        CHECK (status IN ('building', 'ready', 'failed', 'expired', 'revoked'))
);

CREATE INDEX bundles_org_id_created_at_idx ON bundles (org_id, created_at DESC);

CREATE TABLE bundle_items (
    bundle_id   uuid NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    org_id      uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    artifact_id uuid NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    path_in_zip text NOT NULL,
    PRIMARY KEY (bundle_id, artifact_id)
);

ALTER TABLE bundles      ENABLE ROW LEVEL SECURITY;
ALTER TABLE bundles      FORCE  ROW LEVEL SECURITY;
ALTER TABLE bundle_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE bundle_items FORCE  ROW LEVEL SECURITY;

CREATE POLICY bundles_tenant_select ON bundles
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY bundles_tenant_insert ON bundles
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY bundles_tenant_update ON bundles
    FOR UPDATE USING      (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY bundles_tenant_delete ON bundles
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY bundle_items_tenant_select ON bundle_items
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY bundle_items_tenant_insert ON bundle_items
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY bundle_items_tenant_update ON bundle_items
    FOR UPDATE USING      (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY bundle_items_tenant_delete ON bundle_items
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP POLICY IF EXISTS bundle_items_tenant_select ON bundle_items;
DROP POLICY IF EXISTS bundle_items_tenant_insert ON bundle_items;
DROP POLICY IF EXISTS bundle_items_tenant_update ON bundle_items;
DROP POLICY IF EXISTS bundle_items_tenant_delete ON bundle_items;
DROP POLICY IF EXISTS bundles_tenant_select ON bundles;
DROP POLICY IF EXISTS bundles_tenant_insert ON bundles;
DROP POLICY IF EXISTS bundles_tenant_update ON bundles;
DROP POLICY IF EXISTS bundles_tenant_delete ON bundles;
DROP TABLE IF EXISTS bundle_items;
DROP INDEX IF EXISTS bundles_org_id_created_at_idx;
DROP TABLE IF EXISTS bundles;
