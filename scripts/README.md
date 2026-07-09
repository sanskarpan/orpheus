# `scripts/`

Operational and dev utility scripts.

## Phase 0

Empty. Add scripts here as the project grows. Convention: small,
self-contained, parameterized via env vars or CLI args.

## Example future scripts

- `bootstrap.sh` — first-time setup (idempotent; safe to re-run)
- `seed-dev.sh` — load dev fixtures
- `release.sh` — tag, build, sign, push
- `db-snapshot.sh` — take a Postgres snapshot
- `restore-from-snapshot.sh` — restore from a snapshot
