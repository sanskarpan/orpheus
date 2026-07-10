-- +goose Up
-- ============================================================================
-- 0002_schema_fixes.sql — add columns the Phase 1 handlers reference but
-- that did not exist on the original 0001 schema.
--
-- The handlers in apps/api/internal/handlers/ read and write:
--   - upload_sessions.s3_bucket, s3_key, s3_upload_id
--       (set in UploadHandler.Create, read in UploadHandler.Complete)
--   - api_keys.expires_at
--       (CreateAPIKeyRequest carries an optional ExpiresAt; the
--        current schema has no column for it, so the handler drops the
--        field silently)
--
-- Adding them in 0001 would have been ideal, but 0001 has shipped to
-- dev / staging. ALTER TABLE is additive and safe on the live
-- database; the new columns are NULL-able, so existing rows are
-- untouched.
-- ============================================================================

ALTER TABLE upload_sessions
    ADD COLUMN IF NOT EXISTS s3_bucket    text,
    ADD COLUMN IF NOT EXISTS s3_key       text,
    ADD COLUMN IF NOT EXISTS s3_upload_id text;

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS expires_at timestamptz;

-- A partial index on unexpired keys is small and keeps the
-- "is this key still valid?" check fast as the table grows.
CREATE INDEX IF NOT EXISTS api_keys_expires_at_idx
    ON api_keys (expires_at)
    WHERE expires_at IS NOT NULL AND revoked_at IS NULL;

-- +goose StatementBegin
-- Goose splits on `;` by default; bracket this multi-statement block
-- so the splitter treats it as one statement.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_name = 'upload_sessions'
          AND constraint_name = 'upload_sessions_s3_location_required'
    ) THEN
        ALTER TABLE upload_sessions
            ADD CONSTRAINT upload_sessions_s3_location_required
            CHECK (
                status <> 'complete'
                OR (s3_bucket IS NOT NULL AND s3_key IS NOT NULL)
            );
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- ============================================================================
-- Roll back the additive changes. The CHECK constraint is dropped
-- first so the column drop can proceed without complaining.
-- ============================================================================

ALTER TABLE upload_sessions
    DROP CONSTRAINT IF EXISTS upload_sessions_s3_location_required;

DROP INDEX IF EXISTS api_keys_expires_at_idx;

ALTER TABLE api_keys
    DROP COLUMN IF EXISTS expires_at;

ALTER TABLE upload_sessions
    DROP COLUMN IF EXISTS s3_bucket,
    DROP COLUMN IF EXISTS s3_key,
    DROP COLUMN IF EXISTS s3_upload_id;
