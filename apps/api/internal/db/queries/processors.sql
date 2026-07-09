-- name: GetProcessor :one
SELECT *
FROM processors
WHERE name = $1;

-- name: ListProcessors :many
SELECT *
FROM processors
ORDER BY name;

-- name: GetProcessorVersion :one
SELECT pv.*
FROM processor_versions pv
JOIN processors p ON p.id = pv.processor_id
WHERE p.name   = $1
  AND pv.version = $2;
