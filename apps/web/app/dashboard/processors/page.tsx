import Link from "next/link";
import { orpheus, type ProcessorSummary } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";

export const dynamic = "force-dynamic";

export default async function ProcessorsPage() {
  let procs: ProcessorSummary[] = [];
  let error: string | null = null;
  try {
    procs = (await orpheus.listProcessors(100)).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Platform"
        title="Processors"
        sub="The catalog of audio processors available to your org, with versions and SLOs."
      />
      {error ? (
        <ErrorNotice title="Couldn't load the catalog" detail={error} />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {procs.map((p) => (
            <Link
              key={p.name}
              href={`/dashboard/processors/${p.name}`}
              className="panel card-hover flex flex-col gap-2 p-5"
            >
              <div className="flex items-center gap-2.5">
                <span className="flex h-8 w-8 items-center justify-center rounded-md border border-brass/30 bg-brass/10 font-mono text-brass">
                  {p.name.includes("transcribe") ? "✎" : "⚙"}
                </span>
                <span className="font-display font-semibold text-ink-hi">{p.display_name || p.name}</span>
              </div>
              <p className="line-clamp-2 text-sm text-ink-mid">{p.description || "—"}</p>
              <span className="mt-auto pt-2 font-mono text-2xs text-ink-lo">{p.name}</span>
            </Link>
          ))}
          {procs.length === 0 && <div className="text-sm text-ink-mid">No processors registered.</div>}
        </div>
      )}
    </div>
  );
}
