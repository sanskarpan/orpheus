"use client";

import { useEffect, useRef, useState } from "react";
import { usePathname } from "next/navigation";
import { Nav } from "@/components/Nav";

/**
 * Mobile-only slide-over navigation. Renders a hamburger toggle (visible below
 * the `md` breakpoint) that opens the nav as a drawer over an overlay. Closes on
 * route change, overlay click, and Escape.
 */
export function MobileNav({ admin }: { admin: boolean }) {
  const [open, setOpen] = useState(false);
  const pathname = usePathname();
  const closeRef = useRef<HTMLButtonElement>(null);

  // Close on navigation.
  useEffect(() => {
    setOpen(false);
  }, [pathname]);

  // Escape to close + move focus into the drawer when it opens.
  useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("keydown", onKey);
    closeRef.current?.focus();
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label="Open navigation menu"
        aria-expanded={open}
        className="flex h-8 w-8 items-center justify-center rounded-md border border-hairline-2 text-ink-mid transition-colors hover:border-brass/50 hover:text-ink-hi md:hidden"
      >
        <span className="block font-mono text-base leading-none">☰</span>
      </button>

      {open && (
        <div className="fixed inset-0 z-40 md:hidden">
          <div
            className="absolute inset-0 bg-ground/70 backdrop-blur-sm"
            onClick={() => setOpen(false)}
            aria-hidden="true"
          />
          <aside
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
            className="absolute inset-y-0 left-0 flex w-64 flex-col border-r border-hairline bg-panel/95 backdrop-blur-sm"
          >
            <div className="flex items-center justify-between px-5 py-5">
              <div className="flex items-center gap-2.5">
                <div className="flex h-7 w-7 items-center justify-center rounded-md border border-brass/40 bg-brass/10">
                  <span className="font-display text-lg font-extrabold leading-none text-brass">O</span>
                </div>
                <div className="leading-tight">
                  <div className="font-display text-base font-bold tracking-tight text-ink-hi">Orpheus</div>
                  <div className="label -mt-0.5">Studio Console</div>
                </div>
              </div>
              <button
                ref={closeRef}
                type="button"
                onClick={() => setOpen(false)}
                aria-label="Close navigation menu"
                className="rounded-md border border-hairline-2 p-1.5 text-ink-mid transition-colors hover:border-fail/50 hover:text-fail"
              >
                <span className="block h-3.5 w-3.5 text-center font-mono text-xs leading-none">✕</span>
              </button>
            </div>

            <div className="flex-1 overflow-y-auto">
              <Nav admin={admin} />
            </div>
          </aside>
        </div>
      )}
    </>
  );
}
