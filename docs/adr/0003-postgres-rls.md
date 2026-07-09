# ADR-0003: PostgreSQL 16 with Row-Level Security for Multi-Tenancy

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

We are multi-tenant from day one. Three viable strategies:
1. Schema-per-tenant
2. Database-per-tenant
3. Row-level (shared schema, `tenant_id` column, RLS policies)

## Decision

Use **row-level security**. Every table has a `tenant_id` column. RLS
policies enforce isolation. The application sets
`SET LOCAL app.current_org_id = <org_id>` per request, per transaction.

We pin to **PostgreSQL 16** (current LTS) with `pg_partman`, `pg_cron`,
`pgvector` (future-proofing), and `wal2json` (for CDC).

## Consequences

- Single database, single connection pool, easy cross-tenant analytics.
- Defense in depth: RLS catches mistakes at the database layer; the
  application also has an explicit tenant filter.
- Risk: one missed RLS policy = data leak. Mitigation: a per-table test
  that asserts cross-tenant queries return zero rows.
- Reversibility: medium. Migrating to schema-per-tenant later is a known
  path if needed.

## Alternatives Considered

- **Schema-per-tenant** — operational complexity (migrations across thousands
  of schemas).
- **Database-per-tenant** — too expensive for the SaaS margin target.
- **MongoDB** — not needed; we don't have a true document model.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §3.3 (Database), §9 (Database
  Design)
- PostgreSQL docs, [Row Security Policies](https://www.postgresql.org/docs/16/ddl-rowsecurity.html)
