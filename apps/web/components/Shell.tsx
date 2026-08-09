import Link from "next/link";
import { Nav } from "@/components/Nav";
import { LiveClock } from "@/components/LiveClock";
import { signOut } from "@/app/actions/auth";

/**
 * The application chrome: fixed left rail (wordmark + nav + identity) and a
 * top strip (breadcrumb slot + live clock). Children render on the dark canvas.
 */
function initials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase())
    .join("");
}

export function Shell({
  children,
  admin,
  name,
  email,
  orgId,
}: {
  children: React.ReactNode;
  admin: boolean;
  name: string;
  email: string;
  orgId?: string;
}) {
  return (
    <div className="flex min-h-screen">
      {/* Left rail */}
      <aside className="fixed inset-y-0 left-0 z-20 flex w-64 flex-col border-r border-hairline bg-panel/60 backdrop-blur-sm">
        <div className="flex items-center gap-2.5 px-5 py-5">
          <div className="flex h-7 w-7 items-center justify-center rounded-md border border-brass/40 bg-brass/10">
            <span className="font-display text-lg font-extrabold leading-none text-brass">O</span>
          </div>
          <div className="leading-tight">
            <div className="font-display text-base font-bold tracking-tight text-ink-hi">Orpheus</div>
            <div className="label -mt-0.5">Studio Console</div>
          </div>
        </div>

        <div className="flex-1 overflow-y-auto">
          <Nav admin={admin} />
        </div>

        {/* Identity footer */}
        <div className="border-t border-hairline px-4 py-3">
          <div className="flex items-center gap-2.5">
            <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full border border-hairline-2 bg-panel-2 font-mono text-2xs text-ink-mid">
              {initials(name) || "U"}
            </div>
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-1.5">
                <span className="truncate text-sm font-medium text-ink-hi">{name}</span>
                {admin && (
                  <span className="shrink-0 rounded border border-brass/40 px-1 py-px text-[9px] font-mono uppercase tracking-wider text-brass">
                    admin
                  </span>
                )}
              </div>
              <div className="truncate text-2xs text-ink-lo" title={orgId ? `org ${orgId}` : undefined}>
                {email}
              </div>
            </div>
            <form action={signOut}>
              <button
                type="submit"
                title="Sign out"
                className="rounded-md border border-hairline-2 p-1.5 text-ink-mid transition-colors hover:border-fail/50 hover:text-fail"
              >
                <span className="block h-3.5 w-3.5 text-center font-mono text-xs leading-none">⏻</span>
              </button>
            </form>
          </div>
        </div>
      </aside>

      {/* Main column */}
      <div className="ml-64 flex min-h-screen flex-1 flex-col">
        <header className="sticky top-0 z-10 flex h-14 items-center justify-between border-b border-hairline bg-ground/70 px-8 backdrop-blur-md">
          <Link href="/dashboard" className="label transition-colors hover:text-ink-mid">
            orpheus · studio console
          </Link>
          <div className="flex items-center gap-4">
            <span className="hidden items-center gap-1.5 text-2xs font-mono text-ink-lo sm:flex">
              <span className="h-1.5 w-1.5 rounded-full bg-ok" /> API live
            </span>
            <LiveClock />
          </div>
        </header>

        <main className="flex-1 px-8 py-8">{children}</main>
      </div>
    </div>
  );
}
