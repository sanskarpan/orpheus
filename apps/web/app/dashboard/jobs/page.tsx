import Link from "next/link";
import { orpheus, type Job } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { StatusBadge } from "@/components/primitives";
import { usd, absTime, shortId } from "@/lib/format";

export const dynamic = "force-dynamic";

// The list API rejects `dead_letter` as a filter value (only these are valid).
const STATUSES = ["", "queued", "running", "completed", "failed", "canceled"];

export default async function JobsPage({
  searchParams,
}: {
  searchParams: { status?: string; processor?: string; cursor?: string };
}) {
  const status = searchParams.status ?? "";
  const processor = searchParams.processor ?? "";
  const cursor = searchParams.cursor;

  let page: { data: Job[]; has_more: boolean; next_cursor?: string } | null = null;
  let error: string | null = null;
  try {
    page = await orpheus.listJobs({ limit: 30, status: status || undefined, processor: processor || undefined, cursor });
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  const qp = (extra: Record<string, string | undefined>) => {
    const p = new URLSearchParams();
    if (status) p.set("status", status);
    if (processor) p.set("processor", processor);
    for (const [k, v] of Object.entries(extra)) {
      if (v) p.set(k, v);
      else p.delete(k);
    }
    const s = p.toString();
    return s ? `?${s}` : "";
  };

  return (
    <div className="reveal space-y-6">
      <PageHeader eyebrow="Workspace" title="Jobs" sub="Every processing job in your org, newest first." />

      {/* Filters (GET form — works without JS) */}
      <form className="panel flex flex-wrap items-end gap-4 p-4">
        <div>
          <label className="label mb-1.5 block">Status</label>
          <select name="status" defaultValue={status} className="input w-44">
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {s || "all"}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="label mb-1.5 block">Processor</label>
          <input name="processor" defaultValue={processor} placeholder="e.g. transcribe" className="input w-56" />
        </div>
        <button type="submit" className="btn">
          Apply
        </button>
        {(status || processor) && (
          <Link href="/dashboard/jobs" className="text-xs text-ink-lo hover:text-ink-mid">
            clear
          </Link>
        )}
      </form>

      {error ? (
        <ErrorNotice title="Couldn't load jobs" detail={error} />
      ) : !page || page.data.length === 0 ? (
        <div className="panel px-5 py-14 text-center text-sm text-ink-mid">
          No jobs match.{" "}
          <Link href="/dashboard/upload" className="text-brass hover:underline">
            Run one →
          </Link>
        </div>
      ) : (
        <>
          <div className="panel overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="label border-b border-hairline text-left">
                  <th className="px-5 py-2.5 font-mono font-normal">Job</th>
                  <th className="px-3 py-2.5 font-mono font-normal">Processor</th>
                  <th className="px-3 py-2.5 font-mono font-normal">Status</th>
                  <th className="px-3 py-2.5 text-right font-mono font-normal">Attempts</th>
                  <th className="px-3 py-2.5 text-right font-mono font-normal">Cost</th>
                  <th className="px-5 py-2.5 text-right font-mono font-normal">Created</th>
                </tr>
              </thead>
              <tbody>
                {page.data.map((j) => (
                  <tr key={j.id} className="border-b border-hairline/60 last:border-0 hover:bg-panel-2">
                    <td className="px-5 py-3">
                      <Link href={`/dashboard/jobs/${j.id}`} className="font-mono text-ink-hi hover:text-brass">
                        {shortId(j.id)}
                      </Link>
                      {j.cache === "hit" && (
                        <span className="ml-2 rounded border border-brass/30 px-1 py-0.5 text-2xs font-mono uppercase text-brass">
                          cached
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-3 text-ink-mid">
                      {j.processor?.name}
                      <span className="ml-1 text-ink-lo">v{j.processor?.version}</span>
                    </td>
                    <td className="px-3 py-3">
                      <StatusBadge status={j.status} />
                    </td>
                    <td className="tnum px-3 py-3 text-right text-ink-mid">
                      {j.attempts}/{j.max_retries}
                    </td>
                    <td className="tnum px-3 py-3 text-right text-ink-mid">{usd(j.cost_usd)}</td>
                    <td className="tnum px-5 py-3 text-right text-ink-lo">{absTime(j.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <div className="flex items-center justify-between">
            <span className="text-2xs font-mono text-ink-lo">{page.data.length} shown</span>
            {page.has_more && page.next_cursor && (
              <Link href={`/dashboard/jobs${qp({ cursor: page.next_cursor })}`} className="btn">
                Next page →
              </Link>
            )}
          </div>
        </>
      )}
    </div>
  );
}
