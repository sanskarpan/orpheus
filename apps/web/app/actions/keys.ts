"use server";

import { revalidatePath } from "next/cache";
import { orpheus, OrpheusError, type APIKey } from "@/lib/orpheus";
import type { ActionResult } from "@/app/actions/jobs";

function wrap(e: unknown): string {
  if (e instanceof OrpheusError) return e.detail || e.message;
  return String(e);
}

export async function createKeyAction(input: { name: string; scopes: string[] }): Promise<ActionResult<APIKey>> {
  try {
    const key = await orpheus.createKey(input);
    revalidatePath("/api-keys");
    return { ok: true, data: key };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function revokeKeyAction(id: string): Promise<ActionResult<null>> {
  try {
    await orpheus.revokeKey(id);
    revalidatePath("/api-keys");
    return { ok: true, data: null };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}
