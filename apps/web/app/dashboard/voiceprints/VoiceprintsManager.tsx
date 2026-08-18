"use client";

import { useState, useTransition } from "react";
import { deleteSpeakerAction } from "@/app/actions/speakers";
import type { SpeakerProfile } from "@/lib/orpheus";
import { EmptyState } from "@/components/layout";
import { absTime } from "@/lib/format";

export function VoiceprintsManager({ initial }: { initial: SpeakerProfile[] }) {
  const [speakers, setSpeakers] = useState<SpeakerProfile[]>(initial);
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();

  const remove = (id: string) =>
    start(async () => {
      setError(null);
      const r = await deleteSpeakerAction(id);
      if (r.ok) {
        setSpeakers((cur) => cur.filter((s) => s.id !== id));
        setConfirmId(null);
      } else {
        setError(r.error);
      }
    });

  if (speakers.length === 0) {
    return (
      <EmptyState
        title="No enrolled voiceprints"
        sub="Voiceprints are created by running the speaker.enroll processor with a speaker name and consent."
      />
    );
  }

  return (
    <div className="space-y-3">
      {error && (
        <div className="rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{error}</div>
      )}
      <div className="panel overflow-hidden">
        <div className="hairline-b px-5 py-3">
          <span className="label">Enrolled speakers · {speakers.length}</span>
        </div>
        <table className="w-full text-sm">
          <thead>
            <tr className="label border-b border-hairline text-left">
              <th className="px-5 py-2.5 font-mono font-normal">Name</th>
              <th className="px-3 py-2.5 text-right font-mono font-normal">Samples</th>
              <th className="px-3 py-2.5 text-right font-mono font-normal">Dim</th>
              <th className="px-3 py-2.5 text-right font-mono font-normal">Enrolled</th>
              <th className="px-5 py-2.5 text-right font-mono font-normal">Erase</th>
            </tr>
          </thead>
          <tbody>
            {speakers.map((s) => (
              <tr key={s.id} className="border-b border-hairline/60 last:border-0">
                <td className="px-5 py-3">
                  <div className="font-medium text-ink-hi">{s.name}</div>
                  <div className="mt-0.5 font-mono text-2xs text-ink-lo">{s.id.slice(0, 8)}</div>
                </td>
                <td className="tnum px-3 py-3 text-right text-ink-mid">{s.sample_count}</td>
                <td className="tnum px-3 py-3 text-right text-ink-mid">{s.dim}</td>
                <td className="tnum px-3 py-3 text-right text-2xs text-ink-lo">{absTime(s.created_at)}</td>
                <td className="px-5 py-3 text-right">
                  {confirmId === s.id ? (
                    <span className="inline-flex items-center gap-2">
                      <button
                        onClick={() => remove(s.id)}
                        disabled={pending}
                        className="text-2xs font-mono uppercase text-fail transition-colors hover:text-fail/80"
                      >
                        confirm
                      </button>
                      <button
                        onClick={() => setConfirmId(null)}
                        disabled={pending}
                        className="text-2xs font-mono uppercase text-ink-lo hover:text-ink-mid"
                      >
                        cancel
                      </button>
                    </span>
                  ) : (
                    <button
                      onClick={() => setConfirmId(s.id)}
                      disabled={pending}
                      className="text-2xs font-mono uppercase text-ink-lo transition-colors hover:text-fail"
                    >
                      delete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
