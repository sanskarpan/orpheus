import type { TimeseriesPoint } from "@/lib/orpheus";

/**
 * Bespoke amber area chart rendered as pure SVG (no client JS, no chart lib).
 * Plots a single metric across time buckets.
 */
export function UsageChart({
  points,
  metric = "cost_usd",
  height = 220,
}: {
  points: TimeseriesPoint[];
  metric?: "cost_usd" | "jobs" | "compute_seconds";
  height?: number;
}) {
  const W = 900;
  const H = height;
  const padX = 8;
  const padY = 16;

  if (points.length === 0) {
    return (
      <div className="flex items-center justify-center rounded-md border border-hairline bg-ground/40 py-16 text-sm text-ink-lo">
        No usage in this window.
      </div>
    );
  }

  const vals = points.map((p) => Number(p[metric] ?? 0));
  const max = Math.max(...vals, metric === "cost_usd" ? 0.0001 : 1);
  const n = points.length;

  const x = (i: number) => padX + (i / Math.max(1, n - 1)) * (W - 2 * padX);
  const y = (v: number) => padY + (1 - v / max) * (H - 2 * padY);

  const line = vals.map((v, i) => `${i === 0 ? "M" : "L"} ${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(" ");
  const area = `${line} L ${x(n - 1).toFixed(1)} ${(H - padY).toFixed(1)} L ${x(0).toFixed(1)} ${(H - padY).toFixed(1)} Z`;

  // gridlines at 0/25/50/75/100%
  const grid = [0, 0.25, 0.5, 0.75, 1];

  return (
    <div className="overflow-x-auto">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ minWidth: 560 }} role="img">
        <defs>
          <linearGradient id="usageFill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="#E0A340" stopOpacity="0.35" />
            <stop offset="100%" stopColor="#E0A340" stopOpacity="0" />
          </linearGradient>
        </defs>

        {grid.map((g) => (
          <line
            key={g}
            x1={padX}
            x2={W - padX}
            y1={padY + g * (H - 2 * padY)}
            y2={padY + g * (H - 2 * padY)}
            stroke="#23262B"
            strokeWidth="1"
            strokeDasharray={g === 1 ? "0" : "2 4"}
          />
        ))}

        <path d={area} fill="url(#usageFill)" />
        <path d={line} fill="none" stroke="#E0A340" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />

        {vals.map((v, i) => (
          <circle key={i} cx={x(i)} cy={y(v)} r={n > 40 ? 0 : 2.5} fill="#0B0C0E" stroke="#E0A340" strokeWidth="1.5" />
        ))}
      </svg>
    </div>
  );
}
