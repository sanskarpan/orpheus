# Orpheus — Verified Defects & Remediation Tracker

Scope: `orpheus/` only (`adkil/` is out of scope). Every issue below was **verified
against source** (and, where possible, empirically against the running Postgres/NATS/
MinIO/Redis dev stack). Findings from the initial adversarial audit that turned out to
be **false positives** are listed at the bottom so we don't chase them again.

Status legend: ⬜ open · 🟦 in progress · ✅ fixed+tested

---

## Critical / High (functional breakage or externally-triggerable security)

### ISSUE-1 ✅ — Upload completion is permanently broken (THREE root causes)
- **Severity:** High · **Category:** Data/Logic · **Location:** `apps/api/internal/handlers/uploads.go` `Create`/`Complete`
- **Problem:** The whole Create→upload→Complete flow was broken by three independent bugs (all found while writing the round-trip test):
  1. `Create` never persisted `s3_bucket`/`s3_key`/`s3_upload_id`, so `Complete`'s `SELECT … Scan(&string)` hit NULL → 500, and the real multipart key/uploadID were lost.
  2. **ISSUE-17:** `Complete` inserted `probe_status = 'ok'`, but the enum is `pending/running/completed/failed` → enum cast error → 500.
  3. **ISSUE-18:** `Complete`'s closure called `tx.Commit()` itself, then `WithTenant` committed again → "tx is closed" → 500.
- **Fix:** Persist the S3 columns in Create; use `probe_status='pending'`; let `WithTenant` own the tx (closure uses `dbtx.Exec`, returns nil).
- **Test:** ✅ `TestUploadCreateComplete_RoundTrip` — real Create→PUT part→Complete against MinIO, asserts columns persisted + artifact returned.

### ISSUE-2 ✅ — `GET /v1/uploads` and `/v1/artifacts` 500 on every call
- **Severity:** High · **Category:** Logic · **Location:** `uploads.go` `List` (~L341/355), `artifacts.go` `List` (~L138/153)
- **Problem:** `args` is seeded as `{OrgID, limit+1}` (2 elems) with `argIdx=2`, then `limit+1` is appended **again** at the end. With no filters the query has placeholders `$1,$2` but 3 args → pgx arg-count mismatch → 500. With a cursor, `$2` binds the int `limit+1` into `created_at < $2` → type error. List is unusable.
- **Fix:** Seed `args := []any{p.OrgID}`; append `limit+1` exactly once at the end.
- **Test:** List handler test (no filters, and with cursor) asserting 200 + correct rows.

### ISSUE-4 ✅ — Webhook SSRF (registration + delivery)
- **Severity:** High · **Category:** Security · **Location:** `handlers/webhooks.go` `validateCreate`/`validateUpdate` (~L586/611); delivery client `webhooks/delivery.go` `post` (~L326)
- **Problem:** URL validation only checks `scheme == "https"`. A tenant can register `https://169.254.169.254/…` (cloud metadata), `https://localhost/…`, RFC1918 hosts, etc. The delivery client uses the default redirect policy (follows redirects with no re-validation → DNS-rebind / redirect-to-internal). Response body is stored in `webhook_deliveries.response_body` (exfil).
- **Fix:** Add host/IP validation rejecting loopback/link-local/RFC1918/ULA/metadata; block non-public hostnames; set `CheckRedirect` to re-validate each hop; validate at both registration and delivery time.
- **Test:** Unit tests asserting internal URLs are rejected at create and at delivery.

---

## Medium (authz gaps, correctness, robustness)

### ISSUE-3 ✅ — Audit logging is 100% broken (TWO root causes) + ISSUE-19/20
- **Severity:** High (was mis-scoped Medium) · **Category:** Data/Contract/Security · **Location:** `audit/middleware.go`
- **Problem:** Audit was silently writing **zero rows** for two independent reasons:
  1. **Enum inversion (ISSUE-3):** middleware emitted `verb.resource` (`create.upload`) but the enum is `resource.verb` (`upload.create`) → every `::audit_action` cast failed.
  2. **RLS rejection (ISSUE-20):** `Record` ran the INSERT on the bare pool with no tenant GUC, but `audit_log` has FORCE RLS with insert policy `is_service_role() OR org_id = current_org_id()` → **every** audit insert (handler-level too, contrary to the initial audit's claim) was rejected. Handlers discard the error (`_ =`), so it was invisible.
  3. **ISSUE-19:** the workflow handler audited `workflow.create`, which wasn't even in the enum.
- **Fix:** buildAction emits normalized `resource.verb`; middleware skips (not fails) non-enum shapes; `Record` wraps the INSERT in `WithTenant(e.OrgID, …)`; new migration `0004_audit_workflow_actions.sql` adds `workflow.{create,update,cancel}`.
- **Test:** ✅ `TestBuildAction` (resource.verb), ✅ `TestRecord_WritesRowUnderRLS` (real INSERT persisted + read back under RLS).
- **Note:** middleware + handler now both write for simple routes (2 rows: one access-log-style w/ IP/UA/status, one semantic) — acceptable, documented.

### ISSUE-5 ✅ — Missing `org_id` claim collapses tenants into the zero-UUID org
- **Severity:** Medium · **Category:** Security · **Location:** `auth/keycloak.go` `DefaultOrgID` (L38, L81, ~L107)
- **Problem:** A valid Keycloak token without an `org_id` claim is assigned the all-zeros org. Every such user shares one RLS tenant and can read/write each other's data. Fails *usable* instead of *closed*.
- **Fix:** Reject tokens lacking `org_id` (401). Keep an explicit, config-gated non-prod escape hatch if bootstrap requires it.
- **Test:** Verifier test asserting a token without `org_id` is rejected (prod), allowed only under the dev flag.

### ISSUE-6 ✅ — API-key scopes are never enforced
- **Severity:** Medium · **Category:** Security · **Location:** all `handlers/*`; key creation `api_keys.go` (~L105); no `RequireScope` anywhere
- **Problem:** `Principal.Scopes/Roles` are populated but no handler/middleware checks them, and `Create` stores `req.Scopes` verbatim (a narrow key can mint a `["*"]` key). The OpenAPI scope enum is decorative. (Cross-org is still blocked by RLS; this is within-org authz.)
- **Fix:** Add a `RequireScope` middleware; deny by default on mutating routes; on key creation forbid granting scopes the caller doesn't hold and reject unknown scopes.
- **Test:** Middleware test: read-only scope → 403 on a write route; subset check on key creation.

### ISSUE-7 ✅ — Outbox claim not transactional → double-publish
- **Severity:** Medium · **Category:** Race · **Location:** `outbox/publisher.go` `tick` (~L118)
- **Problem:** `FOR UPDATE SKIP LOCKED` runs on `p.DB.Query` (pool auto-commit, **no enclosing tx**), so row locks release before publish. Two concurrent publishers can claim the same rows and double-publish.
- **Fix:** Wrap claim→publish→mark in one `pgx.Tx`, or `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED) RETURNING`.
- **Test:** Concurrent-publisher test asserting each event published exactly once.

### ISSUE-8 ✅ — Idempotency key stored after side effects; scope lacks method+path
- **Severity:** Medium · **Category:** Race/Logic · **Location:** `idempotency/middleware.go` (~L105/124/155)
- **Problem:** The key row is written only *after* the handler runs, so two concurrent same-key requests both execute (double-apply). Scope is `(org_id, key)` only — same key+body across two endpoints replays the wrong response.
- **Fix:** Reserve a `pending` row atomically (`INSERT … ON CONFLICT DO NOTHING`) *before* running the handler; loser polls/409s. Include method+path in the uniqueness scope.
- **Test:** Concurrent same-key test → one execution; cross-endpoint same-body → not a replay.

### ISSUE-9 ✅ — Prod secrets fall back to dev defaults with no validation
- **Severity:** Medium · **Category:** Security · **Location:** `config/config.go` (L35 `sslmode=disable`, L48-49 `orpheus`/`orpheus-dev-secret`)
- **Problem:** Secrets have dev-value defaults applied in all envs; `Load()` never validates them in prod. A prod deploy that forgets to set `S3_SECRET_KEY`/`DATABASE_URL` boots with the well-known dev secret and TLS-disabled DB.
- **Fix:** In `Load()`, when `IsProd()`, reject any secret still equal to its dev default and require a non-`disable` sslmode. Normalize env (`prod`/`production`).
- **Test:** `Load` test: prod + default secret → error; prod + real secret → ok.

### ISSUE-11 ✅ — Bad query params 500 instead of 400 (enum/uuid/cursor)
- **Severity:** Medium · **Category:** Contract · **Location:** `jobs.go` (~L337 `status`), `system.go` (~L134 `action`, L139 `actor_id`), list `cursor` params
- **Problem:** `status`/`action`/`actor_id`/`cursor` are cast directly in SQL (`$n::job_status`, `::uuid`, `created_at < $n`). Invalid input → Postgres cast error → 500 (and leaks DB error text in some responses).
- **Fix:** Validate enum membership / `uuid.Parse` / RFC3339 cursor before querying; return 400. Return generic error text; log details.
- **Test:** `?status=bogus` → 400; `?actor_id=x` → 400; `?cursor=notatime` → 400.

### ISSUE-13 ✅ — Worker slice/transcribe params unbounded (NaN/0/huge → DoS/crash)
- **Severity:** Medium · **Category:** Data/DoS · **Location:** `workers/.../processors/slice.py`, `transcribe.py`; `ffmpeg.py`
- **Problem:** `start_seconds`/`end_seconds`/`chunk_seconds` come from untrusted `jobs.params` unbounded. `float("nan")` bypasses `end<=start`; `chunk_seconds=0` → `ZeroDivisionError`; tiny `chunk_seconds` → millions of ffmpeg/whisper invocations.
- **Fix:** Reject non-finite (`math.isfinite`); enforce `0 <= start < end <= max`; clamp/cap `chunk_seconds` and total chunk count; overall job timeout.
- **Test:** pytest: NaN/0/negative/huge params rejected cleanly (job failed, no runaway).

### ISSUE-14 ✅ — Worker reprocessing double-uploads (non-idempotent)
- **Severity:** Medium · **Category:** Data · **Location:** `workers/.../processors/slice.py`; `worker.py` nak→redelivery
- **Problem:** On redelivery, `slice` generates a fresh `uuid4()` and inserts a new artifact + S3 object each run → duplicates. No "already processed" guard.
- **Fix:** Derive deterministic slice id/key from `job_id`; `INSERT … ON CONFLICT DO NOTHING`; short-circuit if job already completed.
- **Test:** pytest: processing the same job twice yields one artifact.

---

## Low (hardening / defense-in-depth)

### ISSUE-10 ✅ — Rate limiter fails open on Redis error
- **Severity:** Low · **Category:** Security · **Location:** `ratelimit/middleware.go` (~L53)
- **Problem:** Any Redis error → request allowed. An attacker inducing Redis pressure bypasses limits.
- **Fix:** Make fail-open/closed configurable; fail closed for sensitive routes in prod.
- **Test:** Middleware test with a broken limiter asserting configured behavior.

### ISSUE-12 ✅ — App should connect as a NOSUPERUSER/NOBYPASSRLS role
- **Severity:** Low · **Category:** Security (defense-in-depth) · **Location:** `docker-compose.yml` (`POSTGRES_USER: orpheus`), `config.go` DSN
- **Note:** RLS `FORCE` **is** correctly applied to all tenant tables (the audit's "missing FORCE" claim was a false positive — the DDL uses `FORCE  ROW LEVEL SECURITY` with a double space). So the table *owner* is subject to RLS. The only residual risk is a *superuser* app role (stock `POSTGRES_USER` is a superuser and bypasses RLS even with FORCE).
- **Fix:** Provision a dedicated `NOSUPERUSER NOBYPASSRLS` app role for the request path; keep migrations/admin on a separate owner. Document + a startup assertion that `current_user` is non-super/non-bypassrls in prod.
- **Test:** Startup/health check asserts `rolsuper=f, rolbypassrls=f` in prod.

### ISSUE-16 ✅ — `GET /v1/artifacts/{id}` 500s for any unprobed artifact
- **Severity:** Medium · **Category:** Data · **Location:** `artifacts.go` `Get`/`List` scan
- **Problem:** `codec`/`duration_seconds`/`sample_rate`/`channels` are NULL until the probe worker runs, but were scanned into non-pointer `string`/`float64`/`int` → NULL-scan error → 500 (the docstring even promised zero values). Also, a malformed `{id}` path param was cast straight into `WHERE id = $1` (`uuid` column) → cast error → 500.
- **Fix:** Scan probe fields through nullable pointer holders (`derefStr/Float/Int`); add a shared `uuidParam` helper that 404s malformed ids before the query. Applied `uuidParam` across all by-id handlers (jobs, uploads, webhooks, api_keys, workflows) → covers ISSUE-11's uuid half.
- **Test:** ✅ `TestArtifactGet_UnprobedReturnsZeroValues`, and `WrongOrgIs404` now genuinely exercises RLS (harness split into service seed pool vs tenant SUT pool).

### ISSUE-15 ✅ — Sliced/converted output not validated before upload
- **Severity:** Low · **Category:** Data · **Location:** `workers/.../processors/slice.py` (~L59)
- **Problem:** ffmpeg `-c copy` can exit 0 with a truncated/0-byte file; it's uploaded + recorded as a completed artifact with no re-probe.
- **Fix:** Assert `size > 0` (and optionally re-probe) before upload; fail the job otherwise.
- **Test:** pytest asserting a 0-byte output fails the job.

---

## Verified FALSE POSITIVES from the initial audit (do NOT act on these)

- **"RLS missing FORCE / non-functional":** FALSE. All 17 tenant tables have `FORCE ROW LEVEL SECURITY` (written with a double space, which fooled a grep). RLS load-bearing test passes on a fresh-migrated DB.
- **"Connection-pool `app.is_service` GUC leak":** Not reachable in prod. `SET app.is_service='true'` (session scope) exists only in **test** setup; the production API pool never sets it, and the Python worker uses `SET LOCAL` (tx scope).
- **"Worker path traversal via `job_id`":** Not exploitable. `job_id` is a parameterized value against a `uuid` column — a non-UUID errors at the DB before any filepath use; `Path(s3_key).suffix` yields only an extension.
- **"Worker cross-tenant S3 access":** Not reachable. Job creation verifies artifact ownership (`jobs.go:155`) before the worker ever sees the `artifact_id`.
- **"Subject double-prefix breaks routing":** Cosmetic only. Published `adkil.job.job.queued` still matches the worker's `adkil.job.>` subscription; naming is ugly but functional.
