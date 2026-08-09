import Link from "next/link";
import { orpheus, type Artifact } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { bytes, absTime, shortId, duration } from "@/lib/format";

export const dynamic = "force-dynamic";

export default async function ArtifactsPage({ searchParams }: { searchParams: { cursor?: string } }) {
  let page: { data: Artifact[]; has_more: boolean; next_cursor?: string } | null = null;
  let error: string | null = null;
  try {
    page = await orpheus.listArtifacts({ limit: 30, cursor: searchParams.cursor });
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader eyebrow="Workspace" title="Artifacts" sub="Stored audio inputs, addressable by content hash." />

      {error ? (
        <ErrorNotice title="Couldn't load artifacts" detail={error} />
      ) : !page || page.data.length === 0 ? (
        <div className="panel px-5 py-14 text-center text-sm text-ink-mid">
          No artifacts yet.{" "}
          <Link href="/dashboard/upload" className="text-brass hover:underline">
            Upload one →
          </Link>
        </div>
      ) : (
        <>
          <div className="panel overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="label border-b border-hairline text-left">
                  <th className="px-5 py-2.5 font-mono font-normal">Artifact</th>
                  <th className="px-3 py-2.5 font-mono font-normal">Type</th>
                  <th className="px-3 py-2.5 text-right font-mono font-normal">Size</th>
                  <th className="px-3 py-2.5 text-right font-mono font-normal">Duration</th>
                  <th className="px-5 py-2.5 text-right font-mono font-normal">Created</th>
                </tr>
              </thead>
              <tbody>
                {page.data.map((a) => (
                  <tr key={a.id} className="border-b border-hairline/60 last:border-0 hover:bg-panel-2">
                    <td className="px-5 py-3">
                      <Link href={`/dashboard/artifacts/${a.id}`} className="font-mono text-ink-hi hover:text-brass">
                        {shortId(a.id)}
                      </Link>
                    </td>
                    <td className="px-3 py-3 text-ink-mid">{a.content_type}</td>
                    <td className="tnum px-3 py-3 text-right text-ink-mid">{bytes(a.size_bytes)}</td>
                    <td className="tnum px-3 py-3 text-right text-ink-mid">
                      {a.duration_seconds ? duration(a.duration_seconds) : "—"}
                    </td>
                    <td className="tnum px-5 py-3 text-right text-ink-lo">{absTime(a.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {page.has_more && page.next_cursor && (
            <div className="flex justify-end">
              <Link href={`/dashboard/artifacts?cursor=${page.next_cursor}`} className="btn">
                Next page →
              </Link>
            </div>
          )}
        </>
      )}
    </div>
  );
}
