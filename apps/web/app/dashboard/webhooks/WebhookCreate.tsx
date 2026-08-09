"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { createWebhookAction } from "@/app/actions/webhooks";
import { CopyOnce } from "@/components/CopyOnce";
import { ALLOWED_EVENTS } from "@/lib/events";
import { clsx } from "@/lib/clsx";

export function WebhookCreate() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [url, setUrl] = useState("");
  const [desc, setDesc] = useState("");
  const [events, setEvents] = useState<string[]>(["job.completed", "job.failed"]);
  const [secret, setSecret] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();

  const toggle = (e: string) =>
    setEvents((cur) => (cur.includes(e) ? cur.filter((x) => x !== e) : [...cur, e]));

  const submit = () =>
    start(async () => {
      setError(null);
      if (!/^https?:\/\//.test(url)) return setError("Enter a valid http(s) URL.");
      if (events.length === 0) return setError("Choose at least one event.");
      const r = await createWebhookAction({ url, subscribed_events: events, description: desc || undefined });
      if (!r.ok) return setError(r.error);
      if (r.data.secret) setSecret(r.data.secret);
      setUrl("");
      setDesc("");
      router.refresh();
    });

  if (!open) {
    return (
      <button onClick={() => setOpen(true)} className="btn-brass">
        + New endpoint
      </button>
    );
  }

  return (
    <div className="panel w-full space-y-4 p-5">
      <div className="flex items-center justify-between">
        <div className="label">New webhook endpoint</div>
        <button onClick={() => setOpen(false)} className="text-2xs font-mono text-ink-lo hover:text-ink-mid">
          close
        </button>
      </div>
      <div>
        <label className="label mb-1.5 block">Delivery URL</label>
        <input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://example.com/hooks" className="input font-mono" />
      </div>
      <div>
        <label className="label mb-1.5 block">Description (optional)</label>
        <input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="prod events → pager" className="input" />
      </div>
      <div>
        <label className="label mb-1.5 block">Events</label>
        <div className="flex flex-wrap gap-1.5">
          {ALLOWED_EVENTS.map((e) => (
            <button
              key={e}
              type="button"
              onClick={() => toggle(e)}
              className={clsx(
                "rounded-md border px-2 py-1 font-mono text-2xs transition-colors",
                events.includes(e)
                  ? "border-brass/50 bg-brass/10 text-brass"
                  : "border-hairline-2 text-ink-mid hover:text-ink-hi",
              )}
            >
              {e}
            </button>
          ))}
        </div>
      </div>
      <button onClick={submit} disabled={pending} className="btn-brass">
        {pending ? "Creating…" : "Create endpoint"}
      </button>
      {error && <div className="text-2xs text-fail">{error}</div>}
      {secret && <CopyOnce label="Signing secret" secret={secret} />}
    </div>
  );
}
