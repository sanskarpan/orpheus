import Link from "next/link";
import { orpheus, type AuditEntry } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { Badge } from "@/components/primitives";
import { absTime } from "@/lib/format";

export const dynamic = "force-dynamic";

export default async function AuditPage({
  searchParams,
}: {
  searchParams: { action?: string; resource_type?: string; cursor?: string };
}) {
  const { action = "", resource_type = "", cursor } = searchParams;

  let page: { data: AuditEntry[]; has_more: boolean; next_cursor?: string } | null = null;
  let error: string | null = null;
  try {
    page = await orpheus.listAudit({
      limit: 40,
      action: action || undefined,
      resource_type: resource_type || undefined,
      cursor,
    });
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader eyebrow="Operations" title="Audit Log" sub="Every state-changing request, immutably recorded." />

      <form className="panel flex flex-wrap items-end gap-4 p-4">
        <div>
          <label className="label mb-1.5 block">Action</label>
          <input name="action" defaultValue={action} placeholder="job.create" className="input w-48" />
        </div>
        <div>
          <label className="label mb-1.5 block">Resource type</label>
          <input name="resource_type" defaultValue={resource_type} placeholder="job" className="input w-40" />
        </div>
        <button type="submit" className="btn">
          Filter
        </button>
        {(action || resource_type) && (
          <Link href="/dashboard/ops/audit" className="text-xs text-ink-lo hover:text-ink-mid">
            clear
          </Link>
        )}
      </form>

      {error ? (
        <ErrorNotice title="Couldn't load the audit log" detail={error} />
      ) : !page || page.data.length === 0 ? (
        <div className="panel px-5 py-14 text-center text-sm text-ink-mid">No audit entries match.</div>
      ) : (
        <>
          <div className="panel overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="label border-b border-hairline text-left">
                  <th className="px-5 py-2.5 font-mono font-normal">Action</th>
                  <th className="px-3 py-2.5 font-mono font-normal">Resource</th>
                  <th className="px-3 py-2.5 font-mono font-normal">Actor</th>
                  <th className="px-3 py-2.5 font-mono font-normal">IP</th>
                  <th className="px-5 py-2.5 text-right font-mono font-normal">When</th>
                </tr>
              </thead>
              <tbody>
                {page.data.map((e) => (
                  <tr key={e.id} className="border-b border-hairline/60 last:border-0 hover:bg-panel-2">
                    <td className="px-5 py-3 font-mono text-ink-hi">{e.action}</td>
                    <td className="px-3 py-3">
                      <div className="flex items-center gap-2">
                        <Badge>{e.resource_type || "—"}</Badge>
                        {e.resource_id && <span className="font-mono text-2xs text-ink-lo">{e.resource_id.slice(0, 8)}</span>}
                      </div>
                    </td>
                    <td className="px-3 py-3">
                      <span className="font-mono text-2xs text-ink-mid">{e.actor_type}</span>
                      <span className="ml-1 font-mono text-2xs text-ink-lo">{e.actor_id?.slice(0, 8)}</span>
                    </td>
                    <td className="px-3 py-3 font-mono text-2xs text-ink-lo">{e.ip || "—"}</td>
                    <td className="tnum px-5 py-3 text-right text-2xs text-ink-lo">{absTime(e.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {page.has_more && page.next_cursor && (
            <div className="flex justify-end">
              <Link
                href={`/dashboard/ops/audit?cursor=${page.next_cursor}${action ? `&action=${action}` : ""}${resource_type ? `&resource_type=${resource_type}` : ""}`}
                className="btn"
              >
                Next page →
              </Link>
            </div>
          )}
        </>
      )}
    </div>
  );
}
