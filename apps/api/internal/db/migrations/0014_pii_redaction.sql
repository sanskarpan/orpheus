-- +goose Up
-- ============================================================================
-- 0014_pii_redaction.sql — PII redaction (PRD 08).
--
-- artifacts.sensitivity flags an un-redact mapping ('pii_mapping'), which
-- forces the pii:unmask scope on download (enforced in the artifact handler)
-- and shorter retention. Register the text.redact processor.
-- ============================================================================

ALTER TABLE artifacts ADD COLUMN sensitivity text NOT NULL DEFAULT 'normal';

SELECT set_config('app.is_service', 'true', true);

-- +goose StatementBegin
DO $$
DECLARE
    p_redact uuid := gen_random_uuid();
BEGIN
    INSERT INTO processors (id, name, display_name, description, tier, timeout_seconds, cost_per_job_usd)
    VALUES (p_redact, 'text.redact', 'Redact PII', 'Mask configurable PII entity types in a transcript.', 'cpu_small', 300, 0.001);
    INSERT INTO processor_versions (processor_id, version, model_id, model_version_id, cacheable)
    VALUES (p_redact, '1.0.0', 'orpheus-redact', 'orpheus-redact-1', true);
END $$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM processor_versions WHERE processor_id IN (SELECT id FROM processors WHERE name='text.redact');
DELETE FROM processors WHERE name='text.redact';
ALTER TABLE artifacts DROP COLUMN IF EXISTS sensitivity;
