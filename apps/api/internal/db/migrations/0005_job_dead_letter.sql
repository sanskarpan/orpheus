-- +goose Up
-- +goose NO TRANSACTION
-- ============================================================================
-- 0005_job_dead_letter.sql — add the 'dead_letter' terminal status to the
-- job_status enum. A job that fails more than max_retries times is moved to
-- dead_letter (distinct from a transient 'failed'); operators requeue it via
-- POST /v1/jobs/{id}/requeue. ALTER TYPE ... ADD VALUE cannot run inside a
-- transaction, hence NO TRANSACTION; IF NOT EXISTS makes it idempotent.
-- ============================================================================
ALTER TYPE job_status ADD VALUE IF NOT EXISTS 'dead_letter';

-- +goose Down
-- Postgres cannot drop an enum value; the Down is a no-op.
SELECT 1;
