"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { createStreamingAction, finalizeStreamingAction } from "@/app/actions/streaming";
import type { StreamingSession } from "@/lib/orpheus";
import { StatusBadge } from "@/components/primitives";
import { usd } from "@/lib/format";
import { clsx } from "@/lib/clsx";

type Phase = "idle" | "starting" | "live" | "finalizing" | "done" | "error";

/* Minimal typings for the browser SpeechRecognition API (not in lib.dom for all TS targets). */
interface SpeechRecognitionLike {
  continuous: boolean;
  interimResults: boolean;
  lang: string;
  onresult: ((e: { resultIndex: number; results: ArrayLike<{ 0: { transcript: string }; isFinal: boolean }> }) => void) | null;
  onerror: ((e: unknown) => void) | null;
  start: () => void;
  stop: () => void;
}

/**
 * A genuinely live streaming session: it opens the mic, renders a REAL
 * WebAudio frequency waveform from the input, transcribes live via the
 * browser's SpeechRecognition engine when available, and finalizes the real
 * Orpheus session (create → capture → finalize) with the captured transcript
 * and duration. (A server-side WebSocket ASR bridge is a separate backend
 * effort — the API exposes create/finalize, not a browser-reachable socket.)
 */
export function LiveStreamStudio() {
  const router = useRouter();
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState<string | null>(null);
  const [session, setSession] = useState<StreamingSession | null>(null);
  const [elapsed, setElapsed] = useState(0);
  const [transcript, setTranscript] = useState("");
  const [interim, setInterim] = useState("");
  const [asrSupported, setAsrSupported] = useState(true);

  const canvasRef = useRef<HTMLCanvasElement>(null);
  const rafRef = useRef<number | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const audioCtxRef = useRef<AudioContext | null>(null);
  const analyserRef = useRef<AnalyserNode | null>(null);
  const recognitionRef = useRef<SpeechRecognitionLike | null>(null);
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
      const v = sum / step / 255; // 0..1
      const bh = Math.max(2 * dpr, v * h);
      const x = i * (bw + gap);
      const y = (h - bh) / 2;
      const g = ctx.createLinearGradient(0, y, 0, y + bh);
      g.addColorStop(0, "#E0A340");
      g.addColorStop(1, "#B87A28");
      ctx.fillStyle = g;
      const r = Math.min(bw / 2, 2 * dpr);
      ctx.beginPath();
      ctx.roundRect(x, y, bw, bh, r);
      ctx.fill();
    }
    rafRef.current = requestAnimationFrame(draw);
  }, []);

  const stopCapture = useCallback(() => {
    if (rafRef.current) cancelAnimationFrame(rafRef.current);
    rafRef.current = null;
    recognitionRef.current?.stop();
    recognitionRef.current = null;
    streamRef.current?.getTracks().forEach((t) => t.stop());
    streamRef.current = null;
    audioCtxRef.current?.close().catch(() => {});
    audioCtxRef.current = null;
    analyserRef.current = null;
  }, []);

  useEffect(() => () => stopCapture(), [stopCapture]);

  // elapsed timer while live
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

    // 1. create the real Orpheus streaming session
    const created = await createStreamingAction();
    if (!created.ok) {
      setError(created.error);
      setPhase("error");
      return;
    }
    setSession(created.data);

    // 2. open the mic + WebAudio graph
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      streamRef.current = stream;
      const AudioCtx = window.AudioContext || (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
      const audioCtx = new AudioCtx();
      audioCtxRef.current = audioCtx;
      const source = audioCtx.createMediaStreamSource(stream);
      const analyser = audioCtx.createAnalyser();
      analyser.fftSize = 256;
      analyser.smoothingTimeConstant = 0.8;
      source.connect(analyser);
      analyserRef.current = analyser;
      startedAtRef.current = Date.now();
      setPhase("live");
      rafRef.current = requestAnimationFrame(draw);
    } catch {
      setError("Microphone access was denied. Allow mic access to stream.");
      setPhase("error");
      stopCapture();
      return;
    }

    // 3. live transcription via the browser speech engine (best-effort)
    const SR =
      (window as unknown as { SpeechRecognition?: new () => SpeechRecognitionLike }).SpeechRecognition ||
      (window as unknown as { webkitSpeechRecognition?: new () => SpeechRecognitionLike }).webkitSpeechRecognition;
    if (!SR) {
      setAsrSupported(false);
    } else {
      try {
        const rec = new SR();
        rec.continuous = true;
        rec.interimResults = true;
        rec.lang = "en-US";
        rec.onresult = (e) => {
          let finalTxt = "";
          let interimTxt = "";
          for (let i = e.resultIndex; i < e.results.length; i++) {
            const r = e.results[i];
            if (r.isFinal) finalTxt += r[0].transcript;
            else interimTxt += r[0].transcript;
          }
          if (finalTxt) {
            transcriptRef.current = (transcriptRef.current + " " + finalTxt).trim();
            setTranscript(transcriptRef.current);
          }
          setInterim(interimTxt);
        };
        rec.onerror = () => {};
        recognitionRef.current = rec;
        rec.start();
      } catch {
        setAsrSupported(false);
      }
    }
  }

  async function stopAndFinalize() {
    if (!session) return;
    const seconds = (Date.now() - startedAtRef.current) / 1000;
    const finalText = (transcriptRef.current || interim || "").trim();
    setPhase("finalizing");
    stopCapture();
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
        {session && phase !== "idle" && <StatusBadge status={phase === "done" ? session.status : "streaming"} />}
      </div>

      {/* Waveform stage */}
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

      {/* Live transcript */}
      {(live || phase === "done") && (
        <div className="mt-4">
          <div className="label mb-1.5">Transcript {asrSupported ? "" : "· (browser speech engine unavailable)"}</div>
          <div className="min-h-[3rem] rounded-md border border-hairline bg-ground/40 p-3 text-sm text-ink-hi">
            {transcript || <span className="text-ink-lo">{asrSupported ? "Listening…" : "Live transcription isn't supported in this browser; the session still captures duration."}</span>}
            {interim && <span className="text-ink-lo"> {interim}</span>}
          </div>
        </div>
      )}

      {error && <div className="mt-3 rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{error}</div>}

      {/* Controls */}
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
            ■ Stop & finalize
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
