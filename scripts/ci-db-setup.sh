#!/usr/bin/env bash
# Provision the test database + a dedicated NON-superuser application role
# and the MinIO buckets used by the Go integration / e2e tests.
#
# The role MUST be NOSUPERUSER NOBYPASSRLS: a superuser (which the stock
# postgres image makes POSTGRES_USER by default) silently bypasses every
# RLS policy, so the tenant-isolation tests would pass vacuously. The role
# owns orpheus_test so it can run migrations (pgcrypto is a trusted
# extension, creatable by a non-superuser DB owner).
set -euo pipefail

echo "==> Waiting for Postgres"
for i in $(seq 1 30); do
  if docker compose exec -T postgres pg_isready -U orpheus >/dev/null 2>&1; then
    echo "Postgres is up"; break
  fi
  sleep 2
done

echo "==> Creating orpheus_app role + orpheus_test database"
docker compose exec -T postgres psql -U orpheus -d orpheus -v ON_ERROR_STOP=1 <<'SQL'
DROP DATABASE IF EXISTS orpheus_test;
DROP ROLE IF EXISTS orpheus_app;
CREATE ROLE orpheus_app LOGIN PASSWORD 'orpheus_app' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE orpheus_test OWNER orpheus_app;
SQL

echo "==> Verifying the app role does NOT bypass RLS"
docker compose exec -T postgres psql -U orpheus -d orpheus -tAc \
  "SELECT rolname||' super='||rolsuper||' bypassrls='||rolbypassrls FROM pg_roles WHERE rolname='orpheus_app';"

echo "==> Applying migrations up front (as the app role)"
# Integration test packages run in parallel; only some migrate. Bring the
# schema up before the test binaries start so none races on an empty DB.
( cd apps/api && ORPHEUS_TEST_DATABASE_URL="postgres://orpheus_app:orpheus_app@localhost:5432/orpheus_test?sslmode=disable" go run ./cmd/migrate )

echo "==> Creating MinIO buckets"
for i in $(seq 1 30); do
  if curl -fsS http://localhost:9000/minio/health/live >/dev/null 2>&1; then break; fi
  sleep 2
done
docker run --rm --network host --entrypoint sh minio/mc -c "
  mc alias set local http://localhost:9000 orpheus orpheus-dev-secret &&
  mc mb -p local/orpheus-uploads &&
  mc mb -p local/orpheus-e2e
"

echo "==> CI DB/bucket setup complete"
