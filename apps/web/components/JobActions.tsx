"use client";

import { useEffect, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { cancelJobAction, requeueJobAction } from "@/app/actions/jobs";
import { clsx } from "@/lib/clsx";

const ACTIVE = new Set(["queued", "running"]);
const RETRYABLE = new Set(["failed", "dead_letter"]);

export function JobActions({ id, status }: { id: string; status: string }) {
  const router = useRouter();
  const [pending, start] = useTransition();
  const [err, setErr] = useState<string | null>(null);

  // Auto-refresh while the job is in flight so the server component re-renders.
  useEffect(() => {
    if (!ACTIVE.has(status)) return;
    const t = setInterval(() => router.refresh(), 2500);
    return () => clearInterval(t);
  }, [status, router]);

  const run = (fn: () => Promise<{ ok: boolean; error?: string }>) =>
    start(async () => {
      setErr(null);
      const r = await fn();
      if (!r.ok) setErr(r.error ?? "Action failed");
      else router.refresh();
    });

  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex gap-2">
        {RETRYABLE.has(status) && (
          <button
            onClick={() => run(() => requeueJobAction(id))}
            disabled={pending}
            className="btn-brass"
          >
            ↻ Requeue
          </button>
        )}
        {ACTIVE.has(status) && (
          <button
            onClick={() => run(() => cancelJobAction(id))}
            disabled={pending}
            className={clsx("btn", "hover:border-fail/50 hover:text-fail")}
          >
            ✕ Cancel
          </button>
        )}
      </div>
      {err && <span className="text-2xs text-fail">{err}</span>}
    </div>
  );
}
