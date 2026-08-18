-- +goose Up
-- ============================================================================
-- 0021_speaker_profiles.sql — persistent speaker enrollment / voiceprints
-- (PRD 03 §4.8).
--
-- Stores per-tenant enrolled speaker voiceprints (mean-normalized ECAPA
-- embeddings) so a returning speaker can be recognized across jobs. Voiceprints
-- are BIOMETRIC data: enrollment is explicit + consent-gated (consent must be
-- true to insert), rows are RLS-scoped to the owning org, and DELETE is allowed
-- to the tenant so GDPR erasure can purge a profile. Embeddings are compared in
-- the worker (cosine), so no pgvector dependency is required.
-- ============================================================================

CREATE TABLE speaker_profiles (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name              text        NOT NULL,
    embedding         real[]      NOT NULL,               -- unit-norm voice embedding
    dim               int         NOT NULL,
    sample_count      int         NOT NULL DEFAULT 1,
    consent           boolean     NOT NULL DEFAULT false, -- explicit enrollment consent
    model_version_id  text        NOT NULL DEFAULT 'ecapa-agglomerative-1',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX speaker_profiles_org_idx ON speaker_profiles (org_id, name);

ALTER TABLE speaker_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE speaker_profiles FORCE  ROW LEVEL SECURITY;

-- Tenant reads/writes only its own voiceprints; the service role bypasses for
-- worker-side enrollment/recognition running under a job's org context.
CREATE POLICY speaker_profiles_tenant_select ON speaker_profiles
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY speaker_profiles_tenant_insert ON speaker_profiles
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY speaker_profiles_tenant_update ON speaker_profiles
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());
-- GDPR erasure: a tenant may delete its own voiceprints.
CREATE POLICY speaker_profiles_tenant_delete ON speaker_profiles
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP TABLE speaker_profiles;
