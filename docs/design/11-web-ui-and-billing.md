# Design 11 — Admin dashboard (Next.js) + usage metering & billing (Stripe)

> **Status:** proposed · **Owner:** web / billing · **Scope:** gap #11
> **Depends on:** [ADR-0002 / ADR-0013 (Go public API)](../adr/0013-polyglot.md), [PRODUCTION_DESIGN §3.9, §5.3, §8.3](../architecture/PRODUCTION_DESIGN.md)
> **Companion scaffold:** [`apps/web/`](../../apps/web/) — Next.js App Router skeleton (placeholder pages, no build required).

Design + build checklist. The `apps/web/` scaffold is **placeholder-only** and
is explicitly marked as scaffold; it is not wired to CI and is not expected to
build until the checklist is done.

---

## 1. Problem

Orpheus has a complete programmatic surface (REST + OpenAPI, API keys, webhooks)
but **no human UI** and **no billing**:

- Operators/customers have no way to see jobs, workflows, usage, DLQ, or webhook
  deliveries except by calling the API directly.
- There is **no usage metering rollup and no invoicing**. `job_costs.cost_usd`
  exists but is not computed (IMPLEMENTATION_STATUS), and there is no path from
  "work happened" → "customer is charged".
- `PRODUCTION_DESIGN §5.3` lists **Billing** as a bounded context
  (`Plan`, `Subscription`, `UsageEvent`, `invoices`) and §3.9 chose **Next.js
  App Router** for the admin dashboard. Neither exists yet.

This design delivers the **admin dashboard IA** and the **usage-metering →
invoice-rollup → Stripe** billing pipeline.

---

## 2. Proposed architecture

```
  Browser ──► Next.js (App Router, RSC, server actions)  [apps/web]
                 │  Keycloak PKCE (browser JWT, §8.5)
                 │  read: RSC server components call the Go API with the
                 │        user's token (RLS scopes to their org)
                 │  write: server actions → Go API
                 ▼
         Go public API (unchanged surface, §8.3)
                 │
                 ▼
   Billing pipeline (event-driven, matches §5.5 outbox+NATS):
     job.completed / job.failed  ─► usage metering consumer
        → compute cost (gpu_s, cpu_s, mem_gb_s, egress) → job_costs
        → INSERT usage_events (append-only, partitioned monthly)
     hourly rollup job  ─► usage_events_hourly (per org/processor)
     monthly close job  ─► invoice draft (line items from rollups)
        → Stripe: create invoice items + finalize + charge
        → webhook back from Stripe → mark invoice paid/failed
```

- **Next.js is read-heavy** → React Server Components fetch from the Go API on
  the server (token forwarded), so most pages need no client JS and no direct DB
  access. **The web app never talks to Postgres directly** — it goes through the
  Go API, inheriting RLS + RBAC + rate limits. This keeps the security model in
  one place.
- **Billing is a separate concern** from the UI: the metering/rollup/close jobs
  are backend (Go API tier owns the `Billing` bounded context, §5.3), and the
  dashboard merely *renders* usage and invoices via the API.
- **Stripe is the system of record for money** (PCI scope stays with Stripe,
  ADR §5.3). We hold usage + invoice metadata; Stripe holds cards, charges,
  tax.

### 2.1 Admin dashboard information architecture

```
/                         Overview: usage this period, spend, recent jobs, DLQ count, SLO
/jobs                     List (filter: processor, status, date), row → detail
/jobs/[id]                Status, params, result JSON, cost, model_version_id, timeline
/workflows                Multi-step runs (design #9); engine (db|temporal); steps
/workflows/[id]           Step-by-step timeline, cancel action
/artifacts                Uploaded audio; signed-URL download; delete (GDPR)
/processors               Catalog (transcribe, diarize, ...); versions; SLO/cost
/webhooks                 Endpoints; deliveries; replay; DLQ (exhausted) inspect
/api-keys                 Create (secret shown once) / list / revoke; scopes
/usage                    Metered usage: by processor, by day; export CSV
/billing                  Current plan, subscription, invoices, payment method
/billing/invoices/[id]    Line items (from rollups), Stripe status, PDF link
/settings/org             Org, members, roles (RBAC), plan
/ops   (staff only)       DLQ requeue, worker/pool health, feature flags
```

- **RBAC-gated** (§8.5 scopes): `/ops` requires staff; `/billing` requires
  `billing:manage`; `/api-keys` requires `apikey:manage`. Enforced server-side
  in the Go API; the UI only hides what the token can't do.
- **Real-time**: job/workflow progress via SSE (§8.1) — the detail pages
  subscribe to the existing SSE channel; no new transport.

### 2.2 Usage metering → invoice rollup

1. **Meter** (per job): the metering consumer subscribes to
   `orpheus.jobs.completed` / `.failed` (NATS, §5.5). It computes cost from
   recorded resource usage (`gpu_seconds`, `cpu_seconds`, `memory_gb_seconds`,
   `s3_egress_bytes`) × the price book for the job's plan, writes `job_costs`,
   and appends a `usage_events` row (append-only, monthly-partitioned, §9.2).
   Idempotent on `job_id` (a re-delivered NATS message must not double-charge).
2. **Roll up** (hourly): aggregate `usage_events` → `usage_events_hourly` per
   `(org, hour, processor)` (the §9.3 query #5 pattern) for cheap dashboards and
   invoice line items.
3. **Close** (monthly, per org): sum the period's rollups into invoice line
   items (one per processor/metric), create a **draft invoice** row, push line
   items to Stripe, finalize, and let Stripe charge the saved payment method.
4. **Reconcile**: a Stripe webhook (`invoice.paid`,
   `invoice.payment_failed`) updates our invoice status; dunning/retries are
   Stripe's job. We store only status + Stripe IDs.

**Pricing model:** usage-based (per gpu-second / per minute of audio) on top of
a plan (free/pro/enterprise) with included quota; overage metered. The **price
book is versioned** so a price change doesn't retroactively re-rate historical
usage.

---

## 3. Data-model changes

The `Billing` context tables are already named in §9.1 (`plans`,
`subscriptions`, `usage_events`, `invoices`). This design fills them in via a
new migration `0007_billing.sql` (**author under
`apps/api/internal/db/migrations/`; out of scope here — do not edit existing
migrations**):

```sql
-- plans: catalog (global). free|pro|enterprise + included quota + price_book_id
-- subscriptions(org_id, plan_id, stripe_subscription_id, status, period_start/end)
-- usage_events(org_id, job_id UNIQUE, processor, metric, quantity, unit_cost_usd,
--              amount_usd, occurred_at)   -- append-only, monthly partitions, RLS
-- usage_events_hourly(org_id, hour, processor, metric, quantity, amount_usd)  -- PK(org,hour,processor,metric)
-- invoices(org_id, stripe_invoice_id, period_start/end, subtotal_usd, total_usd,
--          status draft|open|paid|uncollectible|void, line_items jsonb)  -- RLS
-- price_books(id, version, effective_from, rates jsonb)   -- immutable, versioned
```

All tenant-scoped billing tables get the same RLS policy shape as existing
tables (`FORCE ROW LEVEL SECURITY`, `org_id = current_org_id()` or service role
— matches `0003_workflows.sql`). Stripe IDs are stored, card data never is.

---

## 4. API surface

Public (some already listed in §8.3):

```
GET  /v1/usage                          # current period usage (from rollups)
GET  /v1/usage/export                    # CSV export
GET  /v1/billing/invoices                # list
GET  /v1/billing/invoices/{id}           # line items + Stripe status + PDF url
GET  /v1/billing/subscription            # current plan + status
POST /v1/billing/portal-session          # → Stripe Billing Portal URL (self-serve)
POST /v1/billing/checkout-session        # → Stripe Checkout for plan upgrade
```

Internal / webhook:

```
POST /internal/stripe/webhook            # Stripe events (signature-verified)
# metering consumer + hourly rollup + monthly close run as backend jobs, not HTTP
```

The Next.js app calls only the public `/v1/...` surface with the user's token.

---

## 5. Rollout plan

1. **Metering, invisible.** Ship the metering consumer + `job_costs` computation
   + `usage_events` (append-only). No charging. Validate numbers against
   infra cost for a month (shadow accounting).
2. **Read-only dashboard.** Ship `apps/web` with overview/jobs/usage pages
   (RSC, read-only) behind Keycloak. No billing writes.
3. **Rollups + invoice drafts (no charge).** Hourly rollup + monthly draft
   invoices; review manually; do not push to Stripe yet.
4. **Stripe test mode.** Wire Checkout/Portal + invoice push + webhook in test
   mode; internal orgs only.
5. **Stripe live, self-serve.** Enable live charging for free→pro self-serve;
   enterprise stays invoiced manually first.
6. **Full dashboard.** Add workflows, webhooks/DLQ, api-keys, `/ops` (staff),
   settings.

Rollback = the dashboard is read-only-safe at every step; charging is the last,
independently-flagged step.

---

## 6. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Double-charging on NATS redelivery | Med | `usage_events.job_id` UNIQUE + idempotent consumer; reconcile against `job_costs`. |
| Cost model wrong (charge ≠ infra cost) | Med | Shadow accounting month (step 1); ADR-0011 margin alert; price book versioned. |
| Stripe webhook missed → invoice stuck | Med | Signature-verified endpoint + reconciliation poller against Stripe API as backstop. |
| Next.js RSC leaks another tenant's data | Low | UI never hits DB; all reads go through the Go API under the user's token (RLS). Playwright cross-tenant test. |
| Price change re-rates history | Low | Immutable, versioned `price_books`; rollups pin the price_book_id used. |
| PCI scope creep | Low | Card data never touches our infra; Stripe Checkout/Portal only. |
| Dashboard auth drift from API RBAC | Med | Single source of truth = Go API scopes; UI hides but API enforces. |

---

## 7. What must be built (checklist)

- [ ] `apps/web` Next.js App Router app: real `package.json` deps, layout, auth
      (Keycloak PKCE), API client, replace the placeholder pages.
- [ ] Read-only pages: overview, `/jobs` + `/jobs/[id]`, `/usage`,
      `/processors`, `/artifacts`.
- [ ] Write/interactive pages: `/workflows` (+ cancel), `/webhooks` (+ replay),
      `/api-keys` (create/revoke), `/settings/org`, `/ops` (staff-gated).
- [ ] SSE subscription on job/workflow detail pages (reuse §8.1 channel).
- [ ] Metering consumer (NATS `jobs.completed`/`.failed`): compute cost,
      idempotent `usage_events` + `job_costs`.
- [ ] Hourly rollup job → `usage_events_hourly`.
- [ ] Monthly close job → draft invoice → Stripe invoice items + finalize.
- [ ] Stripe integration: Checkout, Billing Portal, signature-verified webhook,
      reconciliation poller.
- [ ] Migration `0007_billing.sql` (`plans`, `subscriptions`, `usage_events`,
      `usage_events_hourly`, `invoices`, `price_books` + RLS + partitions).
      **Do not edit existing migrations.**
- [ ] Versioned `price_books`; plan quotas + overage metering.
- [ ] Public API: `/v1/usage`, `/v1/billing/*`, portal/checkout sessions.
- [ ] Playwright: cross-tenant isolation + billing happy-path (test mode).
- [ ] Wire `apps/web` into pnpm workspace + CI (build, lint, Playwright) — only
      after the scaffold becomes real.
