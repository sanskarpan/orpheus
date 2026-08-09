import Link from "next/link";
import { orpheus, type Job } from "@/lib/orpheus";
import { PageHeader, StatTile, ErrorNotice } from "@/components/layout";
import { StatusBadge } from "@/components/primitives";
import { absTime, shortId } from "@/lib/format";
import { RequeueButton } from "./RequeueButton";
import { ProvisionPanel } from "./ProvisionPanel";

export const dynamic = "force-dynamic";

export default async function OpsPage() {
  // The list API's status filter rejects `dead_letter` (only queued/running/
  // completed/failed/canceled are queryable), so the recovery queue is built
  // from `failed` jobs — which is exactly what Requeue accepts.
  let failed: Job[] = [];
  let error: string | null = null;
  try {
    failed = (await orpheus.listJobs({ status: "failed", limit: 50 })).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Operations"
        title="Control Room"
        sub="Failed-job recovery and tenant provisioning. Admin-only."
      />

      {error ? (
        <ErrorNotice title="Couldn't load operations data" detail={error} />
      ) : (
        <>
          <div className="grid gap-5 sm:grid-cols-3">
            <StatTile
              label="Recovery queue"
              value={String(failed.length)}
              accent={failed.length > 0}
              hint={failed.length ? "failed · requeue-able" : "clear"}
            />
            <StatTile label="Audit" value="→" hint="view the log" />
            <StatTile label="Provision" value="+" hint="mint a tenant" />
          </div>

          <div className="grid gap-6 lg:grid-cols-3">
            {/* DLQ */}
            <div className="lg:col-span-2">
              <div className="panel overflow-hidden">
                <div className="hairline-b flex items-center justify-between px-5 py-3">
                  <span className="label">Recovery queue · failed jobs</span>
                  <Link href="/dashboard/ops/audit" className="text-xs text-ink-mid hover:text-brass">
                    Audit log →
                  </Link>
                </div>
                {failed.length === 0 ? (
                  <div className="px-5 py-12 text-center text-sm text-ink-mid">
                    Queue is empty — no failed jobs to recover.
                  </div>
                ) : (
                  <table className="w-full text-sm">
                    <tbody>
                      {failed.map((j) => (
                        <tr key={j.id} className="border-b border-hairline/60 last:border-0">
                          <td className="px-5 py-3">
                            <Link href={`/dashboard/jobs/${j.id}`} className="font-mono text-ink-hi hover:text-brass">
                              {shortId(j.id)}
                            </Link>
                            <div className="mt-0.5 text-2xs text-ink-lo">{j.processor?.name}</div>
                          </td>
                          <td className="px-3 py-3">
                            <StatusBadge status={j.status} />
                          </td>
                          <td className="tnum px-3 py-3 text-2xs text-ink-lo">{absTime(j.created_at)}</td>
                          <td className="px-5 py-3 text-right">
                            <RequeueButton id={j.id} />
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
            </div>

            {/* Provision */}
            <ProvisionPanel />
          </div>
        </>
      )}
    </div>
  );
}
