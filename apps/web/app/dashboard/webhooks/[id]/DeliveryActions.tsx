"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { replayDeliveryAction, testWebhookAction } from "@/app/actions/webhooks";

export function TestFireButton({ id }: { id: string }) {
  const router = useRouter();
  const [pending, start] = useTransition();
  const [msg, setMsg] = useState<string | null>(null);

  return (
    <div className="flex items-center gap-2">
      <button
        onClick={() =>
          start(async () => {
            const r = await testWebhookAction(id);
            setMsg(r.ok ? "Test event fired" : r.error);
            router.refresh();
          })
        }
        disabled={pending}
        className="btn"
      >
        {pending ? "Firing…" : "⚡ Test fire"}
      </button>
      {msg && <span className="text-2xs text-ink-lo">{msg}</span>}
    </div>
  );
}

export function ReplayButton({ id, deliveryId }: { id: string; deliveryId: string }) {
  const router = useRouter();
  const [pending, start] = useTransition();

  return (
    <button
      onClick={() =>
        start(async () => {
          await replayDeliveryAction(id, deliveryId);
          router.refresh();
        })
      }
      disabled={pending}
      className="text-2xs font-mono uppercase text-ink-lo transition-colors hover:text-brass"
    >
      {pending ? "…" : "replay"}
    </button>
  );
}
