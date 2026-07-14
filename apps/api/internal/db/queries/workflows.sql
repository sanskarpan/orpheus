-- name: CreateWorkflow :one
INSERT INTO workflows (id, org_id, type, status, artifact_id, params, current_job_id)
VALUES (gen_random_uuid(), $1, $2, 'queued', $3, $4, $5)
RETURNING id;

-- name: GetWorkflow :one
SELECT id, org_id, type, status, artifact_id, params, result, current_job_id, error, created_at, updated_at
FROM workflows
WHERE id = $1 AND (is_service_role() OR org_id = current_org_id());

-- name: ListWorkflowsByOrg :many
SELECT id, org_id, type, status, artifact_id, params, result, current_job_id, error, created_at, updated_at
FROM workflows
WHERE org_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: MarkWorkflowRunning :exec
UPDATE workflows
SET status = 'running', updated_at = now()
WHERE id = $1 AND status = 'queued';

-- name: MarkWorkflowCompleted :exec
UPDATE workflows
SET status = 'completed', result = $2, updated_at = now()
WHERE id = $1;

-- name: MarkWorkflowFailed :exec
UPDATE workflows
SET status = 'failed', error = $2, updated_at = now()
WHERE id = $1;
