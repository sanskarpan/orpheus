import Link from "next/link";
import { clsx } from "@/lib/clsx";

/* Renders a job `result` intelligently: a purpose-built view when the payload
 * matches a known processor-output shape (transcript, chapters, emotion, events,
 * moderation, scorecard, crm, diarization, enhance/edit), otherwise a
 * syntax-tinted JSON viewer. Detection is by distinctive top-level keys, checked
 * most-specific first (several shapes carry a `segments` array). */

function ts(sec: number | undefined): string {
  if (sec === undefined || sec === null) return "--:--";
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

function isObj(r: unknown): r is Record<string, unknown> {
  return !!r && typeof r === "object" && !Array.isArray(r);
}

function JsonView({ value }: { value: unknown }) {
  return (
    <pre className="max-h-[28rem] overflow-auto rounded-md border border-hairline bg-ground/60 p-4 font-mono text-xs leading-relaxed text-ink-mid">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return <div className="label mb-2">{children}</div>;
}

/* ---------- transcript ---------- */

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

function looksLikeTranscript(r: Record<string, unknown>): boolean {
  return (
    typeof r.text === "string" ||
    (Array.isArray(r.segments) && !(isObj(r.segments[0]) && "speaker" in r.segments[0]))
  );
}

function TranscriptView({ t }: { t: TranscriptShape }) {
  const segs = t.segments ?? [];
  return (
    <div className="space-y-5">
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

/* ---------- shared bits ---------- */

function TimeRange({ start, end }: { start?: number; end?: number }) {
  return (
    <span className="tnum whitespace-nowrap font-mono text-2xs text-brass">
      {ts(start)} → {ts(end)}
    </span>
  );
}

function Pills({ items }: { items: string[] }) {
  return (
    <ul className="space-y-1.5">
      {items.map((it, i) => (
        <li key={i} className="flex gap-2 text-sm text-ink-hi">
          <span className="text-brass">•</span>
          <span>{it}</span>
        </li>
      ))}
    </ul>
  );
}

function ArtifactLink({ id }: { id: string }) {
  return (
    <Link href={`/dashboard/artifacts/${id}`} className="font-mono text-xs text-brass hover:underline">
      {id.slice(0, 8)}…
    </Link>
  );
}

function MetricsGrid({ metrics }: { metrics: Record<string, unknown> }) {
  const entries = Object.entries(metrics);
  if (entries.length === 0) return null;
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
      {entries.map(([k, v]) => (
        <div key={k} className="rounded-md border border-hairline bg-ground/40 p-3">
          <div className="label">{k.replace(/_/g, " ")}</div>
          <div className="mt-0.5 tnum text-sm text-ink-hi">
            {typeof v === "number" ? v.toFixed(v % 1 === 0 ? 0 : 2) : String(v)}
          </div>
        </div>
      ))}
    </div>
  );
}

/* ---------- chapters ---------- */

interface Chapter {
  start?: number;
  end?: number;
  title?: string;
  summary?: string;
}
function ChaptersView({ chapters }: { chapters: Chapter[] }) {
  return (
    <div>
      <SectionLabel>Chapters · {chapters.length}</SectionLabel>
      <div className="space-y-2.5">
        {chapters.map((c, i) => (
          <div key={i} className="rounded-md border border-hairline bg-ground/40 p-3.5">
            <div className="flex items-center justify-between gap-3">
              <span className="font-medium text-ink-hi">{c.title || `Chapter ${i + 1}`}</span>
              <TimeRange start={c.start} end={c.end} />
            </div>
            {c.summary && <p className="mt-1 text-sm leading-relaxed text-ink-mid">{c.summary}</p>}
          </div>
        ))}
      </div>
    </div>
  );
}

/* ---------- emotion ---------- */

interface EmotionSegment {
  start?: number;
  end?: number;
  emotion?: string;
  score?: number;
  confidence?: number;
}
function EmotionView({ r }: { r: { segments?: EmotionSegment[]; dominant_emotion?: string } }) {
  const segs = r.segments ?? [];
  return (
    <div className="space-y-4">
      {r.dominant_emotion && (
        <div className="flex items-center gap-2">
          <span className="label">Dominant</span>
          <span className="rounded border border-brass/40 bg-brass/10 px-2 py-0.5 text-sm text-brass">
            {r.dominant_emotion}
          </span>
        </div>
      )}
      {segs.length > 0 && (
        <div className="max-h-96 overflow-auto rounded-md border border-hairline">
          <table className="w-full text-sm">
            <tbody>
              {segs.map((s, i) => (
                <tr key={i} className={clsx("border-b border-hairline/60 last:border-0", i % 2 === 1 && "bg-panel-2/40")}>
                  <td className="px-3 py-2 align-top">
                    <TimeRange start={s.start} end={s.end} />
                  </td>
                  <td className="px-3 py-2 text-ink-hi">{s.emotion}</td>
                  <td className="tnum px-3 py-2 text-right text-2xs text-ink-lo">
                    {typeof (s.score ?? s.confidence) === "number" ? (s.score ?? s.confidence)!.toFixed(2) : ""}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/* ---------- events ---------- */

interface AudioEvent {
  start?: number;
  end?: number;
  label?: string;
  score?: number;
}
function EventsView({ r }: { r: { events?: AudioEvent[]; summary?: string } }) {
  const events = r.events ?? [];
  return (
    <div className="space-y-4">
      {r.summary && <p className="rounded-md border border-hairline bg-ground/40 p-3 text-sm text-ink-mid">{r.summary}</p>}
      {events.length > 0 && (
        <div>
          <SectionLabel>Events · {events.length}</SectionLabel>
          <div className="max-h-96 overflow-auto rounded-md border border-hairline">
            <table className="w-full text-sm">
              <tbody>
                {events.map((e, i) => (
                  <tr key={i} className={clsx("border-b border-hairline/60 last:border-0", i % 2 === 1 && "bg-panel-2/40")}>
                    <td className="px-3 py-2 align-top">
                      <TimeRange start={e.start} end={e.end} />
                    </td>
                    <td className="px-3 py-2 text-ink-hi">{e.label}</td>
                    <td className="tnum px-3 py-2 text-right text-2xs text-ink-lo">
                      {typeof e.score === "number" ? e.score.toFixed(2) : ""}
                    </td>
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

/* ---------- moderation ---------- */

function ModerationView({ r }: { r: { flagged?: boolean; categories?: unknown; profanity?: unknown } }) {
  const cats =
    r.categories && typeof r.categories === "object" && !Array.isArray(r.categories)
      ? Object.entries(r.categories as Record<string, unknown>)
      : Array.isArray(r.categories)
        ? (r.categories as unknown[]).map((c) => [String(c), true] as [string, unknown])
        : [];
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <span className="label">Status</span>
        <span
          className={clsx(
            "rounded px-2 py-0.5 text-sm",
            r.flagged ? "border border-fail/40 bg-fail/10 text-fail" : "border border-ok/40 bg-ok/10 text-ok",
          )}
        >
          {r.flagged ? "flagged" : "clean"}
        </span>
      </div>
      {cats.length > 0 && (
        <div>
          <SectionLabel>Categories</SectionLabel>
          <div className="flex flex-wrap gap-2">
            {cats.map(([k, v]) => (
              <span
                key={k}
                className={clsx(
                  "rounded border px-2 py-0.5 font-mono text-2xs",
                  v ? "border-fail/40 text-fail" : "border-hairline-2 text-ink-lo",
                )}
              >
                {k}
                {typeof v === "number" ? ` ${v.toFixed(2)}` : ""}
              </span>
            ))}
          </div>
        </div>
      )}
      {r.profanity != null && (
        <div>
          <SectionLabel>Profanity</SectionLabel>
          {typeof r.profanity === "string" ? (
            <p className="text-sm text-ink-mid">{r.profanity}</p>
          ) : (
            <JsonView value={r.profanity} />
          )}
        </div>
      )}
    </div>
  );
}

/* ---------- scorecard ---------- */

function ScorecardView({
  r,
}: {
  r: { scores?: unknown; overall?: unknown; strengths?: unknown; improvements?: unknown };
}) {
  const scores =
    r.scores && typeof r.scores === "object" && !Array.isArray(r.scores)
      ? Object.entries(r.scores as Record<string, unknown>)
      : [];
  const strengths = Array.isArray(r.strengths) ? (r.strengths as unknown[]).map(String) : [];
  const improvements = Array.isArray(r.improvements) ? (r.improvements as unknown[]).map(String) : [];
  return (
    <div className="space-y-5">
      {r.overall != null && (
        <div className="flex items-baseline gap-2">
          <span className="label">Overall</span>
          <span className="tnum font-display text-2xl font-semibold text-brass">
            {typeof r.overall === "number" ? r.overall : String(r.overall)}
          </span>
        </div>
      )}
      {scores.length > 0 && (
        <div>
          <SectionLabel>Scores</SectionLabel>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            {scores.map(([k, v]) => (
              <div key={k} className="rounded-md border border-hairline bg-ground/40 p-3">
                <div className="label">{k.replace(/_/g, " ")}</div>
                <div className="mt-0.5 tnum text-lg text-ink-hi">{typeof v === "number" ? v : String(v)}</div>
              </div>
            ))}
          </div>
        </div>
      )}
      {strengths.length > 0 && (
        <div>
          <SectionLabel>Strengths</SectionLabel>
          <Pills items={strengths} />
        </div>
      )}
      {improvements.length > 0 && (
        <div>
          <SectionLabel>Improvements</SectionLabel>
          <Pills items={improvements} />
        </div>
      )}
    </div>
  );
}

/* ---------- crm ---------- */

function CrmView({ fields }: { fields: Record<string, unknown> }) {
  const entries = Object.entries(fields);
  return (
    <div>
      <SectionLabel>Extracted fields · {entries.length}</SectionLabel>
      <div className="overflow-hidden rounded-md border border-hairline">
        <table className="w-full text-sm">
          <tbody>
            {entries.map(([k, v], i) => (
              <tr key={k} className={clsx("border-b border-hairline/60 last:border-0", i % 2 === 1 && "bg-panel-2/40")}>
                <td className="label whitespace-nowrap px-3 py-2 align-top">{k.replace(/_/g, " ")}</td>
                <td className="px-3 py-2 text-ink-hi">
                  {v === null || v === undefined
                    ? "—"
                    : typeof v === "object"
                      ? JSON.stringify(v)
                      : String(v)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/* ---------- diarization ---------- */

interface DiarSegment {
  start?: number;
  end?: number;
  speaker?: string;
  text?: string;
}
const SPEAKER_TONES = ["text-brass", "text-ok", "text-ink-hi", "text-fail"];
function DiarizationView({ r }: { r: { segments?: DiarSegment[]; speakers?: unknown } }) {
  const segs = r.segments ?? [];
  const speakers = Array.isArray(r.speakers)
    ? (r.speakers as unknown[]).map(String)
    : typeof r.speakers === "number"
      ? Array.from({ length: r.speakers }, (_, i) => `speaker_${i}`)
      : [];
  const toneFor = (spk?: string) => {
    const idx = speakers.indexOf(spk ?? "");
    return SPEAKER_TONES[(idx < 0 ? 0 : idx) % SPEAKER_TONES.length];
  };
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <span className="label">Speakers</span>
        <span className="text-sm text-ink-hi">{speakers.length || "—"}</span>
      </div>
      {segs.length > 0 && (
        <div className="max-h-96 overflow-auto rounded-md border border-hairline">
          <table className="w-full text-sm">
            <tbody>
              {segs.map((s, i) => (
                <tr key={i} className={clsx("border-b border-hairline/60 last:border-0", i % 2 === 1 && "bg-panel-2/40")}>
                  <td className="px-3 py-2 align-top">
                    <TimeRange start={s.start} end={s.end} />
                  </td>
                  <td className={clsx("whitespace-nowrap px-3 py-2 align-top font-mono text-2xs", toneFor(s.speaker))}>
                    {s.speaker}
                  </td>
                  <td className="px-3 py-2 text-ink-hi">{s.text?.trim()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/* ---------- enhance / edit ---------- */

function AudioOutputView({ artifactId, metrics }: { artifactId: string; metrics?: unknown }) {
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <span className="label">Output audio</span>
        <ArtifactLink id={artifactId} />
      </div>
      {isObj(metrics) && <MetricsGrid metrics={metrics} />}
    </div>
  );
}

/* ---------- dispatcher ---------- */

export function ResultView({ result }: { result: unknown }) {
  if (result === undefined || result === null) {
    return <div className="text-sm text-ink-lo">No result payload.</div>;
  }
  if (!isObj(result)) return <JsonView value={result} />;
  const r = result;

  // Most-specific shapes first (several carry `segments`).
  if (Array.isArray(r.chapters)) return <ChaptersView chapters={r.chapters as Chapter[]} />;
  if (Array.isArray(r.events)) return <EventsView r={r} />;
  if ("scores" in r || "improvements" in r || "strengths" in r) return <ScorecardView r={r} />;
  if ("flagged" in r || ("categories" in r && "profanity" in r)) return <ModerationView r={r} />;
  if (typeof r.enhanced_audio_artifact_id === "string")
    return <AudioOutputView artifactId={r.enhanced_audio_artifact_id} metrics={r.metrics} />;
  if (typeof r.edited_audio_artifact_id === "string")
    return <AudioOutputView artifactId={r.edited_audio_artifact_id} metrics={r.metrics} />;
  if (isObj(r.fields) && Object.keys(r).length <= 3) return <CrmView fields={r.fields as Record<string, unknown>} />;

  const firstSeg = Array.isArray(r.segments) && isObj(r.segments[0]) ? r.segments[0] : null;
  if (firstSeg && "emotion" in firstSeg) return <EmotionView r={r} />;
  if ("dominant_emotion" in r) return <EmotionView r={r} />;
  if ((firstSeg && "speaker" in firstSeg) || "speakers" in r) return <DiarizationView r={r} />;

  if (looksLikeTranscript(r)) return <TranscriptView t={r as TranscriptShape} />;

  return <JsonView value={result} />;
}
