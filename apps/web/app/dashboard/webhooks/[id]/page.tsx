import Link from "next/link";
import { notFound } from "next/navigation";
import { orpheus, OrpheusError, type WebhookDelivery } from "@/lib/orpheus";
import { PageHeader } from "@/components/layout";
import { StatusBadge, StatusDot } from "@/components/primitives";
import { absTime } from "@/lib/format";
import { ReplayButton, TestFireButton } from "./DeliveryActions";

export const dynamic = "force-dynamic";

export default async function WebhookDetailPage({ params }: { params: { id: string } }) {
  let hook;
  try {
    hook = await orpheus.getWebhook(params.id);
  } catch (e) {
    if (e instanceof OrpheusError && e.status === 404) notFound();
    throw e;
  }

  let deliveries: WebhookDelivery[] = [];
  try {
    deliveries = (await orpheus.listDeliveries(params.id, { limit: 50 })).data;
  } catch {
    deliveries = [];
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Webhook"
        title="Endpoint detail"
        actions={<TestFireButton id={hook.id} />}
      />

      <div className="panel p-5">
        <div className="flex items-center gap-2">
          <StatusDot tone={hook.active ? "ok" : "idle"} />
          <span className="font-mono text-sm text-ink-hi">{hook.active ? "active" : "disabled"}</span>
        </div>
        <div className="mt-3 break-all font-mono text-sm text-ink-mid">{hook.url}</div>
        {hook.description && <div className="mt-1 text-sm text-ink-lo">{hook.description}</div>}
        <div className="mt-4 flex flex-wrap gap-1.5">
          {hook.subscribed_events.map((e) => (
            <span key={e} className="rounded border border-hairline-2 px-1.5 py-0.5 font-mono text-2xs text-ink-mid">
              {e}
            </span>
          ))}
        </div>
      </div>

      <div className="panel overflow-hidden">
        <div className="hairline-b px-5 py-3">
          <span className="label">Deliveries · {deliveries.length}</span>
        </div>
        {deliveries.length === 0 ? (
          <div className="px-5 py-12 text-center text-sm text-ink-mid">
            No deliveries yet. Fire a test event to see one here.
          </div>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="label border-b border-hairline text-left">
                <th className="px-5 py-2.5 font-mono font-normal">Event</th>
                <th className="px-3 py-2.5 font-mono font-normal">Status</th>
                <th className="px-3 py-2.5 text-right font-mono font-normal">Attempts</th>
                <th className="px-3 py-2.5 text-right font-mono font-normal">Code</th>
                <th className="px-3 py-2.5 text-right font-mono font-normal">Last</th>
                <th className="px-5 py-2.5 text-right font-mono font-normal">Replay</th>
              </tr>
            </thead>
            <tbody>
              {deliveries.map((d) => (
                <tr key={d.id} className="border-b border-hairline/60 last:border-0 align-top hover:bg-panel-2">
                  <td className="px-5 py-3">
                    <div className="font-mono text-ink-hi">{d.event_type}</div>
                    {d.last_error && <div className="mt-0.5 max-w-xs truncate text-2xs text-fail">{d.last_error}</div>}
                  </td>
                  <td className="px-3 py-3">
                    <StatusBadge status={d.status} />
                  </td>
                  <td className="tnum px-3 py-3 text-right text-ink-mid">{d.attempt_count}</td>
                  <td className="tnum px-3 py-3 text-right text-ink-mid">{d.last_status_code ?? "—"}</td>
                  <td className="tnum px-3 py-3 text-right text-2xs text-ink-lo">{absTime(d.last_attempt_at)}</td>
                  <td className="px-5 py-3 text-right">
                    <ReplayButton id={hook.id} deliveryId={d.id} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <Link href="/dashboard/webhooks" className="inline-block text-sm text-ink-mid hover:text-brass">
        ← All webhooks
      </Link>
    </div>
  );
}
