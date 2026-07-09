-- name: GetIdempotencyKey :one
SELECT *
FROM idempotency_keys
WHERE org_id = $1
  AND key    = $2;

-- name: InsertIdempotencyKey :one
-- ON CONFLICT DO NOTHING: if a concurrent request already inserted a
-- row for (org_id, key), this returns 0 rows and the caller should
-- fall back to GetIdempotencyKey. The composite UNIQUE constraint on
-- (org_id, key) backs the conflict target.
INSERT INTO idempotency_keys (
    id, org_id, key, request_hash, status, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (org_id, key) DO NOTHING
RETURNING *;

-- name: MarkCompleted :exec
UPDATE idempotency_keys
SET status          = 'completed',
    response_status = $3,
    response_body   = $4
WHERE org_id = $1
  AND key    = $2;
