-- name: GetWebhookEndpoint :one
SELECT *
FROM webhook_endpoints
WHERE id = $1
  AND org_id = $2;

-- name: ListEndpointsByOrg :many
SELECT *
FROM webhook_endpoints
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: CreateEndpoint :one
INSERT INTO webhook_endpoints (id, org_id, url, secret, description, subscribed_events, active)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateEndpoint :exec
-- The signing secret is intentionally not part of update; rotate by
-- creating a new endpoint and revoking the old one.
UPDATE webhook_endpoints
SET url              = $2,
    description      = $3,
    subscribed_events = $4,
    active           = $5,
    updated_at       = now()
WHERE id = $1
  AND org_id = $6;

-- name: DeleteEndpoint :exec
DELETE FROM webhook_endpoints
WHERE id = $1
  AND org_id = $2;
