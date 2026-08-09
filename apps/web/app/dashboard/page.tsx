import Link from "next/link";
import { orpheus, type APIKey, type Job, type Usage, type WebhookEndpoint } from "@/lib/orpheus";
import { PageHeader, StatTile, ErrorNotice } from "@/components/layout";
import { WaveBars, StatusBadge } from "@/components/primitives";
import { OnboardingChecklist } from "@/components/onboarding/OnboardingChecklist";
import { getAccount } from "@/lib/session";
import { usd, absTime, shortId } from "@/lib/format";

export const dynamic = "force-dynamic";

async function safe<T>(p: Promise<T>, fallback: T): Promise<T> {
  try {
    return await p;
  } catch {
    return fallback;
  }
}

const emptyList = { data: [] as never[], has_more: false };

export default async function OverviewPage() {
  const account = await getAccount();

  // Note: the list API's status filter accepts queued/running/completed/failed/
  // canceled only — `dead_letter` is a valid job *state* but not a filter value
  // (it 400s), so the recovery signal here is the count of `failed` jobs.
  const [usage, recent, failedJobs, keysList, hooksList, transcribed] = await Promise.all([
    safe<Usage | null>(orpheus.usage(), null),
    safe(orpheus.listJobs({ limit: 8 }), emptyList as { data: Job[]; has_more: boolean }),
    safe(orpheus.listJobs({ status: "failed", limit: 20 }), emptyList as { data: Job[]; has_more: boolean }),
    safe(orpheus.listKeys(100), { data: [] as APIKey[], has_more: false }),
    safe(orpheus.listWebhooks({ limit: 1 }), emptyList as { data: WebhookEndpoint[]; has_more: boolean }),
    safe(orpheus.listJobs({ processor: "transcribe", status: "completed", limit: 1 }), emptyList as { data: Job[]; has_more: boolean }),
  ]);

  const jobs = recent.data;
  const failedCount = failedJobs.data.length;

  const checklist = {
    ranJob: (usage?.jobs_count ?? 0) > 0,
    transcribed: transcribed.data.length > 0,
    // The owner key always exists, so "created a key" means more than one.
    createdKey: keysList.data.length > 1,
    addedWebhook: hooksList.data.length > 0,
  };

  return (
    <div className="reveal space-y-7">
      <PageHeader
        eyebrow="Workspace"
        title="Overview"
        sub="Live state of your Orpheus workspace — jobs, spend, and the pipeline at a glance."
        actions={
          <Link href="/dashboard/upload" className="btn-brass">
            ↥ Upload & Run
          </Link>
        }
      />

      {/* First-run onboarding (hides itself once every step is done) */}
      {account && <OnboardingChecklist state={checklist} name={account.name} />}

      {/* Hero + stat rail (asymmetric) */}
      <div className="grid gap-5 lg:grid-cols-3">
        <div className="panel relative overflow-hidden p-6 lg:col-span-2">
          <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-brass/[0.06] to-transparent" />
          <div className="relative flex h-full flex-col justify-between gap-6">
            <div>
              <div className="label mb-2">Pipeline</div>
              <h2 className="max-w-md font-display text-2xl font-bold leading-tight text-ink-hi">
                Your audio pipeline is <span className="text-ok">live</span> and listening.
              </h2>
              <p className="mt-2 max-w-md text-sm text-ink-mid">
                Drop a file, pick a processor, and read the transcript — every job is cached,
                audited, and RLS-isolated to your org.
              </p>
            </div>
            <div className="h-20">
              <WaveBars bars={56} className="h-full" />
            </div>
          </div>
        </div>

        <div className="grid grid-cols-2 gap-5 lg:grid-cols-1">
          <StatTile label="Jobs this month" value={String(usage?.jobs_count ?? 0)} accent />
          <StatTile label="Spend" value={usd(usage?.total_usd)} hint="current billing period" />
          <StatTile
            label="GPU seconds"
            value={(usage?.gpu_seconds ?? 0).toFixed(1)}
            unit="s"
          />
          <StatTile
            label="Failed jobs"
            value={String(failedCount > 0 ? `${failedCount}${failedJobs.has_more ? "+" : ""}` : 0)}
            hint={failedCount ? "needs attention" : "all clear"}
          />
        </div>
      </div>

      {/* Recent jobs */}
      <div className="panel overflow-hidden">
        <div className="hairline-b flex items-center justify-between px-5 py-3.5">
          <div className="label">Recent jobs</div>
          <Link href="/dashboard/jobs" className="text-xs text-ink-mid transition-colors hover:text-brass">
            View all →
          </Link>
        </div>

        {usage === null ? (
          <div className="p-5">
            <ErrorNotice
              title="Couldn't reach the API"
              detail={`Expected the Orpheus API at ${process.env.ORPHEUS_API_URL ?? "http://127.0.0.1:8090"} — is it running?`}
            />
          </div>
        ) : jobs.length === 0 ? (
          <div className="px-5 py-12 text-center text-sm text-ink-mid">
            No jobs yet.{" "}
            <Link href="/dashboard/upload" className="text-brass hover:underline">
              Run your first one →
            </Link>
          </div>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="label border-b border-hairline text-left">
                <th className="px-5 py-2.5 font-mono font-normal">Job</th>
                <th className="px-3 py-2.5 font-mono font-normal">Processor</th>
                <th className="px-3 py-2.5 font-mono font-normal">Status</th>
                <th className="px-3 py-2.5 text-right font-mono font-normal">Cost</th>
                <th className="px-5 py-2.5 text-right font-mono font-normal">Created</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((j) => (
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
                  <td className="px-3 py-3 text-ink-mid">{j.processor?.name}</td>
                  <td className="px-3 py-3">
                    <StatusBadge status={j.status} />
                  </td>
                  <td className="tnum px-3 py-3 text-right text-ink-mid">{usd(j.cost_usd)}</td>
                  <td className="tnum px-5 py-3 text-right text-ink-lo">{absTime(j.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
