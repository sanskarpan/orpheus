-- name: GetArtifact :one
SELECT *
FROM artifacts
WHERE id = $1
  AND org_id = $2;

-- name: GetArtifactBySha256 :one
-- Used for client-side dedup. First matching artifact in the org.
SELECT *
FROM artifacts
WHERE org_id = $1
  AND sha256 = $2
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateArtifact :one
INSERT INTO artifacts (
    id,
    org_id,
    upload_session_id,
    s3_bucket,
    s3_key,
    sha256,
    size_bytes,
    content_type,
    codec,
    duration_seconds,
    sample_rate,
    channels
)
VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    sqlc.narg('codec'),
    sqlc.narg('duration_seconds'),
    sqlc.narg('sample_rate'),
    sqlc.narg('channels')
)
RETURNING *;

-- name: ListArtifactsByOrg :many
SELECT *
FROM artifacts
WHERE org_id = $1
ORDER BY created_at DESC
LIMIT $2;
