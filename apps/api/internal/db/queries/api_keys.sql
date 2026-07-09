-- name: GetAPIKey :one
SELECT *
FROM api_keys
WHERE id = $1
  AND org_id = $2;

-- name: GetAPIKeyByPrefix :one
-- Used by the auth middleware. The prefix is the only piece of the key
-- we can use to find the row before verifying the hashed_secret. This
-- query intentionally does NOT scope to org_id; the caller resolves the
-- org from the row itself.
SELECT *
FROM api_keys
WHERE prefix = $1
  AND revoked_at IS NULL
LIMIT 1;

-- name: ListAPIKeysByOrg :many
SELECT *
FROM api_keys
WHERE org_id = $1
  AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: CreateAPIKey :one
INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateAPIKeyLastUsed :exec
UPDATE api_keys
SET last_used_at = now()
WHERE id = $1
  AND org_id = $2;

-- name: RevokeAPIKey :exec
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1
  AND org_id = $2;
