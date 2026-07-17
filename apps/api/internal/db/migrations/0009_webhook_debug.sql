-- +goose Up
-- ============================================================================
-- 0009_webhook_debug.sql — webhook tester + delivery replay (PRD 03).
--
-- Turns existing delivery data into a debuggable surface: a per-attempt
-- timeline, the signature base string a receiver should reconstruct, a
-- captured response-body snippet, a test-fire flag, and an auto-disable
-- counter on the endpoint. Additive; no behavior change until the delivery
-- worker + new endpoints write/read these.
-- ============================================================================

ALTER TABLE webhook_deliveries ADD COLUMN signature_base_string text;
ALTER TABLE webhook_deliveries ADD COLUMN response_body_snippet text;
ALTER TABLE webhook_deliveries ADD COLUMN is_test boolean NOT NULL DEFAULT false;

-- Auto-disable bookkeeping: consecutive terminal-failure deliveries. Reset to
-- 0 on any successful delivery; the endpoint is deactivated when it crosses
-- the threshold (enforced in the delivery worker).
ALTER TABLE webhook_endpoints ADD COLUMN consecutive_failures integer NOT NULL DEFAULT 0;

CREATE TABLE webhook_delivery_attempts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_id  uuid        NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    org_id       uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    attempt_no   smallint    NOT NULL,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    status_code  integer,
    duration_ms  integer,
    error        text
);

CREATE INDEX webhook_delivery_attempts_delivery_id_idx
    ON webhook_delivery_attempts (delivery_id, attempt_no);

ALTER TABLE webhook_delivery_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_delivery_attempts FORCE  ROW LEVEL SECURITY;

CREATE POLICY webhook_delivery_attempts_tenant_select ON webhook_delivery_attempts
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY webhook_delivery_attempts_tenant_insert ON webhook_delivery_attempts
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY webhook_delivery_attempts_tenant_update ON webhook_delivery_attempts
    FOR UPDATE USING      (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY webhook_delivery_attempts_tenant_delete ON webhook_delivery_attempts
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP POLICY IF EXISTS webhook_delivery_attempts_tenant_select ON webhook_delivery_attempts;
DROP POLICY IF EXISTS webhook_delivery_attempts_tenant_insert ON webhook_delivery_attempts;
DROP POLICY IF EXISTS webhook_delivery_attempts_tenant_update ON webhook_delivery_attempts;
DROP POLICY IF EXISTS webhook_delivery_attempts_tenant_delete ON webhook_delivery_attempts;
DROP TABLE IF EXISTS webhook_delivery_attempts;
ALTER TABLE webhook_endpoints  DROP COLUMN IF EXISTS consecutive_failures;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS is_test;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS response_body_snippet;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS signature_base_string;
