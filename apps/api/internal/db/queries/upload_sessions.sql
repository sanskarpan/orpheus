-- name: GetUploadSession :one
SELECT *
FROM upload_sessions
WHERE id = $1
  AND org_id = $2;

-- name: ListUploadSessionsByOrg :many
-- Optional status filter via sqlc.narg. If NULL, returns all statuses.
SELECT *
FROM upload_sessions
WHERE org_id = $1
  AND (sqlc.narg('status')::upload_status IS NULL OR status = sqlc.narg('status')::upload_status)
ORDER BY created_at DESC
LIMIT $2;

-- name: CreateUploadSession :one
INSERT INTO upload_sessions (id, org_id, user_id, filename, content_type, size_bytes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateUploadSessionStatus :exec
UPDATE upload_sessions
SET status     = $3,
    updated_at = now()
WHERE id = $1
  AND org_id = $2;

-- name: CompleteUploadSession :one
-- Marks the session complete and returns the artifact that was created
-- for it. The artifact row is expected to have been written in the same
-- transaction by the caller; this query joins through to it.
WITH completed AS (
    UPDATE upload_sessions
    SET status       = 'complete',
        completed_at = now()
    WHERE id = $1
      AND org_id = $2
    RETURNING id, org_id
)
SELECT a.*
FROM artifacts a
JOIN completed c
  ON c.id = a.upload_session_id
WHERE a.org_id = $2
LIMIT 1;
