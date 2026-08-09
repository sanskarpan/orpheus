import Link from "next/link";
import { notFound } from "next/navigation";
import { orpheus, OrpheusError } from "@/lib/orpheus";
import { PageHeader } from "@/components/layout";
import { usd, absTime } from "@/lib/format";

export const dynamic = "force-dynamic";

export default async function ProcessorDetailPage({ params }: { params: { name: string } }) {
  let proc;
  try {
    proc = await orpheus.getProcessor(params.name);
  } catch (e) {
    if (e instanceof OrpheusError && e.status === 404) notFound();
    throw e;
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Processor"
        title={proc.display_name || proc.name}
        sub={proc.description}
        actions={
          <Link href={`/dashboard/upload?proc=${proc.name}`} className="btn-brass">
            ↥ Run this
          </Link>
        }
      />

      <div className="panel overflow-hidden">
        <div className="hairline-b px-5 py-3">
          <span className="label">Versions</span>
        </div>
        <table className="w-full text-sm">
          <thead>
            <tr className="label border-b border-hairline text-left">
              <th className="px-5 py-2.5 font-mono font-normal">Version</th>
              <th className="px-3 py-2.5 font-mono font-normal">Model</th>
              <th className="px-3 py-2.5 text-right font-mono font-normal">SLO p95</th>
              <th className="px-3 py-2.5 text-right font-mono font-normal">SLO p99</th>
              <th className="px-3 py-2.5 text-right font-mono font-normal">Cost</th>
              <th className="px-5 py-2.5 text-right font-mono font-normal">Added</th>
            </tr>
          </thead>
          <tbody>
            {proc.versions?.map((v) => (
              <tr key={v.version} className="border-b border-hairline/60 last:border-0">
                <td className="px-5 py-3 font-mono text-ink-hi">
                  v{v.version}
                  {v.deprecated_at && (
                    <span className="ml-2 rounded border border-fail/30 px-1 py-0.5 text-2xs uppercase text-fail">
                      deprecated
                    </span>
                  )}
                </td>
                <td className="px-3 py-3 font-mono text-2xs text-ink-mid">{v.model_id || "—"}</td>
                <td className="tnum px-3 py-3 text-right text-ink-mid">{Number(v.slo_p95_seconds ?? 0).toFixed(1)}s</td>
                <td className="tnum px-3 py-3 text-right text-ink-mid">{Number(v.slo_p99_seconds ?? 0).toFixed(1)}s</td>
                <td className="tnum px-3 py-3 text-right text-ink-mid">{usd(v.cost_usd)}</td>
                <td className="tnum px-5 py-3 text-right text-ink-lo">{absTime(v.created_at)}</td>
              </tr>
            ))}
            {(!proc.versions || proc.versions.length === 0) && (
              <tr>
                <td colSpan={6} className="px-5 py-8 text-center text-sm text-ink-lo">
                  No published versions.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <Link href="/dashboard/processors" className="inline-block text-sm text-ink-mid hover:text-brass">
        ← All processors
      </Link>
    </div>
  );
}
