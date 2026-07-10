-- +goose Up
-- ============================================================================
-- 0001_init.sql — Phase 1 schema for the Orpheus API.
--
-- Tenancy model:
--   Every domain table carries an `org_id` (or `id` for `organizations`
--   itself) and is guarded by a row-level-security policy that scopes reads
--   and writes to the org currently in `app.current_org_id`. A separate
--   policy allows the service role (`app.is_service = 'true'`) to bypass
--   the tenant check, used by background workers and admin tooling.
--
-- RLS is forced on every table (FORCE ROW LEVEL SECURITY) so even the
-- table owner is subject to the policies. This is the only way to make
-- RLS load-bearing — otherwise superusers/owners silently bypass it.
-- ============================================================================

-- ----------------------------------------------------------------------------
-- Extensions
-- ----------------------------------------------------------------------------
-- pgcrypto: gen_random_uuid() is also in core since PG 13, but we keep pgcrypto
--           for digest()/hmac() helpers used by the API key verifier.
-- pg_partman: deferred — will drive partitioning of jobs/audit_log/outbox later.
-- pg_cron:    deferred — will drive scheduled cleanup of expired upload
--             sessions and idempotency keys.
--
-- pg_partman and pg_cron are not in the stock postgres:16-alpine image, so
-- we wrap them in a small helper that emits a NOTICE and continues when the
-- extension is missing. Production deployments should rebuild the image
-- with the relevant packages (postgresql-16-partman, postgresql-16-cron) so
-- the NOTICE is replaced by CREATE EXTENSION.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- +goose StatementBegin
DO $$
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS pg_partman;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pg_partman unavailable, skipping: %', SQLERRM;
    END;
    BEGIN
        CREATE EXTENSION IF NOT EXISTS pg_cron;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pg_cron unavailable, skipping: %', SQLERRM;
    END;
END $$;
-- +goose StatementEnd

-- ----------------------------------------------------------------------------
-- Enums
-- ----------------------------------------------------------------------------
CREATE TYPE job_status AS ENUM (
    'queued',
    'running',
    'completed',
    'failed',
    'canceled'
);

CREATE TYPE job_type AS ENUM (
    'extract-metadata',
    'probe',
    'slice',
    'convert-to-wav',
    'transcribe',
    'diarize',
    'demucs',
    'classify',
    'musicgen',
    'custom'
);

CREATE TYPE upload_status AS ENUM (
    'pending',
    'uploading',
    'complete',
    'failed',
    'expired'
);

CREATE TYPE probe_status AS ENUM (
    'pending',
    'running',
    'completed',
    'failed'
);

CREATE TYPE webhook_status AS ENUM (
    'pending',
    'delivering',
    'delivered',
    'failed',
    'exhausted'
);

CREATE TYPE idempotency_status AS ENUM (
    'in_progress',
    'completed',
    'failed'
);

CREATE TYPE processor_tier AS ENUM (
    'cpu_tiny',
    'cpu_small',
    'cpu_medium',
    'cpu_large',
    'gpu_a10g',
    'gpu_a100'
);

CREATE TYPE actor_type AS ENUM (
    'user',
    'apikey',
    'system'
);

CREATE TYPE audit_action AS ENUM (
    -- Organization lifecycle
    'org.create',
    'org.update',
    'org.delete',
    -- User lifecycle
    'user.invite',
    'user.join',
    'user.leave',
    'user.update',
    'user.remove',
    -- API key lifecycle
    'apikey.create',
    'apikey.update',
    'apikey.revoke',
    -- Upload lifecycle
    'upload.create',
    'upload.complete',
    'upload.abort',
    'upload.expire',
    -- Artifact lifecycle
    'artifact.create',
    'artifact.update',
    'artifact.delete',
    -- Job lifecycle
    'job.create',
    'job.cancel',
    'job.retry',
    'job.update',
    -- Webhook lifecycle
    'webhook.create',
    'webhook.update',
    'webhook.delete',
    'webhook.deliver',
    'webhook.delivery_fail',
    'webhook.delivery_exhausted',
    -- Billing
    'billing.plan_change',
    'billing.payment',
    'billing.refund',
    -- Auth
    'auth.login',
    'auth.logout',
    'auth.token_refresh',
    -- RBAC
    'rbac.role_grant',
    'rbac.role_revoke',
    -- Settings
    'settings.update'
);

-- ----------------------------------------------------------------------------
-- Helper function
-- ----------------------------------------------------------------------------
-- Returns the current tenant org id from the session setting, or NULL if
-- it has not been set. NULL is intentional: `org_id = NULL` is unknown
-- (not true), so RLS denies when the setting is missing.
CREATE OR REPLACE FUNCTION current_org_id() RETURNS uuid
    LANGUAGE sql
    STABLE
AS $$
    SELECT NULLIF(current_setting('app.current_org_id', true), '')::uuid
$$;

CREATE OR REPLACE FUNCTION is_service_role() RETURNS boolean
    LANGUAGE sql
    STABLE
AS $$
    SELECT coalesce(current_setting('app.is_service', true), 'false') = 'true'
$$;

-- ============================================================================
-- Tables
-- ============================================================================

-- organizations --------------------------------------------------------------
CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    slug       text        NOT NULL UNIQUE,
    plan       text        NOT NULL DEFAULT 'free',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- users ----------------------------------------------------------------------
CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email      text        NOT NULL,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- org_members (composite PK) -------------------------------------------------
CREATE TABLE org_members (
    org_id    uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id   uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role      text        NOT NULL DEFAULT 'member',
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

-- api_keys -------------------------------------------------------------------
CREATE TABLE api_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name          text        NOT NULL,
    hashed_secret text        NOT NULL,
    prefix        text        NOT NULL,
    scopes        text[]      NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);

-- upload_sessions ------------------------------------------------------------
CREATE TABLE upload_sessions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid           NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id       uuid           REFERENCES users(id) ON DELETE SET NULL,
    filename      text           NOT NULL,
    content_type  text           NOT NULL,
    size_bytes    bigint         NOT NULL,
    status        upload_status  NOT NULL DEFAULT 'pending',
    created_at    timestamptz    NOT NULL DEFAULT now(),
    expires_at    timestamptz    NOT NULL,
    completed_at  timestamptz
);

-- upload_parts (composite PK on session + part) ------------------------------
CREATE TABLE upload_parts (
    upload_session_id uuid        NOT NULL REFERENCES upload_sessions(id) ON DELETE CASCADE,
    part_number       integer     NOT NULL,
    etag              text        NOT NULL,
    size_bytes        bigint      NOT NULL,
    uploaded_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (upload_session_id, part_number)
);

-- artifacts ------------------------------------------------------------------
CREATE TABLE artifacts (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    upload_session_id uuid         REFERENCES upload_sessions(id) ON DELETE SET NULL,
    s3_bucket        text         NOT NULL,
    s3_key           text         NOT NULL,
    sha256           text         NOT NULL,
    size_bytes       bigint       NOT NULL,
    content_type     text         NOT NULL,
    codec            text,
    duration_seconds numeric(12, 4),
    sample_rate      integer,
    channels         integer,
    probe_status     probe_status NOT NULL DEFAULT 'pending',
    created_at       timestamptz  NOT NULL DEFAULT now()
);

-- jobs -----------------------------------------------------------------------
CREATE TABLE jobs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id      uuid        REFERENCES users(id) ON DELETE SET NULL,
    artifact_id  uuid        REFERENCES artifacts(id) ON DELETE SET NULL,
    job_type     job_type    NOT NULL,
    params       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    result       jsonb,
    status       job_status  NOT NULL DEFAULT 'queued',
    priority     smallint    NOT NULL DEFAULT 0,
    attempts     smallint    NOT NULL DEFAULT 0,
    max_retries  smallint    NOT NULL DEFAULT 3,
    cost_usd     numeric(12, 6),
    started_at   timestamptz,
    completed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    version      integer     NOT NULL DEFAULT 1
);

-- webhook_endpoints ----------------------------------------------------------
CREATE TABLE webhook_endpoints (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    url              text        NOT NULL,
    secret           text        NOT NULL,
    description      text        NOT NULL DEFAULT '',
    subscribed_events text[]     NOT NULL DEFAULT '{}',
    active           boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- webhook_deliveries ---------------------------------------------------------
CREATE TABLE webhook_deliveries (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid            NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    endpoint_id     uuid            NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_type      text            NOT NULL,
    event_id        uuid            NOT NULL,
    payload         jsonb           NOT NULL,
    response_status integer,
    response_body   text,
    attempt_count   smallint        NOT NULL DEFAULT 0,
    max_attempts    smallint        NOT NULL DEFAULT 5,
    status          webhook_status  NOT NULL DEFAULT 'pending',
    next_retry_at   timestamptz     NOT NULL DEFAULT now(),
    delivered_at    timestamptz,
    created_at      timestamptz     NOT NULL DEFAULT now()
);

-- outbox ---------------------------------------------------------------------
CREATE TABLE outbox (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    aggregate_type text        NOT NULL,
    aggregate_id   text        NOT NULL,
    event_type     text        NOT NULL,
    payload        jsonb       NOT NULL,
    headers        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz
);

-- audit_log ------------------------------------------------------------------
CREATE TABLE audit_log (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id       uuid          REFERENCES users(id) ON DELETE SET NULL,
    actor_type    actor_type    NOT NULL,
    action        audit_action  NOT NULL,
    resource_type text          NOT NULL,
    resource_id   text          NOT NULL,
    ip            inet,
    user_agent    text,
    request_id    text,
    metadata      jsonb         NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz   NOT NULL DEFAULT now()
);

-- idempotency_keys -----------------------------------------------------------
CREATE TABLE idempotency_keys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid                NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key             text                NOT NULL,
    request_hash    text                NOT NULL,
    response_status integer,
    response_body   jsonb,
    status          idempotency_status  NOT NULL DEFAULT 'in_progress',
    created_at      timestamptz         NOT NULL DEFAULT now(),
    expires_at      timestamptz         NOT NULL,
    UNIQUE (org_id, key)
);

-- processors (global catalog, NOT org-scoped) -------------------------------
CREATE TABLE processors (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text            NOT NULL UNIQUE,
    display_name     text            NOT NULL,
    description      text            NOT NULL DEFAULT '',
    input_schema     jsonb           NOT NULL DEFAULT '{}'::jsonb,
    output_schema    jsonb           NOT NULL DEFAULT '{}'::jsonb,
    tier             processor_tier  NOT NULL,
    timeout_seconds  integer         NOT NULL,
    max_retries      smallint        NOT NULL DEFAULT 3,
    cost_per_job_usd numeric(12, 6)  NOT NULL DEFAULT 0
);

-- processor_versions (global catalog) ---------------------------------------
CREATE TABLE processor_versions (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    processor_id      uuid        NOT NULL REFERENCES processors(id) ON DELETE CASCADE,
    version           text        NOT NULL,
    model_id          text        NOT NULL,
    model_version_id  text        NOT NULL,
    license_gates     text[]      NOT NULL DEFAULT '{}',
    slo_p95_seconds   numeric(10, 3),
    slo_p99_seconds   numeric(10, 3),
    manifest          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    deprecated_at     timestamptz,
    UNIQUE (processor_id, version)
);

-- ============================================================================
-- Indexes
-- ============================================================================
-- organizations
CREATE INDEX organizations_deleted_at_idx
    ON organizations (deleted_at)
    WHERE deleted_at IS NULL;

-- users
CREATE UNIQUE INDEX users_org_id_email_idx
    ON users (org_id, email);

-- api_keys
CREATE INDEX api_keys_org_id_prefix_idx
    ON api_keys (org_id, prefix);
CREATE INDEX api_keys_hashed_secret_idx
    ON api_keys (hashed_secret);
CREATE INDEX api_keys_revoked_at_idx
    ON api_keys (revoked_at)
    WHERE revoked_at IS NULL;

-- upload_sessions
CREATE INDEX upload_sessions_org_id_status_created_at_idx
    ON upload_sessions (org_id, status, created_at);
CREATE INDEX upload_sessions_expires_at_idx
    ON upload_sessions (expires_at)
    WHERE status = 'pending';

-- artifacts
CREATE UNIQUE INDEX artifacts_org_id_s3_key_idx
    ON artifacts (org_id, s3_key);
CREATE INDEX artifacts_sha256_idx
    ON artifacts (sha256);

-- jobs
CREATE INDEX jobs_org_id_status_created_at_idx
    ON jobs (org_id, status, created_at);
CREATE INDEX jobs_org_id_job_type_created_at_idx
    ON jobs (org_id, job_type, created_at);
CREATE INDEX jobs_artifact_id_idx
    ON jobs (artifact_id)
    WHERE artifact_id IS NOT NULL;

-- webhook_deliveries
CREATE INDEX webhook_deliveries_status_next_retry_at_idx
    ON webhook_deliveries (status, next_retry_at)
    WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_id_created_at_idx
    ON webhook_deliveries (endpoint_id, created_at);

-- outbox
CREATE INDEX outbox_published_at_idx
    ON outbox (published_at)
    WHERE published_at IS NULL;

-- audit_log
CREATE INDEX audit_log_org_id_created_at_idx
    ON audit_log (org_id, created_at);
CREATE INDEX audit_log_resource_type_resource_id_created_at_idx
    ON audit_log (resource_type, resource_id, created_at);

-- idempotency_keys
CREATE INDEX idempotency_keys_expires_at_idx
    ON idempotency_keys (expires_at);

-- ============================================================================
-- Row-level security
-- ============================================================================
-- We enable RLS on every table and FORCE it so the table owner is still
-- subject to the policies. Without FORCE, the application role connecting
-- with the migration would bypass RLS, defeating the purpose.
--
-- Each table gets four policies (SELECT / INSERT / UPDATE / DELETE) so we
-- can later tighten individual operations without rewriting the rule set.
-- All policies include a service-role bypass via is_service_role() — the
-- service role is set with `SET app.is_service = 'true'` on a connection
-- before performing admin or cross-tenant work (job dispatcher, audit
-- log compaction, etc.).
-- ============================================================================

-- organizations (tenant column is `id`, not `org_id`) -------------------------
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE  ROW LEVEL SECURITY;

CREATE POLICY organizations_tenant_select ON organizations
    FOR SELECT
    USING (is_service_role() OR id = current_org_id());

CREATE POLICY organizations_tenant_insert ON organizations
    FOR INSERT
    WITH CHECK (is_service_role() OR id = current_org_id());

CREATE POLICY organizations_tenant_update ON organizations
    FOR UPDATE
    USING      (is_service_role() OR id = current_org_id())
    WITH CHECK (is_service_role() OR id = current_org_id());

CREATE POLICY organizations_tenant_delete ON organizations
    FOR DELETE
    USING (is_service_role() OR id = current_org_id());

-- users ----------------------------------------------------------------------
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE  ROW LEVEL SECURITY;

CREATE POLICY users_tenant_select ON users
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY users_tenant_insert ON users
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY users_tenant_update ON users
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY users_tenant_delete ON users
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- org_members ----------------------------------------------------------------
ALTER TABLE org_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_members FORCE  ROW LEVEL SECURITY;

CREATE POLICY org_members_tenant_select ON org_members
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY org_members_tenant_insert ON org_members
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY org_members_tenant_update ON org_members
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY org_members_tenant_delete ON org_members
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- api_keys -------------------------------------------------------------------
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE  ROW LEVEL SECURITY;

CREATE POLICY api_keys_tenant_select ON api_keys
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY api_keys_tenant_insert ON api_keys
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY api_keys_tenant_update ON api_keys
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY api_keys_tenant_delete ON api_keys
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- upload_sessions ------------------------------------------------------------
ALTER TABLE upload_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE upload_sessions FORCE  ROW LEVEL SECURITY;

CREATE POLICY upload_sessions_tenant_select ON upload_sessions
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY upload_sessions_tenant_insert ON upload_sessions
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY upload_sessions_tenant_update ON upload_sessions
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY upload_sessions_tenant_delete ON upload_sessions
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- upload_parts (org-scoped via the parent session) ---------------------------
ALTER TABLE upload_parts ENABLE ROW LEVEL SECURITY;
ALTER TABLE upload_parts FORCE  ROW LEVEL SECURITY;

CREATE POLICY upload_parts_tenant_select ON upload_parts
    FOR SELECT
    USING (
        is_service_role()
        OR EXISTS (
            SELECT 1 FROM upload_sessions s
            WHERE s.id = upload_session_id
              AND s.org_id = current_org_id()
        )
    );

CREATE POLICY upload_parts_tenant_insert ON upload_parts
    FOR INSERT
    WITH CHECK (
        is_service_role()
        OR EXISTS (
            SELECT 1 FROM upload_sessions s
            WHERE s.id = upload_session_id
              AND s.org_id = current_org_id()
        )
    );

CREATE POLICY upload_parts_tenant_update ON upload_parts
    FOR UPDATE
    USING      (is_service_role() OR EXISTS (SELECT 1 FROM upload_sessions s WHERE s.id = upload_session_id AND s.org_id = current_org_id()))
    WITH CHECK (is_service_role() OR EXISTS (SELECT 1 FROM upload_sessions s WHERE s.id = upload_session_id AND s.org_id = current_org_id()));

CREATE POLICY upload_parts_tenant_delete ON upload_parts
    FOR DELETE
    USING (
        is_service_role()
        OR EXISTS (
            SELECT 1 FROM upload_sessions s
            WHERE s.id = upload_session_id
              AND s.org_id = current_org_id()
        )
    );

-- artifacts ------------------------------------------------------------------
ALTER TABLE artifacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifacts FORCE  ROW LEVEL SECURITY;

CREATE POLICY artifacts_tenant_select ON artifacts
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY artifacts_tenant_insert ON artifacts
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY artifacts_tenant_update ON artifacts
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY artifacts_tenant_delete ON artifacts
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- jobs -----------------------------------------------------------------------
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE  ROW LEVEL SECURITY;

CREATE POLICY jobs_tenant_select ON jobs
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY jobs_tenant_insert ON jobs
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY jobs_tenant_update ON jobs
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY jobs_tenant_delete ON jobs
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- webhook_endpoints ----------------------------------------------------------
ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoints FORCE  ROW LEVEL SECURITY;

CREATE POLICY webhook_endpoints_tenant_select ON webhook_endpoints
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY webhook_endpoints_tenant_insert ON webhook_endpoints
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY webhook_endpoints_tenant_update ON webhook_endpoints
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY webhook_endpoints_tenant_delete ON webhook_endpoints
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- webhook_deliveries ---------------------------------------------------------
ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE  ROW LEVEL SECURITY;

CREATE POLICY webhook_deliveries_tenant_select ON webhook_deliveries
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY webhook_deliveries_tenant_insert ON webhook_deliveries
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY webhook_deliveries_tenant_update ON webhook_deliveries
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY webhook_deliveries_tenant_delete ON webhook_deliveries
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- outbox ---------------------------------------------------------------------
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE  ROW LEVEL SECURITY;

CREATE POLICY outbox_tenant_select ON outbox
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY outbox_tenant_insert ON outbox
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY outbox_tenant_update ON outbox
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY outbox_tenant_delete ON outbox
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- audit_log ------------------------------------------------------------------
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE  ROW LEVEL SECURITY;

CREATE POLICY audit_log_tenant_select ON audit_log
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY audit_log_tenant_insert ON audit_log
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

-- audit_log is append-only: no UPDATE/DELETE policies.
-- The FORCE RLS + missing policy combination denies those operations
-- (even for the service role when not flagged), preserving tamper
-- resistance. Admin tooling must use a role without FORCE RLS to purge.

-- idempotency_keys -----------------------------------------------------------
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE  ROW LEVEL SECURITY;

CREATE POLICY idempotency_keys_tenant_select ON idempotency_keys
    FOR SELECT
    USING (is_service_role() OR org_id = current_org_id());

CREATE POLICY idempotency_keys_tenant_insert ON idempotency_keys
    FOR INSERT
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY idempotency_keys_tenant_update ON idempotency_keys
    FOR UPDATE
    USING      (is_service_role() OR org_id = current_org_id())
    WITH CHECK (is_service_role() OR org_id = current_org_id());

CREATE POLICY idempotency_keys_tenant_delete ON idempotency_keys
    FOR DELETE
    USING (is_service_role() OR org_id = current_org_id());

-- processors (global catalog) ------------------------------------------------
-- Read-public, write-service-only.
ALTER TABLE processors ENABLE ROW LEVEL SECURITY;
ALTER TABLE processors FORCE  ROW LEVEL SECURITY;

CREATE POLICY processors_public_select ON processors
    FOR SELECT
    USING (true);

CREATE POLICY processors_service_insert ON processors
    FOR INSERT
    WITH CHECK (is_service_role());

CREATE POLICY processors_service_update ON processors
    FOR UPDATE
    USING      (is_service_role())
    WITH CHECK (is_service_role());

CREATE POLICY processors_service_delete ON processors
    FOR DELETE
    USING (is_service_role());

-- processor_versions (global catalog) ----------------------------------------
ALTER TABLE processor_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE processor_versions FORCE  ROW LEVEL SECURITY;

CREATE POLICY processor_versions_public_select ON processor_versions
    FOR SELECT
    USING (true);

CREATE POLICY processor_versions_service_insert ON processor_versions
    FOR INSERT
    WITH CHECK (is_service_role());

CREATE POLICY processor_versions_service_update ON processor_versions
    FOR UPDATE
    USING      (is_service_role())
    WITH CHECK (is_service_role());

CREATE POLICY processor_versions_service_delete ON processor_versions
    FOR DELETE
    USING (is_service_role());

-- +goose Down
-- ============================================================================
-- Roll back the entire schema. Order matters: drop policies before tables
-- (so the policy DROP doesn't trip on a missing relation), and drop tables
-- in reverse dependency order.
-- ============================================================================

-- policies (drop first — easier to discover dependency errors here than
-- when the table is dropped)
DROP POLICY IF EXISTS organizations_tenant_select          ON organizations;
DROP POLICY IF EXISTS organizations_tenant_insert          ON organizations;
DROP POLICY IF EXISTS organizations_tenant_update          ON organizations;
DROP POLICY IF EXISTS organizations_tenant_delete          ON organizations;

DROP POLICY IF EXISTS users_tenant_select                  ON users;
DROP POLICY IF EXISTS users_tenant_insert                  ON users;
DROP POLICY IF EXISTS users_tenant_update                  ON users;
DROP POLICY IF EXISTS users_tenant_delete                  ON users;

DROP POLICY IF EXISTS org_members_tenant_select            ON org_members;
DROP POLICY IF EXISTS org_members_tenant_insert            ON org_members;
DROP POLICY IF EXISTS org_members_tenant_update            ON org_members;
DROP POLICY IF EXISTS org_members_tenant_delete            ON org_members;

DROP POLICY IF EXISTS api_keys_tenant_select               ON api_keys;
DROP POLICY IF EXISTS api_keys_tenant_insert               ON api_keys;
DROP POLICY IF EXISTS api_keys_tenant_update               ON api_keys;
DROP POLICY IF EXISTS api_keys_tenant_delete               ON api_keys;

DROP POLICY IF EXISTS upload_sessions_tenant_select        ON upload_sessions;
DROP POLICY IF EXISTS upload_sessions_tenant_insert        ON upload_sessions;
DROP POLICY IF EXISTS upload_sessions_tenant_update        ON upload_sessions;
DROP POLICY IF EXISTS upload_sessions_tenant_delete        ON upload_sessions;

DROP POLICY IF EXISTS upload_parts_tenant_select           ON upload_parts;
DROP POLICY IF EXISTS upload_parts_tenant_insert           ON upload_parts;
DROP POLICY IF EXISTS upload_parts_tenant_update           ON upload_parts;
DROP POLICY IF EXISTS upload_parts_tenant_delete           ON upload_parts;

DROP POLICY IF EXISTS artifacts_tenant_select               ON artifacts;
DROP POLICY IF EXISTS artifacts_tenant_insert               ON artifacts;
DROP POLICY IF EXISTS artifacts_tenant_update               ON artifacts;
DROP POLICY IF EXISTS artifacts_tenant_delete               ON artifacts;

DROP POLICY IF EXISTS jobs_tenant_select                   ON jobs;
DROP POLICY IF EXISTS jobs_tenant_insert                   ON jobs;
DROP POLICY IF EXISTS jobs_tenant_update                   ON jobs;
DROP POLICY IF EXISTS jobs_tenant_delete                   ON jobs;

DROP POLICY IF EXISTS webhook_endpoints_tenant_select      ON webhook_endpoints;
DROP POLICY IF EXISTS webhook_endpoints_tenant_insert      ON webhook_endpoints;
DROP POLICY IF EXISTS webhook_endpoints_tenant_update      ON webhook_endpoints;
DROP POLICY IF EXISTS webhook_endpoints_tenant_delete      ON webhook_endpoints;

DROP POLICY IF EXISTS webhook_deliveries_tenant_select     ON webhook_deliveries;
DROP POLICY IF EXISTS webhook_deliveries_tenant_insert     ON webhook_deliveries;
DROP POLICY IF EXISTS webhook_deliveries_tenant_update     ON webhook_deliveries;
DROP POLICY IF EXISTS webhook_deliveries_tenant_delete     ON webhook_deliveries;

DROP POLICY IF EXISTS outbox_tenant_select                 ON outbox;
DROP POLICY IF EXISTS outbox_tenant_insert                 ON outbox;
DROP POLICY IF EXISTS outbox_tenant_update                 ON outbox;
DROP POLICY IF EXISTS outbox_tenant_delete                 ON outbox;

DROP POLICY IF EXISTS audit_log_tenant_select              ON audit_log;
DROP POLICY IF EXISTS audit_log_tenant_insert              ON audit_log;

DROP POLICY IF EXISTS idempotency_keys_tenant_select       ON idempotency_keys;
DROP POLICY IF EXISTS idempotency_keys_tenant_insert       ON idempotency_keys;
DROP POLICY IF EXISTS idempotency_keys_tenant_update       ON idempotency_keys;
DROP POLICY IF EXISTS idempotency_keys_tenant_delete       ON idempotency_keys;

DROP POLICY IF EXISTS processors_public_select             ON processors;
DROP POLICY IF EXISTS processors_service_insert            ON processors;
DROP POLICY IF EXISTS processors_service_update            ON processors;
DROP POLICY IF EXISTS processors_service_delete            ON processors;

DROP POLICY IF EXISTS processor_versions_public_select     ON processor_versions;
DROP POLICY IF EXISTS processor_versions_service_insert    ON processor_versions;
DROP POLICY IF EXISTS processor_versions_service_update    ON processor_versions;
DROP POLICY IF EXISTS processor_versions_service_delete    ON processor_versions;

-- tables (reverse dependency order)
DROP TABLE IF EXISTS processor_versions;
DROP TABLE IF EXISTS processors;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_endpoints;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS upload_parts;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;

-- helper functions
DROP FUNCTION IF EXISTS current_org_id();
DROP FUNCTION IF EXISTS is_service_role();

-- enums
DROP TYPE IF EXISTS audit_action;
DROP TYPE IF EXISTS actor_type;
DROP TYPE IF EXISTS processor_tier;
DROP TYPE IF EXISTS idempotency_status;
DROP TYPE IF EXISTS webhook_status;
DROP TYPE IF EXISTS probe_status;
DROP TYPE IF EXISTS upload_status;
DROP TYPE IF EXISTS job_type;
DROP TYPE IF EXISTS job_status;

-- extensions (leave installed — removing them may break other databases
-- on the same cluster, and we may want to re-run this migration locally)
-- DROP EXTENSION IF EXISTS pg_cron;
-- DROP EXTENSION IF EXISTS pg_partman;
-- DROP EXTENSION IF EXISTS pgcrypto;
