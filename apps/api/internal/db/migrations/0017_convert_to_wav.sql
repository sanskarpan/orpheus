-- +goose Up
-- ============================================================================
-- 0017_convert_to_wav.sql — register the standalone convert-to-wav processor.
--
-- Transcode any source artifact to 16 kHz mono 16-bit PCM WAV as a first-class
-- job (the same conversion transcribe/diarize do inline). Seeds the catalog so
-- it is submittable via POST /v1/jobs. The catalog is service-role-write (RLS),
-- so set the transaction-local GUC, as in 0010/0011.
-- ============================================================================

SELECT set_config('app.is_service', 'true', true);

-- +goose StatementBegin
DO $$
DECLARE
    p_conv uuid := gen_random_uuid();
BEGIN
    INSERT INTO processors (id, name, display_name, description, tier, timeout_seconds, cost_per_job_usd) VALUES
        (p_conv, 'convert-to-wav', 'Convert to WAV', 'Transcode audio to 16 kHz mono 16-bit PCM WAV.', 'cpu_tiny', 180, 0.0005);

    INSERT INTO processor_versions (processor_id, version, model_id, model_version_id, cacheable) VALUES
        (p_conv, '1.0.0', 'ffmpeg', 'ffmpeg-1', true);
END $$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM processor_versions WHERE processor_id IN (
    SELECT id FROM processors WHERE name = 'convert-to-wav'
);
DELETE FROM processors WHERE name = 'convert-to-wav';
