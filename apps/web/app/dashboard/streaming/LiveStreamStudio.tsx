"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { createStreamingAction, finalizeStreamingAction } from "@/app/actions/streaming";
import type { StreamingSession } from "@/lib/orpheus";
import { StatusBadge } from "@/components/primitives";
import { usd } from "@/lib/format";
import { clsx } from "@/lib/clsx";

type Phase = "idle" | "starting" | "live" | "finalizing" | "done" | "error";

// Browser-reachable API WebSocket origin (the one hop where the browser talks
// to the API directly — authenticated by the per-session token, not the key).
const WS_ORIGIN = process.env.NEXT_PUBLIC_ORPHEUS_WS_URL || "ws://127.0.0.1:8090";
const TARGET_RATE = 16000;

/** Nearest-neighbour resample of Float32 [-1,1] to 16 kHz Int16 PCM (LE). */
function toPCM16(input: Float32Array, inRate: number): Int16Array {
  const ratio = inRate / TARGET_RATE;
  const outLen = Math.max(0, Math.floor(input.length / ratio));
  const out = new Int16Array(outLen);
  for (let i = 0; i < outLen; i++) {
    const s = Math.max(-1, Math.min(1, input[Math.floor(i * ratio)] || 0));
    out[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
  }
  return out;
}

/**
 * Real server-side streaming transcription. Captures the mic, renders a live
 * WebAudio waveform, streams 16 kHz PCM16 frames over a WebSocket to the API
 * relay (→ the worker's whisper ASR), renders partial/final transcripts as they
 * arrive, and finalizes the real Orpheus session.
 */
export function LiveStreamStudio() {
  const router = useRouter();
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState<string | null>(null);
  const [session, setSession] = useState<StreamingSession | null>(null);
  const [elapsed, setElapsed] = useState(0);
  const [transcript, setTranscript] = useState("");
  const [interim, setInterim] = useState("");

  const canvasRef = useRef<HTMLCanvasElement>(null);
  const rafRef = useRef<number | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const audioCtxRef = useRef<AudioContext | null>(null);
  const analyserRef = useRef<AnalyserNode | null>(null);
  const procRef = useRef<ScriptProcessorNode | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const startedAtRef = useRef<number>(0);
  const transcriptRef = useRef<string>("");

  const draw = useCallback(() => {
    const canvas = canvasRef.current;
    const analyser = analyserRef.current;
    if (!canvas || !analyser) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const bins = analyser.frequencyBinCount;
    const data = new Uint8Array(bins);
    analyser.getByteFrequencyData(data);
    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth * dpr;
    const h = canvas.clientHeight * dpr;
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
    }
    ctx.clearRect(0, 0, w, h);
    const bars = 48;
    const step = Math.floor(bins / bars);
    const gap = 2 * dpr;
    const bw = (w - gap * (bars - 1)) / bars;
    for (let i = 0; i < bars; i++) {
      let sum = 0;
      for (let j = 0; j < step; j++) sum += data[i * step + j] || 0;
      const v = sum / step / 255;
      const bh = Math.max(2 * dpr, v * h);
      const x = i * (bw + gap);
      const y = (h - bh) / 2;
      const g = ctx.createLinearGradient(0, y, 0, y + bh);
      g.addColorStop(0, "#E0A340");
      g.addColorStop(1, "#B87A28");
      ctx.fillStyle = g;
      ctx.beginPath();
      ctx.roundRect(x, y, bw, bh, Math.min(bw / 2, 2 * dpr));
      ctx.fill();
    }
    rafRef.current = requestAnimationFrame(draw);
  }, []);

  const stopCapture = useCallback(() => {
    if (rafRef.current) cancelAnimationFrame(rafRef.current);
    rafRef.current = null;
    try {
      procRef.current?.disconnect();
    } catch {}
    procRef.current = null;
    streamRef.current?.getTracks().forEach((t) => t.stop());
    streamRef.current = null;
    audioCtxRef.current?.close().catch(() => {});
    audioCtxRef.current = null;
    analyserRef.current = null;
  }, []);

  useEffect(
    () => () => {
      stopCapture();
      wsRef.current?.close();
    },
    [stopCapture],
  );

  useEffect(() => {
    if (phase !== "live") return;
    const id = setInterval(() => setElapsed((Date.now() - startedAtRef.current) / 1000), 200);
    return () => clearInterval(id);
  }, [phase]);

  async function start() {
    setError(null);
    setTranscript("");
    setInterim("");
    transcriptRef.current = "";
    setElapsed(0);
    setPhase("starting");

    const created = await createStreamingAction();
    if (!created.ok || !created.data.ws_url) {
      setError(created.ok ? "Server didn't return a stream URL." : created.error);
      setPhase("error");
      return;
    }
    setSession(created.data);
    const wsUrl = WS_ORIGIN + created.data.ws_url;

    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch {
      setError("Microphone access was denied. Allow mic access to stream.");
      setPhase("error");
      return;
    }
    streamRef.current = stream;

    const AudioCtx =
      window.AudioContext || (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
    const audioCtx = new AudioCtx();
    audioCtxRef.current = audioCtx;
    const source = audioCtx.createMediaStreamSource(stream);
    const analyser = audioCtx.createAnalyser();
    analyser.fftSize = 256;
    analyser.smoothingTimeConstant = 0.8;
    source.connect(analyser);
    analyserRef.current = analyser;

    // Open the relay WebSocket and stream PCM once it's ready.
    const ws = new WebSocket(wsUrl);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;

    ws.onopen = () => {
      ws.send(JSON.stringify({ type: "start", sample_rate: TARGET_RATE }));
      const proc = audioCtx.createScriptProcessor(4096, 1, 1);
      procRef.current = proc;
      proc.onaudioprocess = (e) => {
        if (ws.readyState !== WebSocket.OPEN) return;
        const pcm = toPCM16(e.inputBuffer.getChannelData(0), audioCtx.sampleRate);
        if (pcm.length) ws.send(pcm.buffer);
      };
      // Route through a muted gain so onaudioprocess fires without echo.
      const mute = audioCtx.createGain();
      mute.gain.value = 0;
      source.connect(proc);
      proc.connect(mute);
      mute.connect(audioCtx.destination);

      startedAtRef.current = Date.now();
      setPhase("live");
      rafRef.current = requestAnimationFrame(draw);
    };

    ws.onmessage = (e) => {
      const raw = e.data;
      if (typeof raw !== "string") return;
      try {
        const ev = JSON.parse(raw);
        const text = (ev.text || ev.transcript || "").trim();
        if (ev.type === "partial") {
          setInterim(text);
        } else if (ev.type === "final" && text) {
          transcriptRef.current = (transcriptRef.current + " " + text).trim();
          setTranscript(transcriptRef.current);
          setInterim("");
        } else if (ev.type === "error") {
          setError((prev) => prev || `Stream: ${ev.error || "error"}`);
        }
      } catch {}
    };
    ws.onerror = () => {};
  }

  async function stopAndFinalize() {
    if (!session) return;
    const seconds = (Date.now() - startedAtRef.current) / 1000;
    setPhase("finalizing");

    // Ask the worker for a final flush, then give it a moment to reply.
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      try {
        ws.send(JSON.stringify({ type: "stop" }));
      } catch {}
      await new Promise((r) => setTimeout(r, 1200));
    }
    stopCapture();
    ws?.close();

    const finalText = (transcriptRef.current || interim || "").trim();
    const r = await finalizeStreamingAction(session.id, finalText, Math.max(0, seconds));
    if (!r.ok) {
      setError(r.error);
      setPhase("error");
      return;
    }
    setSession(r.data);
    setPhase("done");
    router.refresh();
  }

  const live = phase === "live";

  return (
    <div className="panel p-6">
      <div className="mb-4 flex items-center justify-between">
        <div className="label">Live session</div>
        {session && phase !== "idle" && <StatusBadge status={phase === "done" ? session.status : "live"} />}
      </div>

      <div className="relative overflow-hidden rounded-md border border-hairline bg-ground/50">
        <canvas ref={canvasRef} className="h-40 w-full" />
        {!live && phase !== "done" && (
          <div className="absolute inset-0 flex items-center justify-center">
            <span className="text-sm text-ink-lo">
              {phase === "starting" || phase === "finalizing" ? "…" : "Press record to capture live audio"}
            </span>
          </div>
        )}
        {live && (
          <div className="absolute left-3 top-3 flex items-center gap-2">
            <span className="h-2.5 w-2.5 animate-meter-pulse rounded-full bg-fail" />
            <span className="tnum text-xs text-ink-hi">{elapsed.toFixed(1)}s</span>
          </div>
        )}
      </div>

      {(live || phase === "done" || phase === "finalizing") && (
        <div className="mt-4">
          <div className="label mb-1.5">Live transcript · server-side ASR</div>
          <div className="min-h-[3rem] rounded-md border border-hairline bg-ground/40 p-3 text-sm text-ink-hi">
            {transcript || <span className="text-ink-lo">Listening…</span>}
            {interim && <span className="text-ink-lo"> {interim}</span>}
          </div>
        </div>
      )}

      {error && <div className="mt-3 rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{error}</div>}

      <div className="mt-4 flex items-center gap-2">
        {phase === "idle" || phase === "error" || phase === "done" ? (
          <button onClick={start} className="btn-brass">
            ● Record session
          </button>
        ) : (
          <button
            onClick={stopAndFinalize}
            disabled={phase !== "live"}
            className={clsx("btn", "hover:border-fail/50 hover:text-fail")}
          >
            ■ Stop &amp; finalize
          </button>
        )}
        {phase === "done" && session && (
          <span className="font-mono text-2xs text-ink-lo">
            finalized · {(session.audio_seconds ?? 0).toFixed(1)}s · {usd(session.cost_usd)}
          </span>
        )}
      </div>
    </div>
  );
}
