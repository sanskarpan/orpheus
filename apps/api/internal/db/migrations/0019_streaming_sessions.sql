-- +goose Up
-- ============================================================================
-- 0019_streaming_sessions.sql — control-plane records for realtime streaming
-- ASR sessions (Phase 8). The WebSocket service (apps/workers streaming.py)
-- does the transcription; this table tracks the session lifecycle so the REST
-- API can create, inspect, list, and finalize a session (persisting the final
-- transcript + billable audio duration + cost). Org-scoped, same RLS shape as
-- `workflows`.
-- ============================================================================

CREATE TABLE streaming_sessions (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    status           text        NOT NULL DEFAULT 'connecting',  -- connecting|live|closing|closed|failed
    model_version_id text,
    started_at       timestamptz NOT NULL DEFAULT now(),
    ended_at         timestamptz,
    audio_seconds    numeric(12, 3),
    transcript       text,
    cost_usd         numeric(12, 6) NOT NULL DEFAULT 0,
    error            text
);

CREATE INDEX streaming_sessions_org_started_idx ON streaming_sessions (org_id, started_at DESC);

ALTER TABLE streaming_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE streaming_sessions FORCE  ROW LEVEL SECURITY;

CREATE POLICY streaming_sessions_tenant_select ON streaming_sessions
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY streaming_sessions_tenant_insert ON streaming_sessions
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY streaming_sessions_tenant_update ON streaming_sessions
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY streaming_sessions_tenant_delete ON streaming_sessions
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP TABLE streaming_sessions;
