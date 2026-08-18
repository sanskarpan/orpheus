-- +goose Up
-- ============================================================================
-- 0022_transcript_embeddings.sql — transcript embedding index for cross-meeting
-- semantic search + ask-AI-with-citations (PRD 06 #359/#360).
--
-- One row per transcript segment: the segment text, its timing, and a unit-norm
-- sentence embedding. Org-scoped + FORCE RLS like every tenant table. Embeddings
-- are stored as real[] and ranked cosine-wise in the worker; the production scale
-- path is pgvector (add a `vector(dim)` column + an ivfflat/hnsw index and swap
-- the ranking to SQL — the row shape here is chosen to make that a drop-in).
-- Segment text is transcript-derived, so GDPR erasure purges this table too.
-- ============================================================================

CREATE TABLE transcript_embeddings (
    id                uuid                PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            uuid                NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    job_id            uuid,               -- transcript job this segment came from
    artifact_id       uuid,               -- source audio artifact (for playback links)
    segment_index     int                 NOT NULL,
    start_seconds     double precision    NOT NULL DEFAULT 0,
    end_seconds       double precision    NOT NULL DEFAULT 0,
    text              text                NOT NULL,
    embedding         real[]              NOT NULL,   -- unit-norm; pgvector = scale path
    dim               int                 NOT NULL,
    model_version_id  text                NOT NULL,
    created_at        timestamptz         NOT NULL DEFAULT now()
);

CREATE INDEX transcript_embeddings_org_job_idx ON transcript_embeddings (org_id, job_id);
CREATE INDEX transcript_embeddings_org_created_idx ON transcript_embeddings (org_id, created_at DESC);

ALTER TABLE transcript_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE transcript_embeddings FORCE  ROW LEVEL SECURITY;

CREATE POLICY transcript_embeddings_tenant_select ON transcript_embeddings
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY transcript_embeddings_tenant_insert ON transcript_embeddings
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
-- Re-indexing a transcript replaces its rows; GDPR erasure deletes them.
CREATE POLICY transcript_embeddings_tenant_delete ON transcript_embeddings
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP TABLE transcript_embeddings;
