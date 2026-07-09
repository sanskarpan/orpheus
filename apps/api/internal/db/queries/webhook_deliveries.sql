-- name: GetDelivery :one
SELECT *
FROM webhook_deliveries
WHERE id = $1
  AND org_id = $2;

-- name: ListPendingDeliveries :many
-- Used by the dispatcher. Pulls up to `limit` rows that are due (or
-- overdue) and not currently in flight.
SELECT *
FROM webhook_deliveries
WHERE org_id        = $1
  AND status        = 'pending'
  AND next_retry_at <= now()
ORDER BY next_retry_at ASC
LIMIT $2;

-- name: CreateDelivery :one
INSERT INTO webhook_deliveries (
    id, org_id, endpoint_id, event_type, event_id, payload,
    attempt_count, max_attempts, next_retry_at
)
VALUES (
    $1, $2, $3, $4, $5, $6,
    sqlc.narg('attempt_count'),
    sqlc.narg('max_attempts'),
    sqlc.narg('next_retry_at')
)
RETURNING *;

-- name: MarkDelivered :exec
UPDATE webhook_deliveries
SET status          = 'delivered',
    response_status = $3,
    response_body   = $4,
    delivered_at    = now()
WHERE id = $1
  AND org_id = $2;

-- name: MarkFailed :exec
-- Stays in 'pending' so the dispatcher will pick it up at next_retry_at
-- unless attempt_count >= max_attempts (caller's responsibility).
UPDATE webhook_deliveries
SET status          = $3,
    response_status = $4,
    response_body   = $5,
    attempt_count   = $6,
    next_retry_at   = $7
WHERE id = $1
  AND org_id = $2;

-- name: IncrementAttempt :exec
UPDATE webhook_deliveries
SET attempt_count = attempt_count + 1,
    next_retry_at = $3
WHERE id = $1
  AND org_id = $2;
