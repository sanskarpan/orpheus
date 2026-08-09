"use client";

import { useTransition } from "react";
import { useRouter } from "next/navigation";
import { requeueJobAction } from "@/app/actions/jobs";

export function RequeueButton({ id }: { id: string }) {
  const router = useRouter();
  const [pending, start] = useTransition();
  return (
    <button
      onClick={() =>
        start(async () => {
          await requeueJobAction(id);
          router.refresh();
        })
      }
      disabled={pending}
      className="rounded-md border border-brass/50 bg-brass/10 px-2.5 py-1 text-2xs font-mono uppercase tracking-wider text-brass transition-colors hover:bg-brass/20 disabled:opacity-40"
    >
      {pending ? "…" : "↻ requeue"}
    </button>
  );
}
