import Link from "next/link";
import { orpheus, type WebhookEndpoint } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { StatusDot } from "@/components/primitives";
import { absTime } from "@/lib/format";
import { WebhookCreate } from "./WebhookCreate";

export const dynamic = "force-dynamic";

export default async function WebhooksPage() {
  let hooks: WebhookEndpoint[] = [];
  let error: string | null = null;
  try {
    hooks = (await orpheus.listWebhooks({ limit: 50 })).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Platform"
        title="Webhooks"
        sub="Delivery endpoints for platform events, with per-delivery history and replay."
        actions={<WebhookCreate />}
      />

      {error ? (
        <ErrorNotice title="Couldn't load webhooks" detail={error} />
      ) : hooks.length === 0 ? (
        <div className="panel px-5 py-14 text-center text-sm text-ink-mid">
          No endpoints yet. Create one to start receiving events.
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {hooks.map((h) => (
            <Link key={h.id} href={`/dashboard/webhooks/${h.id}`} className="panel card-hover p-5">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <StatusDot tone={h.active ? "ok" : "idle"} />
                  <span className="font-mono text-sm text-ink-hi">{h.active ? "active" : "disabled"}</span>
                </div>
                <span className="font-mono text-2xs text-ink-lo">{absTime(h.created_at)}</span>
              </div>
              <div className="mt-2 truncate font-mono text-sm text-ink-mid" title={h.url}>
                {h.url}
              </div>
              {h.description && <div className="mt-1 text-xs text-ink-lo">{h.description}</div>}
              <div className="mt-3 flex flex-wrap gap-1">
                {h.subscribed_events.slice(0, 4).map((e) => (
                  <span key={e} className="rounded border border-hairline-2 px-1.5 py-0.5 font-mono text-2xs text-ink-mid">
                    {e}
                  </span>
                ))}
                {h.subscribed_events.length > 4 && (
                  <span className="font-mono text-2xs text-ink-lo">+{h.subscribed_events.length - 4}</span>
                )}
              </div>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
