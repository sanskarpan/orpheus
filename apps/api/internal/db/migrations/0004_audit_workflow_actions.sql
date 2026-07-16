-- +goose Up
-- +goose NO TRANSACTION
-- ============================================================================
-- 0004_audit_workflow_actions.sql — add the workflow.* members to the
-- audit_action enum. The workflow endpoints (added in 0003) audit with
-- action="workflow.create"; without these enum members that INSERT
-- fails the ::audit_action cast and the audit row is silently dropped.
--
-- ALTER TYPE ... ADD VALUE cannot run inside a transaction block, hence
-- the "NO TRANSACTION" directive above. Each ADD VALUE is idempotent via
-- IF NOT EXISTS so re-running is safe.
-- ============================================================================
ALTER TYPE audit_action ADD VALUE IF NOT EXISTS 'workflow.create';
ALTER TYPE audit_action ADD VALUE IF NOT EXISTS 'workflow.update';
ALTER TYPE audit_action ADD VALUE IF NOT EXISTS 'workflow.cancel';

-- +goose Down
-- Postgres cannot DROP a value from an enum. The Down migration is a
-- no-op: rolling back leaves the (harmless) extra enum members in place.
SELECT 1;
