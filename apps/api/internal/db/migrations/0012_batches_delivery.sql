-- +goose Up
-- ============================================================================
-- 0012_batches_delivery.sql — tracked batches + tenant-S3 result delivery (PRD 06).
--
-- A batch groups many jobs as one trackable unit with an aggregate
-- batch.completed callback. A delivery_destination optionally pushes each
-- child's result.json to the tenant's own S3 (via a cross-account role we
-- assume — no tenant secret keys stored; a 's3_static' type targets our own
-- MinIO for dev/test). Both tables are org-scoped and RLS-forced.
-- ============================================================================

CREATE TABLE delivery_destinations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    type        text        NOT NULL DEFAULT 's3_sts',
    bucket      text        NOT NULL,
    prefix      text        NOT NULL DEFAULT '',
    region      text        NOT NULL DEFAULT 'us-east-1',
    role_arn    text,        -- for s3_sts: the tenant role we assume via STS
    external_id text,        -- confused-deputy defense for the assume-role
    endpoint    text,        -- for s3_static: a custom S3 endpoint (MinIO)
    verified_at timestamptz,
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delivery_destinations_type_chk CHECK (type IN ('s3_sts', 's3_static'))
);
CREATE INDEX delivery_destinations_org_idx ON delivery_destinations (org_id, created_at DESC);

CREATE TABLE batches (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name                text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT 'running',
    job_count           integer     NOT NULL DEFAULT 0,
    completed_count     integer     NOT NULL DEFAULT 0,
    failed_count        integer     NOT NULL DEFAULT 0,
    callback_webhook_id uuid REFERENCES webhook_endpoints(id) ON DELETE SET NULL,
    destination_id      uuid REFERENCES delivery_destinations(id) ON DELETE SET NULL,
    manifest_s3_key     text,
    callback_sent       boolean     NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT batches_status_chk CHECK (status IN ('running', 'completed', 'failed'))
);
CREATE INDEX batches_org_idx ON batches (org_id, created_at DESC);
CREATE INDEX batches_running_idx ON batches (status) WHERE status = 'running';

-- Child jobs reference their batch; delivery_status tracks the tenant-S3 push.
ALTER TABLE jobs ADD COLUMN batch_id uuid REFERENCES batches(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN delivery_status text;  -- null | delivered | delivery_failed
CREATE INDEX jobs_batch_id_idx ON jobs (batch_id) WHERE batch_id IS NOT NULL;

ALTER TABLE delivery_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_destinations FORCE  ROW LEVEL SECURITY;
ALTER TABLE batches               ENABLE ROW LEVEL SECURITY;
ALTER TABLE batches               FORCE  ROW LEVEL SECURITY;

CREATE POLICY delivery_destinations_tenant_select ON delivery_destinations
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY delivery_destinations_tenant_insert ON delivery_destinations
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY delivery_destinations_tenant_update ON delivery_destinations
    FOR UPDATE USING      (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY delivery_destinations_tenant_delete ON delivery_destinations
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY batches_tenant_select ON batches
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY batches_tenant_insert ON batches
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY batches_tenant_update ON batches
    FOR UPDATE USING      (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY batches_tenant_delete ON batches
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP INDEX IF EXISTS jobs_batch_id_idx;
ALTER TABLE jobs DROP COLUMN IF EXISTS delivery_status;
ALTER TABLE jobs DROP COLUMN IF EXISTS batch_id;
DROP POLICY IF EXISTS batches_tenant_select ON batches;
DROP POLICY IF EXISTS batches_tenant_insert ON batches;
DROP POLICY IF EXISTS batches_tenant_update ON batches;
DROP POLICY IF EXISTS batches_tenant_delete ON batches;
DROP POLICY IF EXISTS delivery_destinations_tenant_select ON delivery_destinations;
DROP POLICY IF EXISTS delivery_destinations_tenant_insert ON delivery_destinations;
DROP POLICY IF EXISTS delivery_destinations_tenant_update ON delivery_destinations;
DROP POLICY IF EXISTS delivery_destinations_tenant_delete ON delivery_destinations;
DROP TABLE IF EXISTS batches;
DROP TABLE IF EXISTS delivery_destinations;
