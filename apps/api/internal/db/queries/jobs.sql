-- name: GetJob :one
SELECT *
FROM jobs
WHERE id = $1
  AND org_id = $2;

-- name: ListJobsByOrg :many
-- Optional status / job_type filters via sqlc.narg.
SELECT *
FROM jobs
WHERE org_id = $1
  AND (sqlc.narg('status')::job_status IS NULL OR status = sqlc.narg('status')::job_status)
  AND (sqlc.narg('job_type')::job_type IS NULL OR job_type = sqlc.narg('job_type')::job_type)
ORDER BY created_at DESC
LIMIT $2;

-- name: CreateJob :one
INSERT INTO jobs (
    id, org_id, user_id, artifact_id, job_type, params,
    priority, max_retries, cost_usd
)
VALUES (
    $1, $2, $3,
    sqlc.narg('artifact_id'),
    $4, $5,
    sqlc.narg('priority'),
    sqlc.narg('max_retries'),
    sqlc.narg('cost_usd')
)
RETURNING *;

-- name: UpdateJobStatus :exec
-- Caller is expected to bump `version` themselves; we touch updated_at
-- to keep the row's freshness metadata accurate.
UPDATE jobs
SET status     = $3,
    updated_at = now(),
    version    = version + 1
WHERE id = $1
  AND org_id = $2;

-- name: UpdateJobResult :exec
UPDATE jobs
SET result     = $3,
    updated_at = now(),
    version    = version + 1
WHERE id = $1
  AND org_id = $2;

-- name: ClaimNextJob :one
-- Worker pool entry point. SELECT ... FOR UPDATE SKIP LOCKED picks the
-- oldest queued job that is not already held by another worker, in a
-- single statement. The outer UPDATE flips the row to 'running'.
UPDATE jobs
SET status     = 'running',
    started_at = now(),
    attempts   = attempts + 1,
    updated_at = now(),
    version    = version + 1
WHERE id = (
    SELECT id
    FROM jobs
    WHERE status = 'queued'
    ORDER BY priority DESC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;
