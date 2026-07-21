-- +goose Up
-- ============================================================================
-- 0020_marketplace.sql — processor marketplace foundation (Phase 7).
--
-- Adds trust classification to the processor catalog (first_party / verified /
-- community) + a publisher, and a moderation queue for tenant-submitted
-- community processors. First-party processors (everything synced from code)
-- default to trust_class='first_party', publisher='orpheus'.
-- ============================================================================

ALTER TABLE processors
    ADD COLUMN trust_class text NOT NULL DEFAULT 'first_party'
        CHECK (trust_class IN ('first_party', 'verified', 'community')),
    ADD COLUMN publisher   text NOT NULL DEFAULT 'orpheus';

-- Tenant-submitted community processors awaiting moderation. Org-scoped so a
-- publisher sees only their own submissions; approval promotes into the
-- public processor catalog.
CREATE TABLE marketplace_submissions (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    display_name text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    publisher    text        NOT NULL,
    status       text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected')),
    review_notes text,
    submitted_at timestamptz NOT NULL DEFAULT now(),
    reviewed_at  timestamptz
);

CREATE INDEX marketplace_submissions_org_idx ON marketplace_submissions (org_id, submitted_at DESC);
CREATE INDEX marketplace_submissions_status_idx ON marketplace_submissions (status);

ALTER TABLE marketplace_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE marketplace_submissions FORCE  ROW LEVEL SECURITY;

CREATE POLICY marketplace_submissions_tenant_select ON marketplace_submissions
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());
CREATE POLICY marketplace_submissions_tenant_insert ON marketplace_submissions
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());
-- Moderation (status change) is service/admin-only.
CREATE POLICY marketplace_submissions_service_update ON marketplace_submissions
    FOR UPDATE
    USING      (is_service_role())
    WITH CHECK (is_service_role());

-- +goose Down
DROP TABLE marketplace_submissions;
ALTER TABLE processors DROP COLUMN publisher;
ALTER TABLE processors DROP COLUMN trust_class;
