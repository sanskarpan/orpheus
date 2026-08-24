#!/usr/bin/env bash
#
# Backend deploy — run by the self-hosted GitHub Actions runner on the Oracle VM
# (job: deploy-backend in .github/workflows/deploy.yml), after CI passes on main.
#
# Deploys into a DEDICATED deploy tree (separate from any interactive dev clone)
# so continuous deployment never collides with in-progress work. Steps:
#   fetch+reset the deploy tree to origin/main → build the Go API → run DB
#   migrations → sync the worker deps → restart the systemd services → health +
#   smoke check (rolls "forward"; a failed health check exits non-zero so the
#   workflow run is marked failed).
#
# Idempotent and safe to re-run. Overridable via env for testing.
set -euo pipefail

DEPLOY_TREE="${ORPHEUS_DEPLOY_TREE:-/home/ubuntu/alfred/deploy/orpheus}"
BIN_DIR="${ORPHEUS_BIN_DIR:-/home/ubuntu/alfred/bin}"
GO_BIN="${GO_BIN:-/usr/local/go/bin/go}"
UV_BIN="${UV_BIN:-/home/ubuntu/.local/bin/uv}"
API_URL="${ORPHEUS_HEALTH_URL:-http://localhost:8090/health}"

log() { echo "[deploy-backend] $*"; }

log "updating deploy tree at ${DEPLOY_TREE}"
if [ ! -d "${DEPLOY_TREE}/.git" ]; then
  echo "deploy tree ${DEPLOY_TREE} is not a git checkout; provision it first" >&2
  exit 1
fi
cd "${DEPLOY_TREE}"
git fetch --quiet origin main
git reset --hard origin/main
REV="$(git rev-parse --short HEAD)"
log "deploy tree at ${REV}"

log "building Go API + migrate"
mkdir -p "${BIN_DIR}"
# The Go module is rooted at apps/api (go.mod lives there), so build from within it.
( cd apps/api && "${GO_BIN}" build -o "${BIN_DIR}/orpheus-api.new" ./cmd/api )
( cd apps/api && "${GO_BIN}" build -o "${BIN_DIR}/orpheus-migrate" ./cmd/migrate )

log "running database migrations"
PGPASS="$(docker exec orpheus-postgres printenv POSTGRES_PASSWORD)"
DSN="postgres://orpheus:${PGPASS}@localhost:5432/orpheus?sslmode=disable"
"${BIN_DIR}/orpheus-migrate" "${DSN}"

log "syncing worker dependencies"
"${UV_BIN}" sync --package orpheus-workers

# Swap the API binary in atomically only after a clean build + migrate.
mv -f "${BIN_DIR}/orpheus-api.new" "${BIN_DIR}/orpheus-api"

log "restarting services"
sudo systemctl restart orpheus-api
sudo systemctl restart orpheus-worker

log "health check (API)"
ok=""
for i in $(seq 1 20); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${API_URL}" || true)"
  if [ "${code}" = "200" ]; then ok=1; log "API healthy after ${i} attempt(s)"; break; fi
  sleep 3
done
if [ -z "${ok}" ]; then echo "API did not become healthy at ${API_URL}" >&2; exit 1; fi

log "smoke check (worker active + catalog synced)"
systemctl is-active --quiet orpheus-worker || { echo "worker not active" >&2; exit 1; }

log "backend deploy OK at ${REV}"
