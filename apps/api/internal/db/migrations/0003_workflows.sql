-- +goose Up
-- ============================================================================
-- 0003_workflows.sql — workflows table for the transcribe-long (and
-- future) multi-step user-facing workflows. The actual work runs as
-- a job in the `jobs` table; the workflow row tracks the higher-level
-- request and aggregates the final result.
-- ============================================================================
CREATE TABLE workflows (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    type         text NOT NULL,
    status       text NOT NULL DEFAULT 'queued',
    artifact_id  uuid NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
    params       jsonb NOT NULL DEFAULT '{}'::jsonb,
    result       jsonb,
    current_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL,
    error        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workflows_org_id_status_created_at_idx
    ON workflows (org_id, status, created_at);
CREATE INDEX workflows_current_job_id_idx
    ON workflows (current_job_id)
    WHERE current_job_id IS NOT NULL;

ALTER TABLE workflows ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflows FORCE  ROW LEVEL SECURITY;

CREATE POLICY workflows_tenant_select ON workflows
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY workflows_tenant_insert ON workflows
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY workflows_tenant_update ON workflows
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY workflows_tenant_delete ON workflows
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP POLICY IF EXISTS workflows_tenant_select    ON workflows;
DROP POLICY IF EXISTS workflows_tenant_insert    ON workflows;
DROP POLICY IF EXISTS workflows_tenant_update    ON workflows;
DROP POLICY IF EXISTS workflows_tenant_delete    ON workflows;
DROP INDEX IF EXISTS workflows_current_job_id_idx;
DROP INDEX IF EXISTS workflows_org_id_status_created_at_idx;
DROP TABLE IF EXISTS workflows;
