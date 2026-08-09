import Link from "next/link";
import { clsx } from "@/lib/clsx";

export interface ChecklistState {
  ranJob: boolean;
  transcribed: boolean;
  createdKey: boolean;
  addedWebhook: boolean;
}

interface Step {
  key: keyof ChecklistState;
  title: string;
  body: string;
  href: string;
  cta: string;
}

const STEPS: Step[] = [
  { key: "ranJob", title: "Upload your first audio", body: "Drop a file and store it as an artifact.", href: "/dashboard/upload", cta: "Upload" },
  { key: "transcribed", title: "Run a transcription", body: "Turn that audio into a transcript with segments.", href: "/dashboard/upload", cta: "Transcribe" },
  { key: "createdKey", title: "Create an API key", body: "Generate a scoped key for your own integration.", href: "/dashboard/api-keys", cta: "Create key" },
  { key: "addedWebhook", title: "Add a webhook", body: "Receive signed events when jobs complete.", href: "/dashboard/webhooks", cta: "Add webhook" },
];

export function OnboardingChecklist({ state, name }: { state: ChecklistState; name: string }) {
  const done = STEPS.filter((s) => state[s.key]).length;
  if (done === STEPS.length) return null; // all set — nothing to nag about

  const firstName = name.split(/\s+/)[0] || "there";

  return (
    <div className="panel relative overflow-hidden p-6">
      <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-brass/[0.06] to-transparent" />
      <div className="relative">
        <div className="mb-5 flex items-end justify-between gap-4">
          <div>
            <div className="label mb-1.5">Get started</div>
            <h2 className="font-display text-xl font-bold tracking-tight text-ink-hi">
              Welcome, {firstName}. Let's get you running.
            </h2>
          </div>
          <div className="text-right">
            <div className="tnum text-2xl font-semibold text-brass">
              {done}
              <span className="text-ink-lo">/{STEPS.length}</span>
            </div>
            <div className="label">complete</div>
          </div>
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          {STEPS.map((s, i) => {
            const complete = state[s.key];
            return (
              <div
                key={s.key}
                className={clsx(
                  "flex items-start gap-3 rounded-md border p-3.5 transition-colors",
                  complete ? "border-ok/25 bg-ok/[0.04]" : "border-hairline hover:border-hairline-2",
                )}
              >
                <div
                  className={clsx(
                    "mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full border font-mono text-2xs",
                    complete ? "border-ok/50 bg-ok/15 text-ok" : "border-hairline-2 text-ink-lo",
                  )}
                >
                  {complete ? "✓" : i + 1}
                </div>
                <div className="min-w-0 flex-1">
                  <div className={clsx("text-sm font-medium", complete ? "text-ink-mid line-through" : "text-ink-hi")}>
                    {s.title}
                  </div>
                  <div className="mt-0.5 text-xs text-ink-lo">{s.body}</div>
                </div>
                {!complete && (
                  <Link
                    href={s.href}
                    className="shrink-0 rounded-md border border-brass/50 bg-brass/10 px-2.5 py-1 text-2xs font-mono uppercase tracking-wider text-brass transition-colors hover:bg-brass/20"
                  >
                    {s.cta}
                  </Link>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
