"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { reviewSubmissionAction } from "@/app/actions/ops";

export function ReviewButtons({ id }: { id: string }) {
  const router = useRouter();
  const [pending, start] = useTransition();
  const [err, setErr] = useState<string | null>(null);

  const review = (decision: "approve" | "reject") =>
    start(async () => {
      setErr(null);
      const r = await reviewSubmissionAction(id, decision);
      if (!r.ok) setErr(r.error);
      else router.refresh();
    });

  return (
    <div className="flex items-center justify-end gap-2">
      <button
        onClick={() => review("approve")}
        disabled={pending}
        className="rounded-md border border-ok/40 bg-ok/10 px-2.5 py-1 text-2xs font-mono uppercase text-ok transition-colors hover:bg-ok/20 disabled:opacity-40"
      >
        approve
      </button>
      <button
        onClick={() => review("reject")}
        disabled={pending}
        className="rounded-md border border-fail/40 bg-fail/10 px-2.5 py-1 text-2xs font-mono uppercase text-fail transition-colors hover:bg-fail/20 disabled:opacity-40"
      >
        reject
      </button>
      {err && <span className="text-2xs text-fail">{err}</span>}
    </div>
  );
}
