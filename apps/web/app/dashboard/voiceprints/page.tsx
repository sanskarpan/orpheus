import { orpheus, type SpeakerProfile } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { VoiceprintsManager } from "./VoiceprintsManager";

export const dynamic = "force-dynamic";

export default async function VoiceprintsPage() {
  let speakers: SpeakerProfile[] = [];
  let error: string | null = null;
  try {
    speakers = (await orpheus.listSpeakers()).speaker_profiles ?? [];
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Platform"
        title="Voiceprints"
        sub="Enrolled speaker profiles (biometric). Deleting a voiceprint is a permanent GDPR erasure of that speaker's embedding."
      />
      {error ? (
        <ErrorNotice title="Couldn't load voiceprints" detail={error} />
      ) : (
        <VoiceprintsManager initial={speakers} />
      )}
    </div>
  );
}
