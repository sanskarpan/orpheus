"use server";

import { revalidatePath } from "next/cache";
import { orpheus, OrpheusError, type StreamingSession } from "@/lib/orpheus";
import type { ActionResult } from "@/app/actions/jobs";

function wrap(e: unknown): string {
  if (e instanceof OrpheusError) return e.detail || e.message;
  return String(e);
}

export async function createStreamingAction(): Promise<ActionResult<StreamingSession>> {
  try {
    const s = await orpheus.createStreaming();
    revalidatePath("/streaming");
    return { ok: true, data: s };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function finalizeStreamingAction(
  id: string,
  transcript: string,
  audio_seconds: number,
): Promise<ActionResult<StreamingSession>> {
  try {
    const s = await orpheus.finalizeStreaming(id, { transcript, audio_seconds });
    revalidatePath("/streaming");
    return { ok: true, data: s };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}
