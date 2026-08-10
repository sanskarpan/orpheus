import { orpheus, type StreamingSession } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { StreamingManager } from "./StreamingManager";
import { LiveStreamStudio } from "./LiveStreamStudio";

export const dynamic = "force-dynamic";

export default async function StreamingPage() {
  let sessions: StreamingSession[] = [];
  let error: string | null = null;
  try {
    sessions = (await orpheus.listStreaming()).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Platform"
        title="Streaming"
        sub="Capture live audio with a real-time waveform and finalize a transcription session."
      />
      <LiveStreamStudio />
      {error ? (
        <ErrorNotice title="Couldn't load streaming sessions" detail={error} />
      ) : (
        <StreamingManager initial={sessions} />
      )}
    </div>
  );
}
