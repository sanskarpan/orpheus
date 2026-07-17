-- +goose Up
-- ============================================================================
-- 0013_usage_budgets.sql — usage analytics rollup + budgets (PRD 07).
--
-- usage_rollup_hourly is the near-real-time analytics store: completed jobs
-- aggregated per org per hour, per dimension (total | processor | status).
-- The GET /v1/usage/timeseries endpoint and budget-spend both read it.
-- budgets set spend limits with threshold alerts; budget_alerts dedupes each
-- (budget, period, threshold) so an alert fires once. All org-scoped, RLS.
-- ============================================================================

CREATE TABLE usage_rollup_hourly (
    org_id          uuid          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    hour            timestamptz   NOT NULL,
    dimension       text          NOT NULL,   -- total | processor | status
    dimension_value text          NOT NULL DEFAULT '',
    jobs            integer       NOT NULL DEFAULT 0,
    compute_seconds numeric(16, 3) NOT NULL DEFAULT 0,
    cost_usd        numeric(14, 6) NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, hour, dimension, dimension_value)
);
CREATE INDEX usage_rollup_hourly_org_hour_idx ON usage_rollup_hourly (org_id, hour);

CREATE TABLE budgets (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    scope           text          NOT NULL DEFAULT 'org',      -- org | processor
    scope_id        text,                                       -- processor name for scope=processor
    period          text          NOT NULL DEFAULT 'monthly',  -- monthly
    limit_usd       numeric(14, 6) NOT NULL,
    alert_thresholds numeric[]    NOT NULL DEFAULT '{0.5,0.8,1.0}',
    enforcement     text          NOT NULL DEFAULT 'alert',    -- alert | hard_cap
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT budgets_scope_chk       CHECK (scope IN ('org', 'processor')),
    CONSTRAINT budgets_enforcement_chk CHECK (enforcement IN ('alert', 'hard_cap'))
);
CREATE INDEX budgets_org_idx ON budgets (org_id);

CREATE TABLE budget_alerts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    budget_id    uuid        NOT NULL REFERENCES budgets(id) ON DELETE CASCADE,
    period_start timestamptz NOT NULL,
    threshold    numeric     NOT NULL,
    spend_usd    numeric(14, 6) NOT NULL,
    fired_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT budget_alerts_dedupe UNIQUE (budget_id, period_start, threshold)
);

ALTER TABLE usage_rollup_hourly ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_rollup_hourly FORCE  ROW LEVEL SECURITY;
ALTER TABLE budgets             ENABLE ROW LEVEL SECURITY;
ALTER TABLE budgets             FORCE  ROW LEVEL SECURITY;
ALTER TABLE budget_alerts       ENABLE ROW LEVEL SECURITY;
ALTER TABLE budget_alerts       FORCE  ROW LEVEL SECURITY;

CREATE POLICY usage_rollup_hourly_tenant_select ON usage_rollup_hourly
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY usage_rollup_hourly_tenant_insert ON usage_rollup_hourly
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY usage_rollup_hourly_tenant_update ON usage_rollup_hourly
    FOR UPDATE USING (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY usage_rollup_hourly_tenant_delete ON usage_rollup_hourly
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY budgets_tenant_select ON budgets
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY budgets_tenant_insert ON budgets
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY budgets_tenant_update ON budgets
    FOR UPDATE USING (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY budgets_tenant_delete ON budgets
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY budget_alerts_tenant_select ON budget_alerts
    FOR SELECT USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY budget_alerts_tenant_insert ON budget_alerts
    FOR INSERT WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY budget_alerts_tenant_update ON budget_alerts
    FOR UPDATE USING (is_service_role() OR org_id = current_org_id())
               WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY budget_alerts_tenant_delete ON budget_alerts
    FOR DELETE USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP TABLE IF EXISTS budget_alerts;
DROP TABLE IF EXISTS budgets;
DROP TABLE IF EXISTS usage_rollup_hourly;
