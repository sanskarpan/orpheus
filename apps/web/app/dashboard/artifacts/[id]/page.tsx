import Link from "next/link";
import { notFound } from "next/navigation";
import { orpheus, OrpheusError, type SignedURL } from "@/lib/orpheus";
import { PageHeader } from "@/components/layout";
import { WaveBars } from "@/components/primitives";
import { bytes, absTime, duration } from "@/lib/format";

export const dynamic = "force-dynamic";

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between border-b border-hairline/60 py-2.5 last:border-0">
      <span className="label">{label}</span>
      <span className="text-right text-sm text-ink-hi">{children}</span>
    </div>
  );
}

export default async function ArtifactDetailPage({ params }: { params: { id: string } }) {
  let artifact;
  try {
    artifact = await orpheus.getArtifact(params.id);
  } catch (e) {
    if (e instanceof OrpheusError && e.status === 404) notFound();
    throw e;
  }

  let signed: SignedURL | null = null;
  try {
    signed = await orpheus.signedUrl(params.id, 1800);
  } catch {
    signed = null;
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader eyebrow="Artifact" title={artifact.id.slice(0, 12)} sub={artifact.content_type} />

      <div className="grid gap-6 lg:grid-cols-3">
        {/* Player */}
        <div className="panel p-6 lg:col-span-2">
          <div className="label mb-4">Playback</div>
          <div className="mb-5 h-20 opacity-80">
            <WaveBars bars={64} className="h-full" />
          </div>
          {signed ? (
            <audio controls preload="none" src={signed.url} className="w-full">
              Your browser doesn't support audio playback.
            </audio>
          ) : (
            <div className="rounded-md border border-hairline bg-ground/40 p-4 text-sm text-ink-mid">
              No signed URL available (the object may be sensitive or storage is unreachable).
            </div>
          )}
          {signed && (
            <a href={signed.url} download className="btn mt-4 inline-flex">
              ↓ Download
            </a>
          )}
        </div>

        {/* Metadata */}
        <div className="panel p-5">
          <div className="label mb-3">Metadata</div>
          <Row label="SHA-256">
            <span className="font-mono text-2xs text-ink-mid">
              {artifact.sha256 ? `${artifact.sha256.slice(0, 16)}…` : "pending"}
            </span>
          </Row>
          <Row label="Size">
            <span className="tnum">{bytes(artifact.size_bytes)}</span>
          </Row>
          <Row label="Content type">{artifact.content_type}</Row>
          {artifact.codec && <Row label="Codec">{artifact.codec}</Row>}
          {artifact.duration_seconds ? (
            <Row label="Duration">
              <span className="tnum">{duration(artifact.duration_seconds)}</span>
            </Row>
          ) : null}
          {artifact.sample_rate ? (
            <Row label="Sample rate">
              <span className="tnum">{artifact.sample_rate} Hz</span>
            </Row>
          ) : null}
          {artifact.channels ? (
            <Row label="Channels">
              <span className="tnum">{artifact.channels}</span>
            </Row>
          ) : null}
          <Row label="Created">
            <span className="tnum text-ink-mid">{absTime(artifact.created_at)}</span>
          </Row>
        </div>
      </div>

      <Link href="/dashboard/artifacts" className="inline-block text-sm text-ink-mid hover:text-brass">
        ← All artifacts
      </Link>
    </div>
  );
}
