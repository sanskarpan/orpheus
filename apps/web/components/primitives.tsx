import { clsx } from "@/lib/clsx";

/* ------------------------------------------------------------------ *
 * Small, dependency-free presentational primitives for Studio Console.
 * Kept together so the design language lives in one place.
 * ------------------------------------------------------------------ */

type Tone = "ok" | "run" | "fail" | "idle" | "warn";

const dotTone: Record<Tone, string> = {
  ok: "bg-ok",
  run: "bg-brass animate-meter-pulse shadow-[0_0_8px_1px_rgba(224,163,64,0.6)]",
  fail: "bg-fail",
  warn: "bg-warn",
  idle: "bg-ink-lo",
};

/** A single status LED — pulsing brass while running. */
export function StatusDot({ tone, className }: { tone: Tone; className?: string }) {
  return <span className={clsx("inline-block h-2 w-2 shrink-0 rounded-full", dotTone[tone], className)} />;
}

/** Maps a job/delivery status string to a tone + label. */
export function statusTone(status: string): Tone {
  const s = (status ?? "").toLowerCase();
  if (["succeeded", "success", "completed", "ok", "delivered", "verified", "approved", "active", "enabled"].includes(s))
    return "ok";
  if (["failed", "dead_letter", "error", "rejected", "cancelled", "canceled", "revoked", "expired"].includes(s))
    return "fail";
  if (["running", "processing", "pending", "queued", "in_progress", "retrying", "provisioning"].includes(s))
    return "run";
  if (["warning", "degraded"].includes(s)) return "warn";
  return "idle";
}

export function StatusBadge({ status }: { status: string }) {
  const tone = statusTone(status);
  const ring: Record<Tone, string> = {
    ok: "border-ok/30 text-ok",
    run: "border-brass/40 text-brass",
    fail: "border-fail/40 text-fail",
    warn: "border-warn/40 text-warn",
    idle: "border-hairline-2 text-ink-mid",
  };
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-2xs font-mono uppercase tracking-wider",
        ring[tone],
      )}
    >
      <StatusDot tone={tone} />
      {(status ?? "unknown").replace(/_/g, " ")}
    </span>
  );
}

export function Badge({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 rounded border border-hairline-2 bg-panel-2 px-1.5 py-0.5 font-mono text-2xs uppercase tracking-wider text-ink-mid",
        className,
      )}
    >
      {children}
    </span>
  );
}

/**
 * A discrete level-meter progress bar rendered as segmented blocks — the
 * signature Studio Console motif. `value` is 0..1.
 */
export function LevelMeter({
  value,
  segments = 24,
  className,
  tone = "brass",
}: {
  value: number;
  segments?: number;
  className?: string;
  tone?: "brass" | "ok" | "fail";
}) {
  const filled = Math.round(Math.max(0, Math.min(1, value)) * segments);
  const litClass =
    tone === "ok" ? "bg-ok" : tone === "fail" ? "bg-fail" : "bg-gradient-to-b from-brass to-brass-deep";
  return (
    <div className={clsx("flex h-3 items-stretch gap-[2px]", className)} aria-hidden>
      {Array.from({ length: segments }).map((_, i) => (
        <span
          key={i}
          className={clsx("flex-1 rounded-[1px]", i < filled ? litClass : "bg-hairline-2/60")}
        />
      ))}
    </div>
  );
}

/** Idle animated waveform — the Overview hero / running-job motif. */
export function WaveBars({ bars = 40, className }: { bars?: number; className?: string }) {
  // Deterministic pseudo-heights (no Math.random — SSR must be stable).
  return (
    <div className={clsx("wavebars", className)} aria-hidden>
      {Array.from({ length: bars }).map((_, i) => {
        const h = 0.35 + 0.6 * Math.abs(Math.sin(i * 0.7) * Math.cos(i * 0.31));
        return (
          <span
            key={i}
            style={{
              height: `${Math.round(h * 100)}%`,
              animationDelay: `${(i % 12) * 0.07}s`,
              animationDuration: `${1 + (i % 5) * 0.12}s`,
            }}
          />
        );
      })}
    </div>
  );
}

export function Panel({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return <div className={clsx("panel p-5", className)}>{children}</div>;
}

export function SectionLabel({ children }: { children: React.ReactNode }) {
  return <div className="label mb-3">{children}</div>;
}
