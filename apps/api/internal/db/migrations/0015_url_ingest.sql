-- +goose Up
-- ============================================================================
-- 0015_url_ingest.sql — URL ingest source columns on upload_sessions (PRD 09).
--
-- A URL-ingest session (source='url') is fetched by the orpheus.ingest.url
-- worker job into S3 as a normal artifact. Resumable multipart uploads need no
-- schema change (S3 ListParts is the source of truth for uploaded parts).
-- ============================================================================
ALTER TABLE upload_sessions ADD COLUMN source       text   NOT NULL DEFAULT 'multipart';  -- multipart | url
ALTER TABLE upload_sessions ADD COLUMN source_url   text;
ALTER TABLE upload_sessions ADD COLUMN fetch_status text;   -- fetching | ready | failed
ALTER TABLE upload_sessions ADD COLUMN fetch_error  text;
ALTER TABLE upload_sessions ADD COLUMN bytes_fetched bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE upload_sessions DROP COLUMN IF EXISTS bytes_fetched;
ALTER TABLE upload_sessions DROP COLUMN IF EXISTS fetch_error;
ALTER TABLE upload_sessions DROP COLUMN IF EXISTS fetch_status;
ALTER TABLE upload_sessions DROP COLUMN IF EXISTS source_url;
ALTER TABLE upload_sessions DROP COLUMN IF EXISTS source;
