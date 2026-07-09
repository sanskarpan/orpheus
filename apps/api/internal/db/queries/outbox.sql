-- name: InsertOutbox :one
-- Outbox events are written in the same transaction as the business
-- state change that produced them, then drained by a background
-- dispatcher that publishes to NATS.
INSERT INTO outbox (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('headers'))
RETURNING *;

-- name: ClaimUnpublished :many
-- Service-role call from the dispatcher. Uses FOR UPDATE SKIP LOCKED
-- so multiple dispatcher instances can run in parallel without
-- double-publishing.
SELECT *
FROM outbox
WHERE published_at IS NULL
ORDER BY created_at ASC
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkPublished :exec
UPDATE outbox
SET published_at = now()
WHERE id = $1;
