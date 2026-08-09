"use client";

import Link from "next/link";
import { useEffect } from "react";

/**
 * Segment error boundary for the dashboard. A server component that throws
 * (e.g. a non-404 API error) lands here instead of Next's raw error page, and
 * the user gets a recoverable action rather than a dead end.
 */
export default function DashboardError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error("dashboard error boundary:", error);
  }, [error]);

  return (
    <div className="flex min-h-[60vh] items-center justify-center">
      <div className="panel max-w-md p-8 text-center">
        <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full border border-fail/40 bg-fail/10 font-mono text-xl text-fail">
          ⚠
        </div>
        <h1 className="font-display text-xl font-bold text-ink-hi">Something went wrong</h1>
        <p className="mt-2 text-sm text-ink-mid">
          This view hit an unexpected error. It's usually transient — retrying often fixes it.
        </p>
        {error.digest && <p className="mt-2 font-mono text-2xs text-ink-lo">ref {error.digest}</p>}
        <div className="mt-6 flex justify-center gap-2">
          <button onClick={reset} className="btn-brass">
            Retry
          </button>
          <Link href="/dashboard" className="btn">
            Back to overview
          </Link>
        </div>
      </div>
    </div>
  );
}
