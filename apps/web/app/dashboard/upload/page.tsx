import { orpheus } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { UploadStudio, type ProcessorOption } from "./UploadStudio";

export const dynamic = "force-dynamic";

export default async function UploadPage() {
  let processors: ProcessorOption[] = [];
  let failed = false;

  try {
    const list = await orpheus.listProcessors(50);
    // Resolve versions for each processor so the client can submit a job.
    processors = await Promise.all(
      list.data.map(async (p) => {
        try {
          const detail = await orpheus.getProcessor(p.name);
          return {
            name: p.name,
            display_name: p.display_name,
            description: p.description,
            versions: (detail.versions ?? []).map((v) => ({ version: v.version })),
          };
        } catch {
          return { name: p.name, display_name: p.display_name, description: p.description, versions: [] };
        }
      }),
    );
    // Surface transcribe-style processors first.
    processors.sort((a, b) => {
      const at = a.name.includes("transcribe") ? 0 : 1;
      const bt = b.name.includes("transcribe") ? 0 : 1;
      return at - bt || a.name.localeCompare(b.name);
    });
  } catch {
    failed = true;
  }

  return (
    <div className="reveal">
      <PageHeader
        eyebrow="Workspace"
        title="Upload & Run"
        sub="Drop an audio file, choose a processor, and watch the job run to a rendered result."
      />
      {failed ? (
        <ErrorNotice
          title="Couldn't load processors"
          detail="The API didn't return the processor catalog. Confirm it's running and your key has access."
        />
      ) : (
        <UploadStudio processors={processors} />
      )}
    </div>
  );
}
