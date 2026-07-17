-- +goose Up
-- ============================================================================
-- 0016_erasure.sql — GDPR right-to-erasure (PRD 10).
--
-- An erasure_request scopes what to hard-delete (an artifact, or all data
-- linked to a job). The erasure worker deletes the audio bytes from S3,
-- soft-deletes the metadata rows, cascades into the content cache (PRD 01) and
-- bundles (PRD 02), and writes a signed certificate. A legal_hold on an
-- artifact blocks its erasure.
-- ============================================================================
ALTER TABLE artifacts ADD COLUMN deleted_at          timestamptz;
ALTER TABLE artifacts ADD COLUMN erasure_request_id  uuid;
ALTER TABLE artifacts ADD COLUMN legal_hold          boolean NOT NULL DEFAULT false;

CREATE TABLE erasure_requests (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id             uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    scope              text        NOT NULL,          -- artifact | job
    target_id          uuid,
    subject_ref        text,
    reason             text,
    status             text        NOT NULL DEFAULT 'scheduled', -- scheduled|running|completed|failed
    requested_by       uuid,
    deleted_counts     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    s3_objects_purged  integer     NOT NULL DEFAULT 0,
    certificate_s3_key text,
    scheduled_at       timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    error              text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT erasure_requests_scope_chk  CHECK (scope IN ('artifact', 'job')),
    CONSTRAINT erasure_requests_status_chk CHECK (status IN ('scheduled', 'running', 'completed', 'failed'))
);
CREATE INDEX erasure_requests_org_idx       ON erasure_requests (org_id, created_at DESC);
CREATE INDEX erasure_requests_scheduled_idx ON erasure_requests (status) WHERE status = 'scheduled';

ALTER TABLE erasure_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE erasure_requests FORCE  ROW LEVEL SECURITY;
CREATE POLICY erasure_requests_tenant_select ON erasure_requests
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY erasure_requests_tenant_insert ON erasure_requests
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY erasure_requests_tenant_update ON erasure_requests
    FOR UPDATE USING (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY erasure_requests_tenant_delete ON erasure_requests
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP TABLE IF EXISTS erasure_requests;
ALTER TABLE artifacts DROP COLUMN IF EXISTS legal_hold;
ALTER TABLE artifacts DROP COLUMN IF EXISTS erasure_request_id;
ALTER TABLE artifacts DROP COLUMN IF EXISTS deleted_at;
