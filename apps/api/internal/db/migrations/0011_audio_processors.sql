-- +goose Up
-- ============================================================================
-- 0011_audio_processors.sql — register diarization + subtitle processors (PRD 05).
--
-- Seeds the catalog so these are submittable via POST /v1/jobs. As with 0010,
-- the catalog is service-role-write (RLS), so set the transaction-local GUC.
-- (word_timestamps is just a param on the existing transcribe processor, so it
-- needs no catalog change here.)
-- ============================================================================

SELECT set_config('app.is_service', 'true', true);

-- +goose StatementBegin
DO $$
DECLARE
    p_diar  uuid := gen_random_uuid();
    p_subs  uuid := gen_random_uuid();
BEGIN
    INSERT INTO processors (id, name, display_name, description, tier, timeout_seconds, cost_per_job_usd) VALUES
        (p_diar, 'audio.diarize',     'Diarize',           'Assign anonymous speaker labels (S1..Sn) to transcript segments.', 'cpu_medium', 1800, 0.01),
        (p_subs, 'export.subtitles',  'Export Subtitles',  'Render .srt/.vtt from a transcript with optional speaker labels.', 'cpu_tiny', 120, 0.0005);

    INSERT INTO processor_versions (processor_id, version, model_id, model_version_id, cacheable) VALUES
        (p_diar, '1.0.0', 'pyannote',        'pyannote-1',   true),
        (p_subs, '1.0.0', 'subtitle-render', 'subtitle-1',   true);
END $$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM processor_versions WHERE processor_id IN (
    SELECT id FROM processors WHERE name IN ('audio.diarize', 'export.subtitles')
);
DELETE FROM processors WHERE name IN ('audio.diarize', 'export.subtitles');
