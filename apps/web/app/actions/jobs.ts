"use server";

import { revalidatePath } from "next/cache";
import { orpheus, OrpheusError, type Job, type ProcessorRef } from "@/lib/orpheus";

export type ActionResult<T> = { ok: true; data: T } | { ok: false; error: string };

function wrap(e: unknown): string {
  if (e instanceof OrpheusError) return e.detail || e.message;
  return String(e);
}

export async function createJobAction(input: {
  artifact_id: string;
  processor: ProcessorRef;
  params?: unknown;
  cache?: "auto" | "bypass" | "only";
}): Promise<ActionResult<Job>> {
  try {
    const job = await orpheus.createJob(input);
    revalidatePath("/jobs");
    revalidatePath("/");
    return { ok: true, data: job };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

/** Lightweight status poll used by the client while a job is in flight. */
export async function pollJobAction(id: string): Promise<ActionResult<Job>> {
  try {
    return { ok: true, data: await orpheus.getJob(id) };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function requeueJobAction(id: string): Promise<ActionResult<Job>> {
  try {
    const job = await orpheus.requeueJob(id);
    revalidatePath(`/jobs/${id}`);
    revalidatePath("/ops");
    return { ok: true, data: job };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function cancelJobAction(id: string): Promise<ActionResult<Job>> {
  try {
    const job = await orpheus.cancelJob(id);
    revalidatePath(`/jobs/${id}`);
    return { ok: true, data: job };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}
