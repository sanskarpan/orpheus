-- name: GetOrganization :one
-- Get an organization by id. RLS scopes this to the current tenant unless
-- the connection has the service role flag set.
SELECT *
FROM organizations
WHERE id = $1
  AND deleted_at IS NULL;

-- name: GetOrganizationBySlug :one
SELECT *
FROM organizations
WHERE slug = $1
  AND deleted_at IS NULL;

-- name: ListOrganizations :many
-- Returns active (non-deleted) organizations, newest first.
SELECT *
FROM organizations
WHERE deleted_at IS NULL
ORDER BY created_at DESC;

-- name: CreateOrganization :one
-- Used at signup; runs in service-role context because the org does not
-- exist yet and RLS would otherwise deny.
INSERT INTO organizations (id, name, slug, plan)
VALUES ($1, $2, $3, sqlc.narg('plan'))
RETURNING *;

-- name: UpdateOrganization :exec
UPDATE organizations
SET name       = $2,
    plan       = $3,
    updated_at = now()
WHERE id = $1;

-- name: DeleteOrganization :exec
-- Soft delete: keep the row for audit, but it will be filtered out of
-- future reads and the partial index.
UPDATE organizations
SET deleted_at = now(),
    updated_at = now()
WHERE id = $1;
