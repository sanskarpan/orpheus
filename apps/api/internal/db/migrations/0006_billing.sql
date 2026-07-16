-- +goose Up
-- ============================================================================
-- 0006_billing.sql — usage-based billing (#11).
--
-- A single `invoices` table doubles as the billing-period rollup: one row
-- per org per period, keyed by (org_id, period_start). The rollup job
-- (internal/billing) aggregates each org's completed-job cost + compute
-- seconds into the row for the period; a payment provider (Dodo Payments)
-- then drives a checkout and, via an inbound webhook, flips the row to
-- 'paid'. RLS is forced, matching every other tenant table.
--
-- status lifecycle: draft → open → paid  (or → void / failed)
--   draft  — rolled up, not yet finalized for collection
--   open   — finalized; a checkout may be created
--   paid   — provider webhook confirmed payment
--   void   — cancelled (e.g. credited)
--   failed — provider reported a failed payment
-- ============================================================================
CREATE TABLE invoices (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    period_start    timestamptz   NOT NULL,
    period_end      timestamptz   NOT NULL,
    jobs_count      integer       NOT NULL DEFAULT 0,
    compute_seconds numeric(14, 3) NOT NULL DEFAULT 0,
    total_usd       numeric(12, 6) NOT NULL DEFAULT 0,
    status          text          NOT NULL DEFAULT 'draft',
    provider        text,
    provider_ref    text,
    checkout_url    text,
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    paid_at         timestamptz,
    CONSTRAINT invoices_status_chk
        CHECK (status IN ('draft', 'open', 'paid', 'void', 'failed')),
    CONSTRAINT invoices_org_period_uniq UNIQUE (org_id, period_start)
);

CREATE INDEX invoices_org_id_period_start_idx
    ON invoices (org_id, period_start DESC);
CREATE INDEX invoices_status_idx
    ON invoices (status)
    WHERE status IN ('draft', 'open');
-- Webhook lookups find the invoice by the provider's payment reference.
CREATE UNIQUE INDEX invoices_provider_ref_idx
    ON invoices (provider_ref)
    WHERE provider_ref IS NOT NULL;

ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE  ROW LEVEL SECURITY;

CREATE POLICY invoices_tenant_select ON invoices
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY invoices_tenant_insert ON invoices
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY invoices_tenant_update ON invoices
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());
CREATE POLICY invoices_tenant_delete ON invoices
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- +goose Down
DROP POLICY IF EXISTS invoices_tenant_select ON invoices;
DROP POLICY IF EXISTS invoices_tenant_insert ON invoices;
DROP POLICY IF EXISTS invoices_tenant_update ON invoices;
DROP POLICY IF EXISTS invoices_tenant_delete ON invoices;
DROP INDEX IF EXISTS invoices_provider_ref_idx;
DROP INDEX IF EXISTS invoices_status_idx;
DROP INDEX IF EXISTS invoices_org_id_period_start_idx;
DROP TABLE IF EXISTS invoices;
