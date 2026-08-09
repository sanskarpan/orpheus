import Link from "next/link";
import { clsx } from "@/lib/clsx";

export function PageHeader({
  eyebrow,
  title,
  sub,
  actions,
}: {
  eyebrow?: string;
  title: string;
  sub?: string;
  actions?: React.ReactNode;
}) {
  return (
    <div className="mb-7 flex items-end justify-between gap-4">
      <div>
        {eyebrow && <div className="label mb-1.5">{eyebrow}</div>}
        <h1 className="font-display text-2xl font-bold tracking-tight text-ink-hi">{title}</h1>
        {sub && <p className="mt-1.5 max-w-2xl text-sm text-ink-mid">{sub}</p>}
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
    </div>
  );
}

export function StatTile({
  label,
  value,
  unit,
  hint,
  accent,
}: {
  label: string;
  value: string;
  unit?: string;
  hint?: string;
  accent?: boolean;
}) {
  return (
    <div className={clsx("panel p-5", accent && "border-brass/30")}>
      <div className="label mb-3">{label}</div>
      <div className="flex items-baseline gap-1.5">
        <span className={clsx("tnum text-3xl font-semibold", accent ? "text-brass" : "text-ink-hi")}>{value}</span>
        {unit && <span className="text-sm text-ink-lo">{unit}</span>}
      </div>
      {hint && <div className="mt-2 text-xs text-ink-lo">{hint}</div>}
    </div>
  );
}

export function EmptyState({
  icon = "◇",
  title,
  sub,
  action,
}: {
  icon?: string;
  title: string;
  sub?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="panel flex flex-col items-center justify-center px-6 py-16 text-center">
      <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-full border border-hairline-2 font-mono text-xl text-ink-lo">
        {icon}
      </div>
      <div className="font-display text-lg font-semibold text-ink-hi">{title}</div>
      {sub && <p className="mt-1.5 max-w-sm text-sm text-ink-mid">{sub}</p>}
      {action && <div className="mt-5">{action}</div>}
    </div>
  );
}

export function ErrorNotice({ title, detail }: { title: string; detail?: string }) {
  return (
    <div className="panel border-fail/30 p-5">
      <div className="flex items-center gap-2 text-fail">
        <span className="font-mono">⚠</span>
        <span className="font-display font-semibold">{title}</span>
      </div>
      {detail && <p className="mt-2 font-mono text-xs text-ink-mid">{detail}</p>}
    </div>
  );
}

export { Link };
