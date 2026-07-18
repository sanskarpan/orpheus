-- +goose Up
-- ============================================================================
-- 0018_model_registry.sql — S3-backed model registry with checksum verification.
--
-- Records the model blobs (whisper/pyannote/… weights) that processors load,
-- with the S3 location and a sha256 the worker verifies on download so a
-- corrupted or tampered model can never be loaded. Global catalog: read-public,
-- write-service-only, same RLS shape as `processors`.
-- ============================================================================

CREATE TABLE model_registry (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    version     text        NOT NULL,
    framework   text        NOT NULL DEFAULT '',   -- faster-whisper, pyannote, ...
    s3_bucket   text        NOT NULL,
    s3_key      text        NOT NULL,
    sha256      text        NOT NULL,
    size_bytes  bigint      NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

ALTER TABLE model_registry ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_registry FORCE  ROW LEVEL SECURITY;

CREATE POLICY model_registry_public_select ON model_registry
    FOR SELECT
    USING (true);

CREATE POLICY model_registry_service_insert ON model_registry
    FOR INSERT
    WITH CHECK (is_service_role());

CREATE POLICY model_registry_service_update ON model_registry
    FOR UPDATE
    USING      (is_service_role())
    WITH CHECK (is_service_role());

CREATE POLICY model_registry_service_delete ON model_registry
    FOR DELETE
    USING (is_service_role());

-- +goose Down
DROP TABLE model_registry;
