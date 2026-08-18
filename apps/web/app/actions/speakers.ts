"use server";

import { revalidatePath } from "next/cache";
import { orpheus, OrpheusError } from "@/lib/orpheus";
import type { ActionResult } from "@/app/actions/jobs";

function wrap(e: unknown): string {
  if (e instanceof OrpheusError) return e.detail || e.message;
  return String(e);
}

/** GDPR erasure of an enrolled voiceprint (biometric data). */
export async function deleteSpeakerAction(id: string): Promise<ActionResult<null>> {
  try {
    await orpheus.deleteSpeaker(id);
    revalidatePath("/voiceprints");
    return { ok: true, data: null };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}
