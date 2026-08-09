"use server";

import { revalidatePath } from "next/cache";
import { orpheus, OrpheusError, type WebhookEndpoint, type WebhookDelivery } from "@/lib/orpheus";
import type { ActionResult } from "@/app/actions/jobs";

function wrap(e: unknown): string {
  if (e instanceof OrpheusError) return e.detail || e.message;
  return String(e);
}

export async function createWebhookAction(input: {
  url: string;
  subscribed_events: string[];
  description?: string;
}): Promise<ActionResult<WebhookEndpoint>> {
  try {
    const wh = await orpheus.createWebhook(input);
    revalidatePath("/webhooks");
    return { ok: true, data: wh };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function replayDeliveryAction(id: string, deliveryId: string): Promise<ActionResult<WebhookDelivery>> {
  try {
    const d = await orpheus.replayDelivery(id, deliveryId);
    revalidatePath(`/webhooks/${id}`);
    return { ok: true, data: d };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function testWebhookAction(id: string): Promise<ActionResult<null>> {
  try {
    await orpheus.testWebhook(id);
    revalidatePath(`/webhooks/${id}`);
    return { ok: true, data: null };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}
