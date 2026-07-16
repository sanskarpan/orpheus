# `apps/web` — Orpheus admin dashboard (SCAFFOLD, not buildable yet)

> **Status: skeleton only.** This is the landing zone for gap #11 (admin
> dashboard + billing UI). It is **not** wired into the pnpm workspace or CI and
> is **not expected to build**. `pnpm dev`/`build` intentionally exit non-zero.
> Design: [`docs/design/11-web-ui-and-billing.md`](../../docs/design/11-web-ui-and-billing.md).

## What this will be

A **Next.js (App Router) + TypeScript** admin dashboard (chosen in
`PRODUCTION_DESIGN §3.9`). Read-heavy → React Server Components fetch from the
**Go public API** on the server with the user's Keycloak token; **it never talks
to Postgres directly**, so it inherits RLS + RBAC + rate limits from the API.
Billing (usage metering → invoice rollup → Stripe) is a backend concern; this UI
only renders usage and invoices via the API.

## Current contents (placeholders)

```
apps/web/
  package.json          ← SCAFFOLD manifest (deps for shape; dev/build exit 1)
  README.md             ← you are here
  app/
    layout.tsx          ← SCAFFOLD root layout (placeholder)
    page.tsx            ← SCAFFOLD overview page (placeholder)
    dashboard/page.tsx  ← SCAFFOLD jobs/overview placeholder
    billing/page.tsx    ← SCAFFOLD billing placeholder
  lib/
    api.ts              ← SCAFFOLD typed API-client stub (throws)
```

Every `.tsx`/`.ts` file begins with a `SCAFFOLD` banner and renders/returns a
"not implemented" placeholder. No data fetching, no auth, no styling.

## Information architecture (target)

See `docs/design/11-web-ui-and-billing.md §2.1` for the full route map
(`/jobs`, `/workflows`, `/artifacts`, `/processors`, `/webhooks`, `/api-keys`,
`/usage`, `/billing`, `/settings/org`, `/ops`).

## Boundaries

- **No direct DB access.** All reads/writes go through the Go API (`/v1/...`)
  with the user's token.
- **RBAC enforced server-side** in the API; the UI only hides what the token
  can't do.
- **Money lives in Stripe** (PCI scope stays with Stripe); we store usage +
  invoice metadata + Stripe IDs only.

## When it becomes real

Follow the build checklist in `docs/design/11-web-ui-and-billing.md §7`, then
add `apps/web` to the pnpm workspace + CI (build, lint, Playwright). Until then,
this directory is documentation-with-shape, not a running app.
