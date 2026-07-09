-- name: InsertAuditLog :one
-- Append-only. RLS policies intentionally omit UPDATE/DELETE for this
-- table — once an event is recorded, it cannot be tampered with via
-- the API role.
INSERT INTO audit_log (
    id, org_id, user_id, actor_type, action,
    resource_type, resource_id, ip, user_agent, request_id, metadata
)
VALUES (
    $1, $2,
    sqlc.narg('user_id'),
    $3, $4,
    $5, $6,
    sqlc.narg('ip')::inet,
    sqlc.narg('user_agent'),
    sqlc.narg('request_id'),
    sqlc.narg('metadata')
)
RETURNING *;

-- name: ListAuditLogByOrg :many
-- Optional action / resource_type filters.
SELECT *
FROM audit_log
WHERE org_id = $1
  AND (sqlc.narg('action')::audit_action IS NULL OR action = sqlc.narg('action')::audit_action)
  AND (sqlc.narg('resource_type') IS NULL OR resource_type = sqlc.narg('resource_type'))
ORDER BY created_at DESC
LIMIT $2;
