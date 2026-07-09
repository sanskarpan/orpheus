-- name: GetUser :one
SELECT *
FROM users
WHERE id = $1
  AND org_id = $2;

-- name: GetUserByEmail :one
SELECT *
FROM users
WHERE org_id = $1
  AND email = $2;

-- name: ListUsersByOrg :many
SELECT *
FROM users
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: CreateUser :one
INSERT INTO users (id, org_id, email, name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateUser :exec
UPDATE users
SET name       = $3,
    updated_at = now()
WHERE id = $1
  AND org_id = $2;
