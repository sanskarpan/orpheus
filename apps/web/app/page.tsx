import Link from "next/link";
import { WaveBars } from "@/components/primitives";
import { getAccount } from "@/lib/session";

export const dynamic = "force-dynamic";

const PROCESSORS = [
  "transcribe",
  "diarize",
  "convert-to-wav",
  "extract-metadata",
  "probe",
  "slice",
  "detect-language",
  "redact-pii",
  "summarize",
  "translate",
  "export-subtitles",
];

const FEATURES = [
  {
    icon: "✎",
    title: "GPU transcription",
    body: "faster-whisper transcription with word-level segments, language detection, and long-audio chunking.",
  },
  {
    icon: "⚙",
    title: "11 processors",
    body: "Diarize, translate, summarize, redact PII, slice, export subtitles — chain them into pipelines.",
  },
  {
    icon: "◈",
    title: "Content-addressed cache",
    body: "Identical inputs never pay twice. Results are hashed, deduped, and served instantly on re-run.",
  },
  {
    icon: "⇄",
    title: "Webhooks & delivery",
    body: "Signed event delivery with retries, a full delivery log, and one-click replay.",
  },
  {
    icon: "⚿",
    title: "Tenant-isolated",
    body: "Postgres row-level security enforces hard isolation per org. Your audio is yours alone.",
  },
  {
    icon: "▟",
    title: "Usage & metering",
    body: "Per-job cost, GPU-seconds, and a day-by-day breakdown — transparent from the first request.",
  },
];

const STEPS = [
  { n: "01", title: "Create your workspace", body: "Sign up with an email — we provision a real org and API key for you automatically." },
  { n: "02", title: "Upload & run", body: "Drop an audio file, pick a processor, and watch the job run to a rendered transcript." },
  { n: "03", title: "Integrate", body: "Generate scoped API keys and wire the same pipeline into your product or CI." },
];

export default async function LandingPage() {
  const account = await getAccount();

  return (
    <div className="min-h-screen">
      {/* Top bar */}
      <header className="sticky top-0 z-30 border-b border-hairline/70 bg-ground/70 backdrop-blur-md">
        <div className="mx-auto flex h-16 max-w-6xl items-center justify-between px-6">
          <div className="flex items-center gap-2.5">
            <div className="flex h-7 w-7 items-center justify-center rounded-md border border-brass/40 bg-brass/10">
              <span className="font-display text-lg font-extrabold leading-none text-brass">O</span>
            </div>
            <span className="font-display text-base font-bold tracking-tight">Orpheus</span>
            <span className="label ml-1 hidden sm:inline">Studio Console</span>
          </div>
          <nav className="flex items-center gap-2">
            {account ? (
              <Link href="/dashboard" className="btn-brass">
                Open dashboard →
              </Link>
            ) : (
              <>
                <Link href="/login" className="btn">
                  Log in
                </Link>
                <Link href="/signup" className="btn-brass">
                  Get started
                </Link>
              </>
            )}
          </nav>
        </div>
      </header>

      {/* Hero */}
      <section className="relative overflow-hidden border-b border-hairline">
        <div className="pointer-events-none absolute inset-0 bg-gradient-to-b from-brass/[0.05] to-transparent" />
        <div className="relative mx-auto grid max-w-6xl gap-10 px-6 py-20 lg:grid-cols-[1.1fr_0.9fr] lg:items-center lg:py-28">
          <div className="reveal">
            <div className="label mb-4">Audio processing platform</div>
            <h1 className="font-display text-5xl font-extrabold leading-[1.05] tracking-tight text-ink-hi sm:text-6xl">
              Turn raw audio into <span className="text-brass">structured results.</span>
            </h1>
            <p className="mt-5 max-w-xl text-lg leading-relaxed text-ink-mid">
              Orpheus is a managed pipeline for transcription and audio understanding — upload a file,
              run a processor, and get a clean, cached, auditable result. API-first, tenant-isolated,
              and ready in a minute.
            </p>
            <div className="mt-8 flex flex-wrap items-center gap-3">
              <Link href={account ? "/dashboard" : "/signup"} className="btn-brass px-5 py-3 text-base">
                {account ? "Open dashboard →" : "Start for free →"}
              </Link>
              <Link href="/login" className="btn px-5 py-3 text-base">
                Log in
              </Link>
            </div>
            <div className="mt-7 flex flex-wrap gap-x-6 gap-y-2 text-2xs font-mono uppercase tracking-wider text-ink-lo">
              <span>no credit card</span>
              <span className="text-hairline-2">·</span>
              <span>no key to paste</span>
              <span className="text-hairline-2">·</span>
              <span>real workspace in seconds</span>
            </div>
          </div>

          {/* Visual */}
          <div className="reveal">
            <div className="panel relative overflow-hidden p-6">
              <div className="mb-4 flex items-center justify-between">
                <span className="label">live transcription</span>
                <span className="flex items-center gap-1.5 text-2xs font-mono text-ok">
                  <span className="h-1.5 w-1.5 rounded-full bg-ok" /> completed
                </span>
              </div>
              <div className="mb-5 h-24">
                <WaveBars bars={52} className="h-full" />
              </div>
              <div className="rounded-md border border-hairline bg-ground/50 p-4">
                <div className="mb-2 flex items-center gap-2">
                  <span className="rounded border border-hairline-2 px-1.5 py-0.5 font-mono text-2xs uppercase text-ink-mid">
                    en
                  </span>
                  <span className="font-mono text-2xs text-ink-lo">transcribe · $0.000087</span>
                </div>
                <p className="text-sm leading-relaxed text-ink-hi">
                  "The quick brown fox jumps over the lazy dog — fierce transcription test, one two
                  three four five."
                </p>
              </div>
              <div className="mt-3 flex gap-2 font-mono text-2xs text-ink-lo">
                <span className="tnum">00:00 → 00:08</span>
                <span className="text-hairline-2">·</span>
                <span>1 segment</span>
              </div>
            </div>
          </div>
        </div>
      </section>

      {/* Features */}
      <section className="mx-auto max-w-6xl px-6 py-20">
        <div className="mb-10 max-w-2xl">
          <div className="label mb-3">What's inside</div>
          <h2 className="font-display text-3xl font-bold tracking-tight text-ink-hi">
            Everything you need to ship audio features.
          </h2>
        </div>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {FEATURES.map((f) => (
            <div key={f.title} className="panel card-hover p-5">
              <div className="mb-3 flex h-9 w-9 items-center justify-center rounded-md border border-brass/30 bg-brass/10 font-mono text-brass">
                {f.icon}
              </div>
              <h3 className="font-display text-lg font-semibold text-ink-hi">{f.title}</h3>
              <p className="mt-1.5 text-sm leading-relaxed text-ink-mid">{f.body}</p>
            </div>
          ))}
        </div>

        {/* Processor marquee */}
        <div className="mt-10 rounded-panel border border-hairline bg-panel p-5">
          <div className="label mb-3">The processor catalog</div>
          <div className="flex flex-wrap gap-2">
            {PROCESSORS.map((p) => (
              <span key={p} className="rounded-md border border-hairline-2 bg-panel-2 px-2.5 py-1 font-mono text-xs text-ink-mid">
                {p}
              </span>
            ))}
          </div>
        </div>
      </section>

      {/* How it works */}
      <section className="border-y border-hairline bg-panel/40">
        <div className="mx-auto max-w-6xl px-6 py-20">
          <div className="mb-10 max-w-2xl">
            <div className="label mb-3">How it works</div>
            <h2 className="font-display text-3xl font-bold tracking-tight text-ink-hi">
              From signup to first transcript in three steps.
            </h2>
          </div>
          <div className="grid gap-5 md:grid-cols-3">
            {STEPS.map((s) => (
              <div key={s.n} className="panel p-6">
                <div className="tnum mb-3 text-3xl font-bold text-brass">{s.n}</div>
                <h3 className="font-display text-lg font-semibold text-ink-hi">{s.title}</h3>
                <p className="mt-1.5 text-sm leading-relaxed text-ink-mid">{s.body}</p>
              </div>
            ))}
          </div>
        </div>
      </section>

      {/* Final CTA */}
      <section className="mx-auto max-w-6xl px-6 py-20">
        <div className="panel relative overflow-hidden p-10 text-center">
          <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-brass/[0.08] to-transparent" />
          <div className="relative">
            <div className="mx-auto mb-6 h-14 max-w-md opacity-60">
              <WaveBars bars={40} className="h-full" />
            </div>
            <h2 className="font-display text-3xl font-bold tracking-tight text-ink-hi">
              Start processing audio today.
            </h2>
            <p className="mx-auto mt-3 max-w-lg text-sm text-ink-mid">
              Create a workspace, upload a file, and read the transcript — no infrastructure, no key
              wrangling.
            </p>
            <div className="mt-7 flex justify-center gap-3">
              <Link href={account ? "/dashboard" : "/signup"} className="btn-brass px-5 py-3 text-base">
                {account ? "Open dashboard →" : "Create your account →"}
              </Link>
            </div>
          </div>
        </div>
      </section>

      {/* Footer */}
      <footer className="border-t border-hairline">
        <div className="mx-auto flex max-w-6xl flex-col items-center justify-between gap-3 px-6 py-8 text-2xs font-mono uppercase tracking-wider text-ink-lo sm:flex-row">
          <span>Orpheus · Studio Console</span>
          <span>Local build · RLS-isolated · API-first</span>
        </div>
      </footer>
    </div>
  );
}
