"use client";

import { useCallback, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { createJobAction, pollJobAction } from "@/app/actions/jobs";
import type { Artifact, Job } from "@/lib/orpheus";
import { ResultView } from "@/components/ResultView";
import { LevelMeter, StatusBadge, WaveBars } from "@/components/primitives";
import { bytes, usd } from "@/lib/format";
import { clsx } from "@/lib/clsx";

export interface ProcessorOption {
  name: string;
  display_name: string;
  description: string;
  versions: { version: string }[];
}

type Phase = "idle" | "uploading" | "uploaded" | "submitting" | "running" | "done" | "error";

const TERMINAL = new Set(["completed", "failed", "canceled", "dead_letter"]);

export function UploadStudio({ processors }: { processors: ProcessorOption[] }) {
  const router = useRouter();
  const inputRef = useRef<HTMLInputElement>(null);

  const [file, setFile] = useState<File | null>(null);
  const [dragging, setDragging] = useState(false);
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState<string | null>(null);
  const [artifact, setArtifact] = useState<Artifact | null>(null);
  const [job, setJob] = useState<Job | null>(null);

  const defaultProc = processors.find((p) => p.name.includes("transcribe")) ?? processors[0];
  const [proc, setProc] = useState<string>(defaultProc?.name ?? "");
  const selected = processors.find((p) => p.name === proc) ?? defaultProc;
  const version = selected?.versions?.[0]?.version ?? "1.0.0";

  const pickFile = (f: File | null) => {
    setFile(f);
    setArtifact(null);
    setJob(null);
    setError(null);
    setPhase("idle");
  };

  const onDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setDragging(false);
    const f = e.dataTransfer.files?.[0];
    if (f) pickFile(f);
  }, []);

  async function upload() {
    if (!file) return;
    setPhase("uploading");
    setError(null);
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await fetch("/api/upload", { method: "POST", body: fd });
      const body = await res.json();
      if (!res.ok) throw new Error(body.error ?? "Upload failed");
      setArtifact(body.artifact as Artifact);
      setPhase("uploaded");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  }

  async function run() {
    if (!artifact || !selected) return;
    setPhase("submitting");
    setError(null);
    let created;
    try {
      created = await createJobAction({
        artifact_id: artifact.id,
        processor: { name: selected.name, version },
      });
    } catch {
      setError("Couldn't submit the job. Please try again.");
      setPhase("error");
      return;
    }
    if (!created.ok) {
      setError(created.error);
      setPhase("error");
      return;
    }
    setJob(created.data);
    if (TERMINAL.has(created.data.status)) {
      setPhase("done");
      return;
    }
    setPhase("running");
    poll(created.data.id);
  }

  // Poll to completion. Transient errors (rate limits, blips) are tolerated —
  // the job keeps running server-side, so we retry a bounded number of times
  // before giving up rather than abandoning a job that's actually fine.
  function poll(id: string, tries = 0, softFails = 0) {
    if (tries > 150) {
      setError("Still processing after 5 minutes — check the job page for the result.");
      setPhase("error");
      return;
    }
    setTimeout(async () => {
      let r;
      try {
        r = await pollJobAction(id);
      } catch {
        r = { ok: false as const, error: "network blip" };
      }
      if (!r.ok) {
        if (softFails >= 5) {
          setError(`Lost contact while polling (${r.error}). The job may still finish — see the job page.`);
          setPhase("error");
          return;
        }
        poll(id, tries + 1, softFails + 1); // transient — keep trying
        return;
      }
      setJob(r.data);
      if (TERMINAL.has(r.data.status)) {
        setPhase("done");
        router.refresh();
        return;
      }
      poll(id, tries + 1, 0); // healthy response resets the soft-fail counter
    }, 2000);
  }

  const busy = phase === "uploading" || phase === "submitting" || phase === "running";

  return (
    <div className="grid gap-6 lg:grid-cols-5">
      {/* Left: source + controls */}
      <div className="space-y-5 lg:col-span-2">
        {/* Dropzone */}
        <div
          onDragOver={(e) => {
            e.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
          onClick={() => inputRef.current?.click()}
          className={clsx(
            "panel flex cursor-pointer flex-col items-center justify-center gap-3 border-dashed px-6 py-10 text-center transition-colors",
            dragging ? "border-brass bg-brass/5" : "border-hairline-2 hover:border-brass/40",
          )}
        >
          <input
            ref={inputRef}
            type="file"
            accept="audio/*,video/*,.wav,.mp3,.m4a,.flac,.ogg,.webm"
            className="hidden"
            onChange={(e) => pickFile(e.target.files?.[0] ?? null)}
          />
          <div className="flex h-12 w-12 items-center justify-center rounded-full border border-brass/30 bg-brass/10 font-mono text-xl text-brass">
            ↥
          </div>
          {file ? (
            <div>
              <div className="font-medium text-ink-hi">{file.name}</div>
              <div className="mt-0.5 font-mono text-2xs text-ink-lo">
                {bytes(file.size)} · {file.type || "audio"}
              </div>
            </div>
          ) : (
            <div>
              <div className="font-medium text-ink-hi">Drop audio here</div>
              <div className="mt-0.5 text-xs text-ink-lo">or click to browse — WAV, MP3, M4A, FLAC…</div>
            </div>
          )}
        </div>

        {/* Processor picker */}
        <div className="panel p-5">
          <div className="label mb-3">Processor</div>
          <div className="space-y-2">
            {processors.map((p) => {
              const active = p.name === proc;
              return (
                <button
                  key={p.name}
                  type="button"
                  onClick={() => setProc(p.name)}
                  disabled={busy}
                  className={clsx(
                    "w-full rounded-md border px-3 py-2.5 text-left transition-colors disabled:opacity-50",
                    active ? "border-brass/50 bg-brass/10" : "border-hairline hover:border-hairline-2 hover:bg-panel-2",
                  )}
                >
                  <div className="flex items-center justify-between">
                    <span className={clsx("text-sm font-medium", active ? "text-brass" : "text-ink-hi")}>
                      {p.display_name || p.name}
                    </span>
                    {active && (
                      <span className="font-mono text-2xs text-ink-lo">v{version}</span>
                    )}
                  </div>
                  {p.description && <div className="mt-0.5 line-clamp-1 text-xs text-ink-lo">{p.description}</div>}
                </button>
              );
            })}
            {processors.length === 0 && <div className="text-sm text-ink-lo">No processors available.</div>}
          </div>
        </div>

        {/* Action */}
        {!artifact ? (
          <button onClick={upload} disabled={!file || busy} className="btn-brass w-full py-2.5">
            {phase === "uploading" ? "Uploading…" : "Upload"}
          </button>
        ) : (
          <button onClick={run} disabled={busy || phase === "done"} className="btn-brass w-full py-2.5">
            {phase === "submitting" || phase === "running" ? "Processing…" : `Run ${selected?.display_name ?? "processor"}`}
          </button>
        )}

        {error && (
          <div className="rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{error}</div>
        )}
      </div>

      {/* Right: live stage */}
      <div className="lg:col-span-3">
        <div className="panel min-h-[24rem] p-6">
          {phase === "idle" && !file && (
            <div className="flex h-full min-h-[20rem] flex-col items-center justify-center text-center">
              <div className="mb-6 h-16 w-full max-w-sm opacity-40">
                <WaveBars bars={48} className="h-full" />
              </div>
              <div className="font-display text-lg font-semibold text-ink-hi">The stage is set</div>
              <p className="mt-1 max-w-xs text-sm text-ink-mid">
                Choose a file and a processor. Progress and the rendered result appear here in real time.
              </p>
            </div>
          )}

          {(phase === "uploading" || (file && phase === "idle")) && (
            <Stage title={phase === "uploading" ? "Uploading to storage" : "Ready to upload"}>
              <LevelMeter value={phase === "uploading" ? 0.5 : 0.06} className="h-4" />
              <p className="mt-3 font-mono text-2xs text-ink-lo">
                {file?.name} · {bytes(file?.size ?? 0)}
              </p>
            </Stage>
          )}

          {artifact && (phase === "uploaded" || phase === "submitting") && (
            <Stage title="Artifact stored">
              <ArtifactCard artifact={artifact} />
              <div className="mt-4 h-12 opacity-60">
                <WaveBars bars={40} className="h-full" />
              </div>
              <p className="mt-3 text-sm text-ink-mid">
                {phase === "submitting" ? "Queuing job…" : `Press “Run ${selected?.display_name}” to process.`}
              </p>
            </Stage>
          )}

          {job && (phase === "running" || phase === "done") && (
            <div className="space-y-5">
              <div className="flex items-center justify-between">
                <div className="label">Job</div>
                <StatusBadge status={job.status} />
              </div>

              {phase === "running" && (
                <div>
                  <div className="mb-3 h-14 opacity-90">
                    <WaveBars bars={44} className="h-full" />
                  </div>
                  <LevelMeter value={job.status === "running" ? 0.7 : 0.3} className="h-4" />
                  <p className="mt-3 font-mono text-2xs text-ink-lo">
                    {job.status} · attempt {job.attempts}/{job.max_retries} · {job.processor.name}
                  </p>
                </div>
              )}

              {phase === "done" && (
                <>
                  <div className="flex flex-wrap gap-x-6 gap-y-1 font-mono text-2xs text-ink-lo">
                    <span>id {job.id.slice(0, 8)}</span>
                    <span>cost {usd(job.cost_usd)}</span>
                    {job.cache && <span>cache {job.cache}</span>}
                  </div>
                  {job.status === "completed" ? (
                    <ResultView result={job.result} />
                  ) : (
                    <div className="rounded-md border border-fail/30 bg-fail/10 p-4 text-sm text-fail">
                      Job {job.status}. Inspect details on the job page.
                    </div>
                  )}
                  <Link href={`/dashboard/jobs/${job.id}`} className="btn inline-flex">
                    Open full job detail →
                  </Link>
                </>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function Stage({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="label mb-4">{title}</div>
      {children}
    </div>
  );
}

function ArtifactCard({ artifact }: { artifact: Artifact }) {
  return (
    <div className="grid grid-cols-2 gap-3 rounded-md border border-hairline bg-ground/40 p-4 text-sm sm:grid-cols-3">
      <Field label="Artifact" value={artifact.id.slice(0, 8)} mono />
      <Field label="Size" value={bytes(artifact.size_bytes)} />
      <Field label="Type" value={artifact.content_type} />
      {artifact.codec && <Field label="Codec" value={artifact.codec} />}
      {artifact.duration_seconds ? <Field label="Duration" value={`${artifact.duration_seconds.toFixed(1)}s`} /> : null}
      {artifact.sample_rate ? <Field label="Rate" value={`${artifact.sample_rate} Hz`} /> : null}
    </div>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <div className="label">{label}</div>
      <div className={clsx("mt-0.5 text-ink-hi", mono && "font-mono text-xs")}>{value}</div>
    </div>
  );
}
