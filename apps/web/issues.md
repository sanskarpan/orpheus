# Orpheus Web — QA Audit & Issue Tracker

Audit method: instrumented Playwright sweep of all 19 routes (console/network/render
capture) + static analysis pass + DB/worker inspection. Environment: web `:3939`,
API `:8090` (DB `orpheus_verify`), worker live.

**Sweep headline:** after the redirect-loop fix, **all 19 routes render (0 blank, 0 error
pages)**; auth guard, role-gating, and 404 deep-links behave correctly. Remaining issues
are robustness/correctness gaps below.

| ID | Sev | Area | Status |
|----|-----|------|--------|
| 001 | P0 | Auth redirect loop | **Fixed ✓ verified** |
| 002 | P1 | Jobs pagination 404 | **Fixed ✓ verified** |
| 003 | P2 | No error boundary → raw 500s | Fixed (boundary added) |
| 004 | P2 | Session decrypt throw → unrecoverable 500 | Fixed |
| 005 | P2 | Transcribe poller gives up on transient error | **Fixed ✓ verified** |
| 006 | P2 | `.toFixed` on optional API fields → page 500 | Fixed |
| 007 | P3 | Status helpers assume non-null status | Fixed |
| 008 | P3 | Upload proxy 0-byte / null parts | Fixed |
| 009 | P3 | First-admin race (two concurrent → both admin) | Won't fix (local) |
| — | — | Owner-key revoke guard (`slice(0,9)`) | Investigated — safe |

## Round 2 — behavior verification (B) + streaming

Drove the real mutations at the API level (same endpoints the UI calls) against the live
API/worker, plus DB inspection. **13/13 behavior checks pass** after the fixes below.

### ISSUE-010 — API-key auth fails intermittently under many keys (prefix collision)
- **Severity:** P1 · **Area:** `apps/api` auth · **Status:** Fixed ✓ verified (3 clean runs)

The stored key prefix is only `ak_live_` + one base64 char (**64 possible prefixes**), and
`GetAPIKeyByPrefix` did `WHERE prefix=$1 LIMIT 1` + verified one hash. Once many keys exist,
the lookup returns a colliding key, Argon2 fails, and auth 401s at random — which broke the
dashboard after provisioning many orgs. **Fix:** `GetAPIKeysByPrefix` returns all candidates;
`Verify` Argon2-checks each (revocation still beats the cache). Added regression coverage.

### ISSUE-011 — Webhook deliveries never listed (parameter off-by-one)
- **Severity:** P1 · **Area:** `apps/api` `WebhookHandler.ListDeliveries` · **Status:** Fixed ✓ verified

`argIdx` started at 2 and was used as the next placeholder, so the query became
`… org_id = $2 … LIMIT $2` — it never returned rows, so the UI's deliveries table was always
empty (even for real events). **Fix:** index placeholders as `len(args)+1`.

### ISSUE-012 — Dashboard "Test fire" 400s (missing request body)
- **Severity:** P1 · **Area:** web `lib/orpheus.ts` · **Status:** Fixed ✓ verified

`POST /v1/webhooks/{id}/test` requires a JSON body (`{event_type}`); the client sent none →
400 "Invalid JSON". **Fix:** `testWebhook` sends `{event_type:"job.completed"}`.

### Verified working (no change needed)
Upload→artifact roundtrip; artifact signed-URL playback (`audio/wav`, 200); transcribe→complete;
**webhook delivery** (real POST to the endpoint, status recorded) + **replay** (new delivery) +
real `job.completed` emission; **requeue** (dead_letter→queued); marketplace submit/list/review;
Ops tenant provisioning.

### FEATURE — Live streaming studio (real WebAudio waveform)
`app/dashboard/streaming/LiveStreamStudio.tsx`: opens the mic (getUserMedia), renders a **real**
frequency waveform from an `AnalyserNode` on canvas, transcribes live via the browser speech
engine when available, and finalizes the real Orpheus session (create→capture→finalize).
Verified with a fake media stream: canvas paints from analyser data, timer runs, finalize
creates + lists the session. (A server-side WebSocket ASR bridge remains a separate backend effort.)

### Regression pass (paced, single-user) — PASS
`typecheck` + `build` clean. Playwright: signup→dashboard ✅ · **transcribe completes + renders
transcript** ✅ (job COMPLETED, real cost) · jobs pagination href → `/dashboard/jobs` ✅ · bad
job id → graceful 404 ✅ · api-key create ✅ · all 10 dashboard routes render, 0 pageerrors ✅.
Note: the initial sweep's transcribe "timeout" was the aggressive audit rate-limiting the org
key mid-poll — the job completed backend-side in 3s; ISSUE-005 makes the UI resilient to that.

---

## ISSUE-001 — Redirect loop: `/dashboard` ↔ `/login` with a stale cookie
- **Severity:** P0 · **Priority:** P0 · **Area:** Auth/middleware · **Type:** Functional/Runtime · **Status:** Fixed + verified

### Description / repro
With any `orpheus_session` cookie whose account no longer resolves (stale from an earlier
run, or a cookie of the old `{apiKey}` shape), the app infinite-loops and renders a blank
page ("Throttling navigation to prevent the browser from hanging").
1. Have a stale `orpheus_session` cookie. 2. Open `/dashboard`. 3. Blank page + loop.

### Root cause
`middleware.ts` bounced `/login`→`/dashboard` on cookie **presence**, while
`app/dashboard/layout.tsx` redirected `/dashboard`→`/login` when the cookie didn't resolve
to an account. Presence ≠ valid account → the two redirects ping-pong forever.

### Fix
- Removed the presence-based `/login|/signup`→`/dashboard` bounce from `middleware.ts`.
- Auth pages (`login/page.tsx`, `signup/page.tsx`) now redirect to `/dashboard` only after a
  real `getAccount()` check.
- Dashboard layout redirects unresolved sessions to a new `app/logout/route.ts` (a Route
  Handler *can* clear the cookie; a Server Component can't), which destroys the cookie → `/login`.

### Verification
`curl` with a bogus cookie: `/dashboard` → `/logout` → `/login` (200), 2 redirects, no loop.
Logged-out `/dashboard` → `/login`. Full Playwright sweep: 0 blank pages.

---

## ISSUE-002 — Jobs "Next page" links to `/jobs` (404)
- **Severity:** P1 · **Area:** Jobs list · **Route:** `/dashboard/jobs` · **Type:** Functional · **Status:** Fixed

Pagination href was `/jobs${qp(...)}` — not prefixed with `/dashboard` (the IA move's
link-rewrite regex missed the `` `/jobs${ `` template form). Clicking Next 404s. Artifacts &
audit pagination were already correct. **Fix:** `/dashboard/jobs${qp(...)}`.

## ISSUE-003 — No error boundary; detail pages 500 on any non-404 API error
- **Severity:** P2 · **Area:** All dashboard detail routes · **Type:** Runtime · **Status:** Fixed

`jobs/[id]`, `artifacts/[id]`, `processors/[name]`, `webhooks/[id]` do `catch { if 404
notFound(); throw }`. Any 500/401/network drop crashes the route to Next's raw error page.
**Fix:** add `app/dashboard/error.tsx` (segment boundary) + `app/global-error.tsx` with a
branded, recoverable fallback.

## ISSUE-004 — Session decrypt throw is unrecoverable
- **Severity:** P2 · **Area:** `lib/session.ts`/`lib/accounts.ts` · **Type:** Runtime · **Status:** Fixed

`decrypt()` throws on a corrupted `org_key_enc` or a rotated `SESSION_SECRET`; the throw
escapes `getAccount()`, so the dashboard 500s and never reaches the `/logout` recovery.
**Fix:** `getAccountById`/`getAccount` swallow decrypt errors and return null → stale-session
recovery path runs.

## ISSUE-005 — Transcribe poller quits on the first transient error
- **Severity:** P2 · **Area:** `upload/UploadStudio.tsx` · **Type:** Functional/UX · **Status:** Fixed

Observed in the audit: job `8ea612b1` **completed on the backend in 3s**, but the UI showed a
failure/hung because a transient poll error (rate-limit 429 under load) set `phase="error"`
and stopped polling. `run()`/`poll()` also had unguarded awaits (a rejected server action
leaves the button stuck on "Processing…"), and the 150-try cap returned silently.
**Fix:** tolerate transient poll errors (retry a bounded number of times before failing),
guard the awaits, and surface a clear message on give-up.

## ISSUE-006 — `.toFixed()` on optional API fields crashes the page
- **Severity:** P2 · **Area:** `usage/page.tsx`, `processors/[name]/page.tsx` · **Type:** Runtime · **Status:** Fixed

`p.compute_seconds.toFixed(1)` and `v.slo_p95_seconds/p99.toFixed(1)` run in JSX (outside the
fetch try/catch). If the API omits a field, the whole page 500s. **Fix:** coerce via a
`num()` helper (`Number(x ?? 0)`).

## ISSUE-007 — Status helpers assume a non-null status
- **Severity:** P3 · **Area:** `components/primitives.tsx` · **Type:** Runtime · **Status:** Fixed

`statusTone(status).toLowerCase()` / `StatusBadge …replace()` throw if `status` is null/omitted.
**Fix:** default to `""`/`"unknown"`.

## ISSUE-008 — Upload proxy: 0-byte file & null parts
- **Severity:** P3 · **Area:** `app/api/upload/route.ts` · **Type:** Runtime · **Status:** Fixed

`part_size 0` for a 0-byte file; `[...session.parts]` throws if the API returns `parts:null`.
**Fix:** reject empty files with a clear 400; default `parts` to `[]`.

## ISSUE-009 — First-admin race
- **Severity:** P3 · **Status:** Won't fix (local dev only)

Two concurrent first-signups both read `countAccounts()===0` and both become platform admin.
Not reachable in single-user local use; documented.

## Investigated — safe
- **Owner-key revoke guard** (`api-keys/page.tsx` `ownerPrefix = org_key.slice(0,9)`): the Go
  API sets `prefix = secret[:9]` and returns exactly that, so the equality check is correct;
  the "dashboard/managed" key cannot be revoked. Confirmed against `onboarding.go` / `api_keys.go`.
- redirect-in-try/catch: all `redirect()` calls are outside try/catch — `NEXT_REDIRECT` not swallowed.
- `ORPHEUS_ADMIN_KEY` missing: handled with friendly errors in signup/demo/ops.
- `/api/upload` self-checks the session (401 without it).
- `UsageChart` SVG math, `ResultView`, and `format.ts` are null/NaN-safe.
