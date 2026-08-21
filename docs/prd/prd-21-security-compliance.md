# PRD: Security & Compliance Hardening

**Status:** Proposed · **Priority:** P0 · **Epic:** Security & Compliance · **Related issues:** #427, #428, #431, #432, #375

## 1. Summary

Orpheus already ships the hard parts of a multi-tenant security foundation: FORCE-RLS on
Postgres, an argon2id API-key layer, an append-only audit log, and a GDPR erasure saga. But the
identity and trust boundaries around that foundation are still dev-grade. Keycloak JWT
verification exists but no realm is configured to emit the claims it depends on; the dashboard
authenticates users against a local SQLite file; the rate limiter fails open on Redis loss; the
streaming WebSocket accepts any Origin; secrets are plain env vars with committed dev defaults;
and the marketplace promotes third-party processors with no code sandbox. This PRD closes those
gaps and lays the groundwork for a SOC2/HIPAA compliance program (SSO/SCIM, BAA, audit) plus a
zero-data-retention toggle for regulated tenants.

## 2. Motivation & goals

- **Make Keycloak JWT real (#427):** the verifier is written and correct but non-functional in
  practice — no realm mapper emits `org_id` or the `platform:admin` realm role, so JWT auth
  cannot be turned on in prod without every token collapsing to a shared org or being rejected.
- **Replace dev-grade dashboard auth (#428):** `apps/web/lib/accounts.ts` is a local SQLite user
  store; it cannot be the identity system for a product selling to enterprises.
- **Decide the rate-limiter fail-open policy:** a Redis outage today silently removes all quota
  enforcement. That is a deliberate availability trade-off, but it must be a per-environment
  decision, not a default.
- **Ship enterprise trust:** SSO/SCIM, a BAA path, and a SOC2/HIPAA-aligned audit program (#431)
  are prerequisites for the regulated tenants (healthcare, call centers) the transcription
  product targets.
- **Close code-execution and origin holes:** marketplace sandbox (#432), WS `CheckOrigin` (prod),
  real secrets management, and a zero-data-retention toggle (#375).
- **Non-goals:** rewriting RLS/erasure/audit (they stay); FedRAMP; a full SIEM build.

## 3. Current state in Orpheus

- **Keycloak verifier:** `apps/api/internal/auth/keycloak.go:97` (`Verify`) reads a custom
  `org_id` claim (`:113`) and `realm_access.roles` (`realmRoles`, `:150`). In non-prod a missing
  `org_id` falls back to `DefaultOrgID` (`:43`, `:118`); in prod it is rejected (`:115`). The doc
  comment (`:21-31`) states the realm must be configured to emit `org_id` and roles — that
  configuration does not exist yet. `platform:admin` is a recognized scope/role
  (`auth/principal.go:58`) gated by `RequireRole` (`principal.go:133`) and used on real routes
  (`server/server.go:252`, `:269`).
- **Dashboard auth:** `apps/web/lib/accounts.ts` — SQLite (`better-sqlite3`) at `.data/accounts.db`,
  scrypt password hashing (`:56`), AES-256-GCM org-key encryption with a defaulted `SESSION_SECRET`
  (`:73`), and a "first account or configured email is admin" policy (`resolvePlatformAdmin`,
  `:108`).
- **Rate limiter:** `internal/ratelimit/limiter.go` (sliding-window Lua, atomic). Middleware
  `ratelimit/middleware.go:24` has a `FailClosed` flag (default false → fail open, `:74`); the
  fail-closed branch already returns 503 (`:66-72`). The mechanism exists; the policy/default and
  wiring do not.
- **Streaming WS:** `handlers/streaming_ws.go:108` — `CheckOrigin` returns `true` for all origins;
  auth is a 2-minute HMAC token (`MintStreamToken`, `:64`; TTL `:36`) with a defaulted secret
  (`streamTokenSecret`, `:47`).
- **Marketplace:** `handlers/marketplace.go` — `Review` (`:154`) promotes a community submission
  straight into the `processors` catalog (`:194`) with only metadata; no code, no sandbox, no
  scan.
- **Secrets:** `internal/config/config.go` — `envconfig` env vars with committed dev defaults
  (`S3_SECRET_KEY` `:57`, `SESSION_SECRET`, `ORPHEUS_STREAM_TOKEN_SECRET`); `RequireProdSecrets`
  (`:124`) fails loud only in prod.
- **Foundations to preserve:** FORCE-RLS + `WithTenant` (used everywhere, e.g.
  `marketplace.go:56`), audit `Recorder` (`internal/audit/middleware.go:74`), erasure saga
  (`internal/erasure/service.go`).

## 4. Proposed design

**4.1 Keycloak realm configuration (#427).** Deliver realm-as-code (a `infra/keycloak/realm.json`
import or a Terraform `keycloak` provider module) that (a) adds a **User Attribute → Token Claim
mapper** emitting `org_id` (string, on access token, from the user's `org_id` attribute), and
(b) defines a `platform:admin` **realm role** plus a role mapper so `realm_access.roles` carries
it. No Go change required — `Verify` already reads both. Add an integration test that mints a
token via the configured realm and asserts `Principal.OrgID`/`Roles`. Provision `org_id` at user
creation via the existing onboarding provisioning path.

**4.2 Real IdP for the dashboard (#428).** Move dashboard identity onto the same Keycloak realm
(OIDC Authorization Code + PKCE) instead of the SQLite store. The BFF exchanges the code, stores
the encrypted org key keyed by the Keycloak `sub`/`org_id`, and drops the local password table.
`resolvePlatformAdmin` (`accounts.ts:108`) is replaced by the `platform:admin` realm role.
Keep `accounts.ts` behind a `DASHBOARD_AUTH=local|oidc` switch for local dev only.

**4.3 Rate-limiter policy.** Make `FailClosed` config-driven (`RATELIMIT_FAIL_CLOSED`, default
**false** in dev, **true**-eligible in prod) and add a **degraded** mode as the recommended
prod default: on Redis error, apply a conservative in-process per-instance fallback cap (a
local token bucket sized to `FreeLimit`) instead of a binary open/closed. Emit a
`orpheus_ratelimit_backend_errors_total` counter (new collector in `internal/metrics/metrics.go`)
so the outage is alertable. Document the trade-off in the middleware comment.
**Scale & backpressure.** The degraded local bucket is per-instance, so aggregate allowance
scales with replica count — acceptable as a conservative cap, and it recovers automatically when
Redis returns (no manual intervention). A Redis-error circuit breaker avoids hammering a dead
backend on every request (bounded retry, half-open probe). **Backward-compatible on-wire:** the
same `429` + `Retry-After`/`X-RateLimit-*` headers are returned in every mode, so clients see no
protocol change whether enforcement is Redis-backed, degraded, or fail-closed.

**4.4 SOC2/HIPAA + SSO/SCIM + audit program (#431).**
- **SSO:** covered by 4.2 (OIDC); add SAML broker support in Keycloak for enterprise IdPs.
- **SCIM:** a `/scim/v2/Users` + `/Groups` endpoint (new handler) that provisions/deprovisions
  Keycloak users and sets `org_id`; deprovision triggers session revocation.
- **Audit:** extend the existing `audit_log` (`audit/middleware.go`) to cover auth events
  (login, SSO, key mint, admin actions) and add tamper-evidence (per-org hash chain) + export
  API (`GET /v1/audit/export`, gated `audit:read`).
- **BAA/HIPAA:** rides on the zero-data-retention toggle (4.7), KMS-backed secrets (4.6), and the
  erasure saga; produce a controls matrix mapping SOC2 CC/HIPAA §164 to code paths.

**4.5 WS `CheckOrigin` for prod.** Replace the allow-all (`streaming_ws.go:108`) with an
allow-list from config (`ORPHEUS_ALLOWED_WS_ORIGINS`, comma-separated) checked against the
`Origin` header when set; empty allow-list preserves today's behavior only in non-prod. The HMAC
token stays the primary credential; Origin becomes defense-in-depth against CSWSH.

**4.6 Secrets management.** Introduce a `Secrets` provider interface in `internal/config` with
`env` (today) and `kms`/`vault` backends (AWS Secrets Manager / Vault). At startup, prod resolves
`S3_SECRET_KEY`, `SESSION_SECRET`, `ORPHEUS_STREAM_TOKEN_SECRET`, `DODO_WEBHOOK_SECRET`,
`ORPHEUS_MODAL_SHARED_SECRET` from the provider; `RequireProdSecrets` (`config.go:124`) fails if
any is still a dev default. Support per-secret rotation without redeploy for the HMAC/stream
secret (key-id prefix on minted tokens so both old and new verify during rotation).

**4.7 Marketplace sandbox (#432).** Community processors must run untrusted code in isolation.
Gate `Review→approve` (`marketplace.go:192`) behind a submission that references a **container
image + declared resource/network profile**, not free text. Execution runs on Modal in a
locked-down sandbox: no egress by default, read-only FS, CPU/mem/timeout caps from the processor
`tier`, and inputs/outputs passed as signed blobs. Add a static-scan + manifest-review step to
the moderation queue before catalog promotion. Community processors carry `trust_class='community'`
(already set, `:199`) and are opt-in per org.
**Scale, bounded cost & failure modes.** Each sandbox invocation runs under a
`max_containers`/timeout/CPU-mem cap derived from `tier` so a runaway or crypto-mining processor
cannot consume unbounded GPU/CPU — cost is metered per invocation (GPU-seconds/CPU-seconds) and
billed to the invoking org, and a per-org concurrency cap bounds blast radius. A sandbox that OOMs,
times out, or exits non-zero fails only the invoking job (graceful degradation) and increments
`orpheus_marketplace_sandbox_failures_total{processor}`; repeated failures auto-quarantine the
processor from the catalog. **Multi-tenant isolation:** signed-blob I/O plus no-egress guarantees a
community processor can never read another tenant's data or reach the internal network; execution
carries no ambient tenant credentials.

**4.8 Zero-data-retention toggle (#375).** Add an org-level `data_retention_mode` (`standard` |
`zero`). In `zero` mode: job payloads/results/artifacts are never persisted to S3 (streamed
result only), transcript text is dropped after delivery, retention sweeper
(`internal/retention/sweeper.go`) runs at TTL=0, and audit records store hashes not content.
Enforced in the worker result path and delivery; verified by an integration test asserting no
S3 object survives a `zero`-mode job.
**Consent/GDPR & transient-audio.** In `zero` mode source audio is treated as strictly transient —
processed in memory/streamed and never landed at rest — which is the technical control behind the
consent + data-minimization commitments a BAA/DPA requires; the retention sweeper running at TTL=0
and hash-only audit records give a provable "nothing retained" posture. **Failure modes:** if a
processor in the job graph structurally requires a persisted intermediate (e.g. artifact bundles),
`zero` mode rejects the job up front with a clear error rather than silently persisting — fail
safe, never leak. A delivery failure in `zero` mode retries from the transient in-memory result
within the job's lifetime and, if undeliverable, drops the data rather than spilling it to S3.
**Observability:** emit `orpheus_zdr_jobs_total{outcome}` and assert zero bytes-at-rest without
logging any transcript content.

## 5. Rollout / milestones

Ordering only. Every milestone ships production-grade — full enforcement, failure handling,
metrics, and audit coverage — not a partial-scope pilot. The complete §4 scope is committed; the
staged per-org JWT rollout is a safe-deploy tactic, not a reduction in what ships.

1. **M1 — Identity foundation:** 4.1 Keycloak realm-as-code + integration tests + onboarding
   `org_id` provisioning; production JWT enforced (staged per-org rollout with the existing
   non-prod fallback as the migration seam, then prod-strict). 4.5 WS Origin allow-list.
   4.3 rate-limiter degraded mode with circuit breaker and metrics.
2. **M2 — Secrets management:** 4.6 provider (KMS/Vault) + `RequireProdSecrets` enforcement with
   every committed dev default rotated out; HMAC/stream-secret rotation with key-id prefix
   (old+new verify during rotation). No dev-default resolvable in prod.
3. **M3 — Dashboard IdP:** 4.2 OIDC (Auth Code + PKCE) on the same realm behind `DASHBOARD_AUTH`,
   dual-run for migration, then SQLite store retired; `platform:admin` gating preserved.
4. **M4 — Enterprise trust:** 4.4 SSO/SAML broker, SCIM provisioning/deprovisioning with session
   revocation, audit export + per-org tamper-evident hash chain, SOC2/HIPAA controls matrix.
   4.8 zero-data-retention toggle with transient-audio enforcement.
5. **M5 — Marketplace sandbox:** 4.7 Modal-isolated sandbox + static scan + manifest review + cost
   caps + auto-quarantine; community processors stay catalog-read-only and opt-in until the
   sandbox is fully in place.

## 6. Verification / acceptance criteria

End-to-end against a running API + configured Keycloak realm + Modal sandbox (not unit-only). Each
item covers a positive path, a negative/failure path, and multi-tenant isolation where relevant.

- **JWT (positive + negative + isolation):** a prod-config token from the configured realm yields
  the correct `OrgID` and `platform:admin` role (`keycloak.go:97`); a token without `org_id` is
  rejected in prod (`keycloak.go:115`); a token for org A cannot access org B data (FORCE-RLS
  denial asserted). Integration test mints real tokens via the realm.
- **Dashboard OIDC:** login works via OIDC with the SQLite store removed; `platform:admin` gating
  is unchanged; a revoked/deprovisioned user is denied on next request.
- **Rate limiter (degraded + fail-closed + recovery):** with Redis down, prod requests are capped
  by the degraded per-instance fallback (not unlimited), `orpheus_ratelimit_backend_errors_total`
  increments, and the circuit breaker stops hammering Redis; fail-closed mode returns 503; when
  Redis returns, Redis-backed enforcement resumes automatically. Response headers are identical
  across modes.
- **WS Origin:** a handshake from a non-allowed Origin (Origin set) is rejected; an allowed Origin
  with a valid HMAC token connects; an expired/forged token is rejected regardless of Origin.
- **Secrets:** no committed dev-default secret resolves in prod (`RequireProdSecrets`,
  `config.go:124` fails loud); rotating the stream/HMAC secret keeps both old and new tokens
  verifying during the overlap window, then old fails after cutover.
- **Zero-data-retention:** a `zero`-mode job leaves zero S3 objects and zero transcript text at
  rest (test-proven, sweeper at TTL=0, hash-only audit); a `zero`-mode job that requires a
  persisted intermediate is rejected up front rather than persisting; `orpheus_zdr_jobs_total`
  reflects outcomes.
- **Marketplace sandbox (isolation + bounds + failure):** community processor code cannot make
  network egress, cannot read another tenant's data, and runs with no ambient tenant credentials;
  a processor exceeding its `tier` CPU/mem/timeout is killed and fails only the invoking job while
  metering the consumed compute; repeated failures auto-quarantine the processor.
- **Enterprise:** SCIM deprovision revokes active sessions; audit export
  (`GET /v1/audit/export`) produces a hash chain that verifies and detects a tampered record; the
  SOC2/HIPAA controls matrix maps each control to a code path.

## 7. Dependencies, risks, open questions

- **Dependencies:** running Keycloak realm + admin API; Modal sandbox primitives for 4.7;
  KMS/Vault for 4.6; billing plan resolver to feed rate-limiter tiers.
- **Risks:** flipping prod JWT on a misconfigured realm locks users out — mitigate with a staged
  per-org rollout and the existing non-prod default-org fallback for dev. Fail-closed rate
  limiting turns a Redis blip into an outage; degraded mode is the safer default. Marketplace
  sandbox is the largest lift and gates any real third-party ecosystem.
- **Open questions:** SAML vs OIDC-only for the initial SSO cut? Vault vs AWS Secrets Manager as the first
  backend? Does ZDR mode disable async processors that require persisted intermediate artifacts
  (e.g. bundles)? Per-org or per-key retention granularity?

## 8. Effort

- 4.1 Keycloak realm-as-code: **S** (config + tests, no Go change).
- 4.3 rate-limiter degraded mode: **S**.
- 4.5 WS Origin: **XS**.
- 4.6 secrets provider: **M**.
- 4.2 dashboard OIDC: **M–L** (BFF rework + migration).
- 4.4 SSO/SCIM/audit program: **L** (SCIM handler + SAML + compliance evidence).
- 4.7 marketplace sandbox: **L** (Modal isolation + scan pipeline).
- 4.8 ZDR toggle: **M** (worker + delivery + sweeper + tests).
- **Total:** ~2 quarters; the M1+M2 identity+secrets slice is ~3–4 weeks. Every milestone ships
  production-grade (full enforcement, failure handling, audit, metrics), not a partial pilot.
