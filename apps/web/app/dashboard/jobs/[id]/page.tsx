import Link from "next/link";
import { notFound } from "next/navigation";
import { orpheus, OrpheusError } from "@/lib/orpheus";
import { PageHeader } from "@/components/layout";
import { ResultView } from "@/components/ResultView";
import { JobActions } from "@/components/JobActions";
import { RunAnalysis, type AnalysisProcessor } from "@/components/RunAnalysis";
import { StatusBadge } from "@/components/primitives";
import { isChainable } from "@/lib/processorParams";
import { usd, absTime } from "@/lib/format";

export const dynamic = "force-dynamic";

/** Chainable text/analysis processors, resolved with a runnable version. */
async function loadAnalysisProcessors(): Promise<AnalysisProcessor[]> {
  try {
    const list = (await orpheus.listProcessors(100)).data.filter((p) => isChainable(p.name));
    const resolved: (AnalysisProcessor | null)[] = await Promise.all(
      list.map(async (p) => {
        try {
          const detail = await orpheus.getProcessor(p.name);
          const version = detail.versions?.[0]?.version;
          if (!version) return null;
          const out: AnalysisProcessor = {
            name: p.name,
            display_name: p.display_name || p.name,
            version,
            input_schema: detail.input_schema,
          };
          return out;
        } catch {
          return null;
        }
      }),
    );
    return resolved
      .filter((p): p is AnalysisProcessor => p !== null)
      .sort((a, b) => a.name.localeCompare(b.name));
  } catch {
    return [];
  }
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-hairline/60 py-2.5 last:border-0">
      <span className="label pt-0.5">{label}</span>
      <span className="text-right text-sm text-ink-hi">{children}</span>
    </div>
  );
}

export default async function JobDetailPage({ params }: { params: { id: string } }) {
  let job;
  try {
    job = await orpheus.getJob(params.id);
  } catch (e) {
    if (e instanceof OrpheusError && e.status === 404) notFound();
    throw e;
  }

  const canChain = job.status === "completed" && !!job.artifact_id;
  const analysisProcessors = canChain ? await loadAnalysisProcessors() : [];

  const timeline: { label: string; at?: string }[] = [
    { label: "Created", at: job.created_at },
    { label: "Started", at: job.started_at },
    { label: "Completed", at: job.completed_at },
    { label: "Updated", at: job.updated_at },
  ];

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow={job.processor.name}
        title="Job detail"
        actions={<JobActions id={job.id} status={job.status} />}
      />

      <div className="flex flex-wrap items-center gap-3">
        <StatusBadge status={job.status} />
        <span className="font-mono text-xs text-ink-mid">{job.id}</span>
        {job.cache === "hit" && (
          <span className="rounded border border-brass/30 px-1.5 py-0.5 font-mono text-2xs uppercase text-brass">
            cache hit
          </span>
        )}
      </div>

      <div className="grid gap-6 lg:grid-cols-3">
        {/* Result — the main event */}
        <div className="panel p-6 lg:col-span-2">
          <div className="label mb-4">Result</div>
          {job.status === "completed" ? (
            <ResultView result={job.result} />
          ) : job.status === "failed" || job.status === "dead_letter" ? (
            <div className="space-y-3">
              <div className="rounded-md border border-fail/30 bg-fail/10 p-4 text-sm text-fail">
                Job {job.status.replace("_", " ")} after {job.attempts} attempt
                {job.attempts === 1 ? "" : "s"}.
              </div>
              {job.result ? <ResultView result={job.result} /> : null}
            </div>
          ) : (
            <div className="flex items-center gap-3 py-8 text-sm text-ink-mid">
              <span className="inline-block h-3.5 w-3.5 animate-spin-slow rounded-full border-2 border-brass/30 border-t-brass" />
              Job is {job.status}. This page refreshes automatically…
            </div>
          )}
        </div>

        {/* Meta rail */}
        <div className="space-y-6">
          <div className="panel p-5">
            <div className="label mb-3">Execution</div>
            <div>
              <Row label="Processor">
                {job.processor.name}
                <span className="ml-1 text-ink-lo">v{job.processor.version}</span>
              </Row>
              <Row label="Cost">
                <span className="tnum">{usd(job.cost_usd)}</span>
              </Row>
              <Row label="Attempts">
                <span className="tnum">
                  {job.attempts}/{job.max_retries}
                </span>
              </Row>
              {job.cache && <Row label="Cache">{job.cache}</Row>}
              {job.cached_from_job_id && (
                <Row label="Cached from">
                  <Link href={`/dashboard/jobs/${job.cached_from_job_id}`} className="font-mono text-xs text-brass">
                    {job.cached_from_job_id.slice(0, 8)}
                  </Link>
                </Row>
              )}
              {job.artifact_id && (
                <Row label="Artifact">
                  <Link href={`/dashboard/artifacts/${job.artifact_id}`} className="font-mono text-xs text-brass">
                    {job.artifact_id.slice(0, 8)}
                  </Link>
                </Row>
              )}
            </div>
          </div>

          <div className="panel p-5">
            <div className="label mb-3">Timeline</div>
            <div>
              {timeline.map((t) => (
                <Row key={t.label} label={t.label}>
                  <span className="tnum text-ink-mid">{absTime(t.at)}</span>
                </Row>
              ))}
            </div>
          </div>

          {canChain && job.artifact_id && analysisProcessors.length > 0 && (
            <RunAnalysis
              sourceJobId={job.id}
              artifactId={job.artifact_id}
              processors={analysisProcessors}
            />
          )}

          {job.params != null && (
            <div className="panel p-5">
              <div className="label mb-3">Params</div>
              <pre className="max-h-48 overflow-auto rounded-md border border-hairline bg-ground/60 p-3 font-mono text-2xs text-ink-mid">
                {JSON.stringify(job.params, null, 2)}
              </pre>
            </div>
          )}
        </div>
      </div>

      <Link href="/dashboard/jobs" className="inline-block text-sm text-ink-mid transition-colors hover:text-brass">
        ← All jobs
      </Link>
    </div>
  );
}
