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

/**
 * Create a follow-on job that reads a prior job's transcript. Text/analysis
 * processors (text.*, audio.diarize/emotion/events/chapters/redact/edit) locate
 * the upstream result via `params.source_job_id`; the new job reuses the source
 * job's artifact (the API requires artifact_id on every job).
 */
export async function createAnalysisJobAction(input: {
  source_job_id: string;
  artifact_id: string;
  processor: ProcessorRef;
  params?: Record<string, unknown>;
}): Promise<ActionResult<Job>> {
  try {
    const job = await orpheus.createJob({
      artifact_id: input.artifact_id,
      processor: input.processor,
      params: { ...(input.params ?? {}), source_job_id: input.source_job_id },
    });
    revalidatePath(`/jobs/${input.source_job_id}`);
    revalidatePath("/jobs");
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
