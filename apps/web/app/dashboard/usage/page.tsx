import { orpheus, type Usage, type Timeseries } from "@/lib/orpheus";
import { PageHeader, StatTile, ErrorNotice } from "@/components/layout";
import { UsageChart } from "@/components/UsageChart";
import { usd, absTime } from "@/lib/format";

export const dynamic = "force-dynamic";

const METRICS = [
  { key: "cost_usd", label: "Spend (USD)" },
  { key: "jobs", label: "Jobs" },
  { key: "compute_seconds", label: "Compute (s)" },
] as const;

export default async function UsagePage({
  searchParams,
}: {
  searchParams: { metric?: string; granularity?: string };
}) {
  const metric = (METRICS.find((m) => m.key === searchParams.metric)?.key ?? "cost_usd") as
    | "cost_usd"
    | "jobs"
    | "compute_seconds";
  const granularity = searchParams.granularity === "hour" ? "hour" : "day";

  let usage: Usage | null = null;
  let ts: Timeseries | null = null;
  let error: string | null = null;
  try {
    [usage, ts] = await Promise.all([orpheus.usage(), orpheus.timeseries({ granularity, group_by: "total" })]);
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  const series = ts?.series ?? [];
  const totalCost = series.reduce((a, p) => a + (p.cost_usd ?? 0), 0);
  const totalJobs = series.reduce((a, p) => a + (p.jobs ?? 0), 0);

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Workspace"
        title="Usage"
        sub="Spend and throughput over time. Billing-period totals plus a day-by-day breakdown."
      />

      {error ? (
        <ErrorNotice title="Couldn't load usage" detail={error} />
      ) : (
        <>
          <div className="grid gap-5 sm:grid-cols-4">
            <StatTile label="Jobs (month)" value={String(usage?.jobs_count ?? 0)} accent />
            <StatTile label="Spend (month)" value={usd(usage?.total_usd)} />
            <StatTile label="GPU seconds" value={(usage?.gpu_seconds ?? 0).toFixed(1)} unit="s" />
            <StatTile label="Window total" value={usd(totalCost)} hint={`${totalJobs} jobs charted`} />
          </div>

          <div className="panel p-6">
            <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
              <div className="label">
                {ts?.granularity ?? "day"} · {absTime(ts?.from)} → {absTime(ts?.to)}
              </div>
              <div className="flex gap-2">
                {METRICS.map((m) => (
                  <a
                    key={m.key}
                    href={`/dashboard/usage?metric=${m.key}&granularity=${granularity}`}
                    className={`rounded-md border px-2.5 py-1 text-2xs font-mono uppercase tracking-wider transition-colors ${
                      m.key === metric
                        ? "border-brass/50 bg-brass/10 text-brass"
                        : "border-hairline-2 text-ink-mid hover:text-ink-hi"
                    }`}
                  >
                    {m.label}
                  </a>
                ))}
              </div>
            </div>
            <UsageChart points={series} metric={metric} />
          </div>

          {/* Breakdown table */}
          {series.length > 0 && (
            <div className="panel overflow-hidden">
              <div className="hairline-b px-5 py-3">
                <span className="label">Breakdown</span>
              </div>
              <div className="max-h-96 overflow-auto">
                <table className="w-full text-sm">
                  <thead className="sticky top-0 bg-panel">
                    <tr className="label border-b border-hairline text-left">
                      <th className="px-5 py-2.5 font-mono font-normal">Bucket</th>
                      <th className="px-3 py-2.5 text-right font-mono font-normal">Jobs</th>
                      <th className="px-3 py-2.5 text-right font-mono font-normal">Compute</th>
                      <th className="px-5 py-2.5 text-right font-mono font-normal">Cost</th>
                    </tr>
                  </thead>
                  <tbody>
                    {[...series].reverse().map((p, i) => (
                      <tr key={i} className="border-b border-hairline/60 last:border-0">
                        <td className="tnum px-5 py-2.5 text-ink-mid">{absTime(p.bucket)}</td>
                        <td className="tnum px-3 py-2.5 text-right text-ink-mid">{p.jobs}</td>
                        <td className="tnum px-3 py-2.5 text-right text-ink-mid">{Number(p.compute_seconds ?? 0).toFixed(1)}s</td>
                        <td className="tnum px-5 py-2.5 text-right text-ink-hi">{usd(p.cost_usd)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}
