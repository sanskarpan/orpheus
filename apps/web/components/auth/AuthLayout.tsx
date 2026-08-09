import Link from "next/link";
import { WaveBars } from "@/components/primitives";

/** Shared split-panel chrome for /login and /signup. */
export function AuthLayout({
  title,
  subtitle,
  children,
  footer,
}: {
  title: string;
  subtitle: string;
  children: React.ReactNode;
  footer: React.ReactNode;
}) {
  return (
    <div className="flex min-h-screen items-center justify-center px-6 py-10">
      <div className="w-full max-w-4xl overflow-hidden rounded-panel border border-hairline bg-panel shadow-panel">
        <div className="grid md:grid-cols-2">
          {/* Brand / atmosphere */}
          <div className="relative hidden flex-col justify-between border-r border-hairline bg-gradient-to-br from-panel-2 to-ground p-8 md:flex">
            <Link href="/" className="flex items-center gap-2.5">
              <div className="flex h-8 w-8 items-center justify-center rounded-md border border-brass/40 bg-brass/10">
                <span className="font-display text-xl font-extrabold leading-none text-brass">O</span>
              </div>
              <div>
                <div className="font-display text-lg font-bold tracking-tight">Orpheus</div>
                <div className="label -mt-0.5">Studio Console</div>
              </div>
            </Link>

            <div className="my-8 h-24">
              <WaveBars bars={44} className="h-full opacity-90" />
            </div>

            <div>
              <h2 className="font-display text-2xl font-bold leading-tight text-ink-hi">
                Upload. Transcribe. <span className="text-brass">Ship audio at scale.</span>
              </h2>
              <p className="mt-3 text-sm leading-relaxed text-ink-mid">
                A managed pipeline for audio — GPU transcription, 11 processors, content-addressed
                caching, and webhook delivery, all behind one console.
              </p>
              <div className="mt-6 flex gap-4 text-2xs font-mono uppercase tracking-wider text-ink-lo">
                <span>self-serve</span>
                <span className="text-hairline-2">·</span>
                <span>rls-isolated</span>
                <span className="text-hairline-2">·</span>
                <span>api-first</span>
              </div>
            </div>
          </div>

          {/* Form */}
          <div className="flex flex-col justify-center p-8 md:p-10">
            <div className="mb-6">
              <h1 className="font-display text-xl font-bold tracking-tight">{title}</h1>
              <p className="mt-1 text-sm text-ink-mid">{subtitle}</p>
            </div>
            {children}
            <div className="mt-6 border-t border-hairline pt-4 text-sm text-ink-mid">{footer}</div>
          </div>
        </div>
      </div>
    </div>
  );
}
