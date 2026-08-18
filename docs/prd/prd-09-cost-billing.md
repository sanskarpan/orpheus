# PRD: Cost / Billing Completion — LLM Pass-through, Real Cache Savings & Metered Billing

**Status:** Proposed · **Priority:** P1 · **Epic:** Metering & Billing · **Related issues:** #11

## 1. Summary

Orpheus already meters compute and bills from it: the worker writes `jobs.cost_usd`
per job (GPU seconds × GPU rate, else wall-clock × CPU rate, `worker.py:247`), the
usage service rolls that into `usage_rollup_hourly` (`internal/usage/service.go:70`),
and the billing rollup sums `cost_usd` into monthly invoices delivered through Dodo
(`internal/billing/rollup.go:76`, `internal/billing/dodo.go:75`). The metering spine
is real. Four gaps keep the money story incomplete:

1. **LLM token cost is not passed through.** Summarize/translate/analysis call
   external LLMs (`worker.py`→ `processors/text_ops.py:167`, `llm.py`) but the
   returned result carries no token counts, so those jobs are billed at the flat CPU
   rate — undercharging on real API spend.
2. **Cache "savings" are computed on the wrong number.** `est_savings_usd` sums the
   *source job's `cost_usd`* (`handlers/cache.go:113`). For local/CPU source jobs
   that flat rate understates the true saved GPU cost; savings should reflect real
   GPU cost avoided.
3. **Billing → metering coupling is partial.** Invoices sum `cost_usd`
   (`rollup.go:96`) but that figure omits LLM pass-through and any per-unit markup;
   there is no single, auditable "billable amount" definition.
4. **No cost dashboards tied to real spend.** `GET /v1/cache/stats` and the usage
   rollups exist, but there is no spend-over-time / cost-by-processor / projected-
   invoice surface built on the real numbers.

## 2. Motivation & goals

**Goals**
- Meter LLM token usage per job and fold it into `jobs.cost_usd` so summarize/
  translate/analysis are billed on actual provider cost + markup.
- Recompute cache savings against **real GPU cost avoided**, not the flat source
  `cost_usd`.
- Define one **billable amount** = compute cost + LLM pass-through, flowing
  unchanged from `jobs.cost_usd` → rollup → invoice → Dodo.
- Ship **cost dashboards** (spend over time, by processor, projected invoice, cache
  savings) sourced from `usage_rollup_hourly` and `jobs`.

**Non-goals**
- Prepaid credits / metered prepay wallets (separate billing epic).
- Re-pricing historical invoices (already-collected invoices are immutable —
  `rollup.go` upsert only touches `draft`/`open`, `rollup.go:104`).
- Per-seat or subscription billing; Orpheus stays usage-based.

## 3. Current state in Orpheus (file:line, patterns to build on)

- **Per-job cost**: `worker.py:247` — on success, `gpu_seconds` (from the Modal
  result, `infra/modal/orpheus_transcribe.py:142`) × `gpu_cost_usd_per_second`
  (`config.py:26`, default ≈ A10 $1.10/hr), else `duration × cost_usd_per_second`
  (`config.py:22`, default $0.00005/s CPU). Written via `mark_job_completed(...,
  cost_usd=cost)` at `worker.py:259`.
- **LLM providers**: `llm.py` — Anthropic/OpenAI/Gemini/openai-compat/stub, all
  `temperature=0`. **No usage/token counts are read from responses** (`_chat`
  returns only text, e.g. `llm.py:167`, `:210`). `manifest_identity()` (`llm.py:293`)
  already advertises the real model id.
- **Text processors**: `processors/text_ops.py:167` (`summarize`), `:206`
  (`_analyze_json`) — they call the LLM and return text; they do not set
  `gpu_seconds` or any token field, so `worker.py` bills them at the CPU rate.
- **Cache savings**: `handlers/cache.go:98` `Stats` — `est_savings_usd` =
  `SUM(source.cost_usd)` over cache-hit jobs (`cache.go:113`). Cache-hit jobs are
  inserted with `cost_usd = 0` (`jobs.go:353`), correctly free; the *savings* figure
  is the questionable one.
- **Usage rollup**: `usage/service.go:70` re-aggregates completed jobs into
  `usage_rollup_hourly` (total/processor/status dims) over a 48h trailing window;
  budgets enforced/hard-capped from these rollups (`service.go:114`, hard-cap read at
  `handlers/jobs.go:186`).
- **Billing**: `billing/rollup.go:76` sums `cost_usd` → `invoices.total_usd`; Dodo
  checkout at `dodo.go:75` (`AmountCents = TotalUSD*100`). Webhook applies payment
  under service role (`handlers/billing.go`).

## 4. Proposed design

### 4.1 LLM token-cost pass-through

**Capture usage at the provider boundary.** Extend each `_chat` in `llm.py` to
return `(text, usage)` where `usage = {prompt_tokens, completion_tokens}` parsed from
the provider response (Anthropic `usage`, OpenAI `usage`, Gemini
`usageMetadata`). The stub returns deterministic token estimates
(`len(text)//4`). Keep the public task methods (`summarize`, `translate`,
`complete`) returning text, but stash the last-call usage on the provider (or
return a small `LLMResult`).

**Price it.** Add a per-model rate table (env/DB-backed), keyed by
`model_version_id` (`llm.py:293` already produces `anthropic:claude-...`,
`openai:gpt-4o-mini@host`, etc.):
```
llm_cost_usd = prompt_tokens/1e6 * input_rate + completion_tokens/1e6 * output_rate
```
Default rates ship in worker config alongside `gpu_cost_usd_per_second`
(`config.py:26`), overridable per model. Apply an optional pass-through markup
multiplier (`ORPHEUS_WORKER_LLM_MARKUP`, default 1.0).

**Rates are stored in DB with effective dates** (config is only the seed/default).
The rate table carries `(model_version_id, input_rate, output_rate, effective_from,
effective_to)` so a job prices against the rate in effect **at the job's completion
time**, and historical jobs keep their as-billed rate when provider prices later
change. This is the audit trail: one billable definition = compute + LLM
pass-through, priced from dated rate rows, never retroactively re-priced. Each
pricing decision logs the `model_version_id` and the `effective_from` row it matched.

**Return it in the result.** Text processors set `llm_usage` +
`llm_cost_usd` on their result dict (`text_ops.py` around `:167`/`:206`). Then extend
the cost path at `worker.py:247`: after computing compute `cost`, add
`(result or {}).get("llm_cost_usd", 0)`. This keeps one billable number in
`jobs.cost_usd` — no schema change required for billing to pick it up.

**Persist detail for audit** (new nullable columns, additive migration):
`jobs.llm_prompt_tokens`, `jobs.llm_completion_tokens`, `jobs.llm_cost_usd`. The
outbox `job.completed` payload (`worker.py:264`) gains `llm_cost_usd` for
observability. All three columns are **additive and nullable** — the existing
`jobs.cost_usd`→rollup→invoice→Dodo pipe is untouched, and `(result or {})
.get("llm_cost_usd", 0)` at `worker.py:247` keeps every pre-migration result and
every non-LLM processor working unchanged (compute-only jobs simply add 0).

**Error handling & failure modes (graceful degradation).** The metering path must
never fail a job or corrupt a bill:
- *Provider omits usage fields* (a proxied openai-compat host, `llm.py:280`, or a
  provider that drops `usage`/`usageMetadata`): fall back to a token estimate
  (`len(prompt)//4`, `len(completion)//4`) and set `jobs.llm_cost_estimated=true`
  so the charge is transparent and auditable. The job still bills — degradation is
  graceful, never a hard error.
- *Rate-table lookup miss* (a `model_version_id` with no row): use a configured
  default rate (`ORPHEUS_WORKER_LLM_DEFAULT_*_RATE`) and emit a `llm_rate_missing`
  alert/metric keyed by model id, so pricing gaps surface without dropping the
  charge.
- *No double-count* between compute and LLM: `gpu_seconds` and `llm_cost_usd` are
  never both attributable to the same unit of work. Transcribe/diarize is
  GPU-metered; summarize/translate/analysis are LLM-metered — distinct processors,
  naturally partitioned. `worker.py:247` adds compute `cost` + `llm_cost_usd` once;
  a job that reports `gpu_seconds` reports `llm_cost_usd = 0` and vice-versa, and the
  billable definition asserts this so no unit is counted twice.

**Observability.** Emit the `llm_cost_estimated` rate (share of LLM jobs on the
estimate fallback), a per-model `llm_rate_missing` counter, and a rate-table
effective-date audit log line each time a job prices against a dated rate row (below).

### 4.2 Cache savings on real GPU cost

Today a cache hit is billed 0 (`jobs.go:353`) — correct. The reported *savings*
(`cache.go:113`) should equal **what recomputation would have cost on GPU**, not the
source job's possibly-CPU `cost_usd`.

Design: when the worker completes a cacheable job, persist the **recompute cost basis**
onto the source job / cache entry. Add `job_result_cache.recompute_cost_usd`
populated from the source job's *true* cost when it ran on GPU, or an estimate
`duration_seconds × gpu_cost_usd_per_second` when the source ran on CPU (so savings
reflect the GPU price a hit avoids, since production serves GPU). `populate_result_cache`
(`worker.py:262`) writes it alongside the existing `cache_meta`.

Then `cache.go:113` changes:
```sql
SUM(CASE WHEN cache_hit THEN
      COALESCE((SELECT c.recompute_cost_usd FROM job_result_cache c
                WHERE c.source_job_id = jobs.cached_from_job_id), src.cost_usd)
    ELSE 0 END)
```
so `est_savings_usd` reflects real GPU cost avoided, with the current source
`cost_usd` as a fallback for legacy entries. `CacheStats` (`cache.go:85`) gains
`gpu_savings_usd` to make the basis explicit.

**Additive & backward-compatible.** `job_result_cache.recompute_cost_usd` is a new
nullable column; the `COALESCE(..., src.cost_usd)` fallback means legacy cache
entries (no basis persisted) still return a value, so the query never regresses.
Cache-hit jobs stay billed `0` (`jobs.go:353`) — this change touches only the
*reported savings*, never the *charge*, so the billing pipe is untouched.

**Failure modes & observability.** If `recompute_cost_usd` cannot be computed at
populate time (missing `duration_seconds`), the entry is written with a null basis
and the query falls back to `src.cost_usd` — degrade, don't fail the job. Emit a
`cache_gpu_savings_usd` metric carrying the basis (GPU-actual vs. CPU-estimated) so
the "GPU cost avoided at current rates" assumption is observable and auditable, not
hidden inside the SQL.

**Scoping.** The savings query is org-scoped exactly as `cache.go:98` `Stats`
already is (RLS-confined per org); no cross-tenant aggregation is introduced.

### 4.3 Couple billing (Dodo) to real metering

The pipe already exists (`jobs.cost_usd` → `usage_rollup_hourly` →
`invoices.total_usd` → Dodo). This work makes the pipe *complete and auditable*:

- **One billable definition**: `jobs.cost_usd = compute_cost + llm_cost_usd`
  (§4.1). Because rollup (`usage/service.go:70`) and invoice rollup
  (`billing/rollup.go:96`) both sum `cost_usd`, LLM pass-through flows through with
  zero billing-code change.
- **Invoice line-item breakdown**: extend `RollupPeriod` (`rollup.go:76`) to also
  populate per-processor and compute-vs-LLM subtotals into a new **additive**
  `invoice_line_items` table (org, period, dimension, amount) sourced from
  `usage_rollup_hourly`'s `processor` dimension (`service.go:84`). The table is
  **org-scoped and RLS-confined** like every other billing table; a row belongs to
  exactly one org and period. `InvoiceView` (`handlers/billing.go:36`) gains an
  additive `line_items[]` field — existing consumers that ignore it are unaffected.
  Subtotals sum to `invoices.total_usd` by construction, so line items are a
  breakdown of the same money, never an independent re-charge.
- **Reconciliation check (bounded, per-org, non-destructive)**: a periodic job
  asserts `SUM(usage_rollup_hourly.cost_usd for month) == invoices.total_usd` per
  org. It runs as a **bounded periodic task over the trailing window** (aligned with
  the existing rollup cadence), not per-request. On drift it **logs a
  `billing_reconciliation_drift_usd` metric per org and alerts** — it **never
  silently corrects a historical invoice** (already-open/collected invoices are
  immutable, `rollup.go:104`); reconciliation observes and surfaces mismatch, humans
  resolve it. Reconciliation is strictly per-org so one tenant's drift never touches
  another's books.
- Dodo `AmountCents` (`dodo.go:75`) is unchanged — it already reads `TotalUSD`.
  Because `jobs.cost_usd` already folds in LLM pass-through (§4.1), the same number
  flows metering → rollup → invoice → Dodo checkout with **zero billing-code change**
  and no risk of the LLM component being double-added downstream.

### 4.4 Cost dashboards tied to real spend

New read-only, tenant-scoped endpoints (mounted under `/v1`, `server/server.go:172`,
gated `rs("usage:read")`), all sourced from real tables:

```
GET /v1/usage/cost?from&to&granularity=hour|day    # spend over time (usage_rollup_hourly)
GET /v1/usage/cost/by-processor?period=month        # processor dim (service.go:84)
GET /v1/usage/cost/breakdown?period=month           # compute vs LLM vs cache-saved
GET /v1/billing/projection                          # month-to-date + linear projection
```
All return the `{data, has_more, next_cursor}` envelope (`health.go:65`). The web
dashboard (apps/web) renders: spend line chart, cost-by-processor bar, projected
invoice vs. budget limit (budgets at `handlers/budgets.go`), and cache
savings (§4.2). No new metering — pure reads over `usage_rollup_hourly` + `jobs`.

**Multi-tenant security & RLS.** Every endpoint above (`GET /v1/usage/cost*`,
`GET /v1/billing/projection`) is **org-scoped via RLS and gated `rs("usage:read")`**
(`server/server.go:172`). Org A can never read org B's spend, breakdown, or
projection — the org filter is enforced at the row-security layer, not just in the
handler. This is asserted as an acceptance test (§6).

**Scale & concurrency (bounded, pure reads).** These endpoints are **pure reads over
pre-aggregated rollup tables** — no per-request recompute of job cost. Spend-over-
time and by-processor read `usage_rollup_hourly` (already dimensioned by
`processor`, `service.go:84`); the breakdown reads the same rollup plus the
`invoice_line_items` subtotals; the projection is month-to-date sum × linear
extrapolation. Query cost is bounded by the requested window (indexed on
`org_id, hour_bucket`) and independent of raw job volume, so dashboard latency does
not grow with throughput. Heavy aggregation stays in the bounded periodic rollup
(`service.go:70`, 48h trailing window), never on the read path.

## 5. Rollout / milestones

Milestones are **ordering only**. Each is independently production-quality and
shippable — none is a reduced-scope prototype, and the full scope (LLM pass-through
metering, real-GPU cache-savings basis, invoice line items + reconciliation, cost
dashboards) all lands. Every milestone ships with the error handling, RLS scoping,
additive-migration guarantees, and observability described in §4.

1. **M1 — LLM pass-through metering (production).** `llm.py` returns
   `(text, usage)`; DB-backed rate table with effective dates + config seed; text
   processors emit `llm_usage`/`llm_cost_usd`; `worker.py:247` folds it into
   `cost`; additive nullable `jobs.llm_*` columns; `llm_cost_estimated` fallback for
   providers that omit usage; default-rate + `llm_rate_missing` alert on lookup miss;
   no-double-count assertion; `llm_cost_estimated`-rate and rate-audit observability.
   Backward-compatible (`.get("llm_cost_usd", 0)`).
2. **M2 — Cache savings on real GPU cost (production).** Additive nullable
   `job_result_cache.recompute_cost_usd` populated at completion; `cache.go` stats
   query uses it with `COALESCE(..., src.cost_usd)` legacy fallback;
   `CacheStats.gpu_savings_usd`; org-scoped; `cache_gpu_savings_usd` basis metric.
3. **M3 — Invoice line items + reconciliation (production).** Additive org-scoped
   `invoice_line_items` table; `InvoiceView.line_items[]`; per-org bounded periodic
   reconciliation that logs `billing_reconciliation_drift_usd` and alerts, never
   mutating historical invoices; Dodo amount unchanged.
4. **M4 — Cost dashboards (production).** `GET /v1/usage/cost*` +
   `/v1/billing/projection`, RLS-confined and `rs("usage:read")`-gated, pure reads
   over rollup tables; web UI (spend, by-processor, breakdown, projection vs budget,
   cache savings).

## 6. Verification / acceptance criteria

Acceptance is measured **end-to-end against a real worker and a real provider**, not
unit-only. Hermetic stub-provider tests run in CI; the real-provider e2e tests run
against a live provider in a gated integration lane. Every criterion is measurable
(exact-equality or bounded assertion), not "looks right".

**E2E — LLM pass-through metering (M1)**
- **Real-provider summarize (e2e).** A `summarize` job driven through the real
  `worker.py` against a live (non-stub) provider records **non-zero**
  `jobs.llm_prompt_tokens`/`llm_completion_tokens`, and `jobs.cost_usd` equals
  **compute + LLM measured e2e**: `abs(cost_usd - (duration × cost_usd_per_second +
  llm_prompt_tokens/1e6 × input_rate + llm_completion_tokens/1e6 × output_rate ×
  markup)) < ε`. Strictly `> duration × cost_usd_per_second` (compute alone).
- **Hermetic stub (CI).** Stub-provider jobs stay deterministic and priced from
  estimated tokens with no network, so the suite is reproducible offline.
- **Provider omits usage (failure path).** With a provider/proxy that drops `usage`,
  the job still bills, `jobs.llm_cost_estimated=true` is set, and `llm_cost_usd` is
  the token-estimate charge (non-zero) — degradation is graceful, never a job
  failure.
- **Rate-table miss (failure path).** A `model_version_id` with no rate row bills at
  the configured default rate and emits the `llm_rate_missing` alert/metric; the job
  is not dropped.
- **No double-count (failure path).** A job that reports `gpu_seconds` reports
  `llm_cost_usd = 0` (and vice-versa); assert `cost_usd` never adds a GPU and an LLM
  charge for the same unit of work.
- **Effective-date pricing.** A job completed while an older dated rate row is in
  effect prices at that row; changing the rate afterward does **not** alter the
  already-billed `jobs.llm_cost_usd` (historical as-billed rate preserved).

**E2E — cache savings (M2)**
- `GET /v1/cache/stats` `est_savings_usd` for a hit on a **GPU-sourced** transcript
  equals that source's GPU `cost_usd` (not a CPU flat rate); `gpu_savings_usd` is
  populated and ≥ the old figure for CPU-sourced entries; legacy entries with null
  basis still return via the `src.cost_usd` fallback (no regression).

**E2E — reconciliation & billing (M3)**
- **Reconciliation exact-equality (e2e).** For a test org, `SUM(
  usage_rollup_hourly.cost_usd)` over the month **==** the org's
  `invoices.total_usd`, **and** that figure **== the Dodo checkout amount**
  (`AmountCents/100`), verified through the real rollup → invoice → Dodo path.
- **Drift is observed, not corrected.** Injecting a deliberate mismatch trips the
  `billing_reconciliation_drift_usd` metric + alert for that org and leaves the
  historical invoice **unmodified** (immutability held).
- **Line items sum to total.** `InvoiceView.line_items[]` (compute vs LLM
  vs per-processor) sum exactly to `invoices.total_usd`.

**E2E — dashboards & isolation (M4)**
- `GET /v1/usage/cost/breakdown` returns compute vs LLM vs cache-saved that sum to
  the period spend; dashboard endpoints are pure reads (no per-request recompute),
  latency bounded by the requested window.
- **Multi-tenant isolation (RLS).** Org A calling `GET /v1/usage/cost*` /
  `/breakdown` / `/v1/billing/projection` **cannot read any of org B's spend** — a
  cross-org request returns only A's data (or 404), asserted as an explicit RLS
  confinement test; `invoice_line_items` and reconciliation are likewise per-org.

**E2E — budget enforcement**
- Budget hard-cap (`jobs.go:186`) trips on **LLM-inclusive** spend: an org whose
  LLM pass-through pushes month-to-date over its limit is hard-capped, proving the
  cap reads the combined `cost_usd`.

## 7. Dependencies, risks, open questions

- **Provider usage fields differ**; a provider that omits usage (or a proxied
  openai-compat host, `llm.py:280`) forces a token-estimate fallback — flag such
  jobs as `llm_cost_estimated=true` for transparency.
- **Rate-table drift**: provider prices change; store rates in DB with an effective
  date, not only env, so historical jobs keep their as-billed rate.
- **Double-count risk**: ensure `gpu_seconds` and `llm_cost_usd` are never both set
  for the same unit of work (transcribe is GPU-metered, text is LLM-metered — they
  are distinct processors, so this is naturally partitioned).
- **CPU-sourced cache savings** are an estimate by construction; document that
  `gpu_savings_usd` is "GPU cost avoided at current rates," not historical actuals.
- **Open**: default markup multiplier for LLM pass-through (1.0 vs. a margin)? Should
  the dashboard show list price or effective (post-cache) spend as the headline?

## 8. Effort

- M1 (LLM pass-through metering): ~1.5 weeks (worker + rate table/migration + config
  + fallback/alert paths + e2e tests).
- M2 (cache savings basis): ~1 week (worker write + cache.go query + metric + tests).
- M3 (invoice line items + reconciliation): ~1 week (table + reconciliation job +
  drift metric/alert + e2e tests).
- M4 (dashboard endpoints + web UI): ~2 weeks (RLS-gated reads + isolation tests +
  web UI).
- **Total: ~5–6 weeks**, largely additive and backward-compatible. Each milestone is
  independently production-quality and shippable; M1–M2 deliver the correctness wins
  and can ship first without holding back the full scope.
