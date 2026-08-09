import { clsx } from "@/lib/clsx";

/* Renders a job `result` intelligently: a transcript view when it looks like
 * an ASR result, otherwise a syntax-tinted JSON viewer. */

interface Segment {
  id?: number;
  start?: number;
  end?: number;
  text?: string;
}

interface TranscriptShape {
  text?: string;
  language?: string;
  duration?: number;
  segments?: Segment[];
}

function looksLikeTranscript(r: unknown): r is TranscriptShape {
  if (!r || typeof r !== "object") return false;
  const o = r as Record<string, unknown>;
  return typeof o.text === "string" || Array.isArray(o.segments);
}

function ts(sec: number | undefined): string {
  if (sec === undefined) return "--:--";
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

function JsonView({ value }: { value: unknown }) {
  return (
    <pre className="max-h-[28rem] overflow-auto rounded-md border border-hairline bg-ground/60 p-4 font-mono text-xs leading-relaxed text-ink-mid">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}

export function ResultView({ result }: { result: unknown }) {
  if (result === undefined || result === null) {
    return <div className="text-sm text-ink-lo">No result payload.</div>;
  }

  if (looksLikeTranscript(result)) {
    const t = result;
    const segs = t.segments ?? [];
    return (
      <div className="space-y-5">
        {/* Transcript prose */}
        {t.text && (
          <div>
            <div className="mb-2 flex items-center gap-3">
              <span className="label">Transcript</span>
              {t.language && (
                <span className="rounded border border-hairline-2 px-1.5 py-0.5 font-mono text-2xs uppercase text-ink-mid">
                  {t.language}
                </span>
              )}
              {typeof t.duration === "number" && (
                <span className="font-mono text-2xs text-ink-lo">{t.duration.toFixed(1)}s audio</span>
              )}
            </div>
            <p className="rounded-md border border-hairline bg-ground/40 p-4 text-[15px] leading-relaxed text-ink-hi">
              {t.text}
            </p>
          </div>
        )}

        {/* Segment table with timestamps */}
        {segs.length > 0 && (
          <div>
            <div className="label mb-2">Segments · {segs.length}</div>
            <div className="max-h-96 overflow-auto rounded-md border border-hairline">
              <table className="w-full text-sm">
                <tbody>
                  {segs.map((s, i) => (
                    <tr
                      key={s.id ?? i}
                      className={clsx("border-b border-hairline/60 last:border-0", i % 2 === 1 && "bg-panel-2/40")}
                    >
                      <td className="tnum whitespace-nowrap px-3 py-2 align-top text-2xs text-brass">
                        {ts(s.start)} → {ts(s.end)}
                      </td>
                      <td className="px-3 py-2 text-ink-hi">{s.text?.trim()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </div>
    );
  }

  return <JsonView value={result} />;
}
