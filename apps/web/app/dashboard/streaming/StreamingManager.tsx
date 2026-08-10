"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { createStreamingAction, finalizeStreamingAction } from "@/app/actions/streaming";
import type { StreamingSession } from "@/lib/orpheus";
import { StatusBadge } from "@/components/primitives";
import { usd, absTime } from "@/lib/format";
import { clsx } from "@/lib/clsx";

export function StreamingManager({ initial }: { initial: StreamingSession[] }) {
  const router = useRouter();
  const [sessions, setSessions] = useState(initial);
  const [pending, start] = useTransition();
  const [error, setError] = useState<string | null>(null);
  const [openId, setOpenId] = useState<string | null>(null);
  const [transcript, setTranscript] = useState("");
  const [seconds, setSeconds] = useState("");

  const create = () =>
    start(async () => {
      setError(null);
      const r = await createStreamingAction();
      if (!r.ok) return setError(r.error);
      setSessions((cur) => [r.data, ...cur]);
      router.refresh();
    });

  const finalize = (id: string) =>
    start(async () => {
      setError(null);
      const secs = Number.parseFloat(seconds || "0");
      const r = await finalizeStreamingAction(id, transcript, Number.isFinite(secs) ? secs : 0);
      if (!r.ok) return setError(r.error);
      setSessions((cur) => cur.map((s) => (s.id === id ? r.data : s)));
      setOpenId(null);
      setTranscript("");
      setSeconds("");
      router.refresh();
    });

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <p className="max-w-2xl text-sm text-ink-mid">
          Past streaming sessions and their finalized transcripts. Use the live studio above to record a new
          one; you can also open a blank session and finalize it manually here.
        </p>
        <button onClick={create} disabled={pending} className="btn-brass shrink-0">
          + New session
        </button>
      </div>
      {error && <div className="rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{error}</div>}

      {sessions.length === 0 ? (
        <div className="panel px-5 py-14 text-center text-sm text-ink-mid">No streaming sessions yet.</div>
      ) : (
        <div className="space-y-3">
          {sessions.map((s) => {
            const active = s.status === "active" || s.status === "open" || s.status === "streaming";
            return (
              <div key={s.id} className="panel p-5">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div className="flex items-center gap-3">
                    <StatusBadge status={s.status} />
                    <span className="font-mono text-xs text-ink-hi">{s.id.slice(0, 12)}</span>
                  </div>
                  <div className="flex items-center gap-4 font-mono text-2xs text-ink-lo">
                    <span>cost {usd(s.cost_usd)}</span>
                    {s.audio_seconds ? <span>{s.audio_seconds.toFixed(1)}s audio</span> : null}
                    <span>{absTime(s.started_at)}</span>
                  </div>
                </div>

                {s.ws_url && (
                  <div className="mt-3 truncate rounded border border-hairline bg-ground/50 px-3 py-2 font-mono text-2xs text-ink-mid">
                    {s.ws_url}
                  </div>
                )}

                {s.transcript && (
                  <p className="mt-3 rounded-md border border-hairline bg-ground/40 p-3 text-sm text-ink-hi">
                    {s.transcript}
                  </p>
                )}

                {active && (
                  <div className="mt-4">
                    {openId === s.id ? (
                      <div className="space-y-2">
                        <textarea
                          value={transcript}
                          onChange={(e) => setTranscript(e.target.value)}
                          placeholder="Final transcript…"
                          rows={3}
                          className="input"
                        />
                        <div className="flex items-center gap-2">
                          <input
                            value={seconds}
                            onChange={(e) => setSeconds(e.target.value)}
                            placeholder="audio seconds"
                            className="input w-40"
                          />
                          <button onClick={() => finalize(s.id)} disabled={pending} className="btn-brass">
                            Finalize
                          </button>
                          <button onClick={() => setOpenId(null)} className="btn">
                            Cancel
                          </button>
                        </div>
                      </div>
                    ) : (
                      <button
                        onClick={() => setOpenId(s.id)}
                        className={clsx("btn")}
                      >
                        Finalize session
                      </button>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
