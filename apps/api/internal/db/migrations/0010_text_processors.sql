-- +goose Up
-- ============================================================================
-- 0010_text_processors.sql — register the text processors (PRD 04).
--
-- These are submittable via POST /v1/jobs, which resolves (name, version) in
-- the catalog. The pinned model_version_id is a placeholder here; the worker's
-- LLM provider reports the concrete snapshot (e.g. anthropic:<model>) in each
-- result for audit. detect-language is deterministic + cacheable; translate is
-- deterministic given the pinned model (cacheable); summarize is marked
-- non-cacheable (generative).
-- ============================================================================

-- The processors/processor_versions catalog is service-role-write (RLS). goose
-- runs each migration in a transaction, so this transaction-local GUC lets the
-- seed satisfy is_service_role().
SELECT set_config('app.is_service', 'true', true);

-- +goose StatementBegin
DO $$
DECLARE
    p_detect uuid := gen_random_uuid();
    p_trans  uuid := gen_random_uuid();
    p_summ   uuid := gen_random_uuid();
BEGIN
    INSERT INTO processors (id, name, display_name, description, tier, timeout_seconds, cost_per_job_usd) VALUES
        (p_detect, 'text.detect-language', 'Detect Language', 'Detect the language of a transcript.', 'cpu_tiny', 60, 0.0001),
        (p_trans,  'text.translate',       'Translate',       'Translate transcript segments to a target language.', 'cpu_small', 300, 0.002),
        (p_summ,   'text.summarize',       'Summarize',       'LLM summary of a transcript (abstract/bullets/chapters/action_items).', 'cpu_small', 300, 0.003);

    INSERT INTO processor_versions (processor_id, version, model_id, model_version_id, cacheable) VALUES
        (p_detect, '1.0.0', 'lang-detect',   'lang-detect-1',  true),
        (p_trans,  '1.0.0', 'orpheus-llm',   'orpheus-llm-1',  true),
        (p_summ,   '1.0.0', 'orpheus-llm',   'orpheus-llm-1',  false);
END $$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM processor_versions WHERE processor_id IN (
    SELECT id FROM processors WHERE name IN ('text.detect-language', 'text.translate', 'text.summarize')
);
DELETE FROM processors WHERE name IN ('text.detect-language', 'text.translate', 'text.summarize');
