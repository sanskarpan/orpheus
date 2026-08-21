from __future__ import annotations

import os
import wave
from pathlib import Path
from typing import Any

import numpy as np
import structlog

from ..ffmpeg import FFmpegError, convert_to_wav_16k_mono, extract_channel_16k_mono
from ..ffmpeg import slice as ffmpeg_slice
from ..ffprobe import FFprobeError
from ..ffprobe import extract_audio_metadata as _audio_meta
from ..ffprobe import probe as _ffprobe
from ..formatting import format_transcript
from ..redact import maybe_redact
from ..transcribe import TranscribeError, transcribe
from ..validation import MAX_CHUNKS, ParamError, parse_chunk_seconds
from . import register_processor

logger = structlog.get_logger(__name__)

DEFAULT_CHUNK_SECONDS = 60


@register_processor(
    "transcribe",
    display_name="Transcribe",
    description="Transcribe an audio artifact with faster-whisper (chunked for long files).",
    tier="cpu_medium",
    timeout_seconds=1800,
    cost_per_job_usd=0.02,
    model_id="whisper",
    model_version_id=f"whisper-{os.environ.get('ORPHEUS_WORKER_WHISPER_MODEL', 'tiny.en')}",
)
async def transcribe_artifact(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Download the artifact, transcribe with whisper, return the result.

    For long files (duration > ``chunk_seconds``), splits the wav into
    ``chunk_seconds``-sized pieces, transcribes each, and concatenates
    the transcripts. ``chunk_seconds`` is read from
    ``jobs.params.chunk_seconds`` (default 60). The on-wire shape is
    unchanged: ``{text, segments, language, duration_seconds}``.
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    artifact_id = job["artifact_id"]
    params = job["params"] or {}
    if isinstance(params, str):
        import json as _json

        params = _json.loads(params)
    # Validate chunk_seconds: reject 0 (ZeroDivisionError), negative, NaN/
    # Inf, and out-of-range values that would explode into millions of
    # ffmpeg + whisper invocations.
    chunk_seconds = parse_chunk_seconds(params, default=DEFAULT_CHUNK_SECONDS)
    word_timestamps = bool(params.get("word_timestamps", False))
    topts = _transcribe_opts(params)

    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", artifact_id)
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    Path(work_dir).mkdir(parents=True, exist_ok=True)
    src_suffix = Path(art["s3_key"]).suffix or ".bin"
    src_path = Path(work_dir) / f"{job_id}.src{src_suffix}"
    wav_path = Path(work_dir) / f"{job_id}.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src_path))
        try:
            convert_to_wav_16k_mono(src_path, wav_path)
        except FFmpegError as e:
            logger.error("worker.ffmpeg_convert_failed", job_id=job_id, err=str(e))
            raise

        duration = _wav_duration(wav_path)
        # Code-switching (opt-in): VAD-segment at pauses and transcribe each
        # segment independently with per-segment language auto-detection.
        if params.get("multilang"):
            result = _transcribe_multilang(
                wav_path, duration, word_timestamps, topts, str(work_dir), job_id
            )
            _maybe_align(result, wav_path, params, job_id)
            _maybe_format(result, params)
            redactions = maybe_redact(result, params)
            if redactions:
                result["redactions"] = redactions
            return result

        # Per-channel (opt-in): transcribe each audio channel separately (e.g. a
        # stereo call-center recording → agent / customer), so channels never bleed
        # into one another. Falls through to the normal path for mono sources.
        if params.get("per_channel"):
            try:
                n_ch = int(_audio_meta(_ffprobe(src_path)).get("channels") or 1)
            except (FFprobeError, ValueError, KeyError, TypeError):
                n_ch = 1  # probe best-effort → treat as mono
            if n_ch > 1:
                result = _transcribe_per_channel(
                    src_path,
                    n_ch,
                    duration,
                    chunk_seconds,
                    word_timestamps,
                    topts,
                    params,
                    str(work_dir),
                    job_id,
                )
                _maybe_align(result, wav_path, params, job_id)
                _maybe_format(result, params)
                redactions = maybe_redact(result, params)
                if redactions:
                    result["redactions"] = redactions
                return result

        result = _transcribe_wav_full(
            wav_path,
            duration,
            chunk_seconds,
            word_timestamps,
            topts,
            params,
            str(work_dir),
            job_id,
        )
        _maybe_align(result, wav_path, params, job_id)
        _maybe_format(result, params)
        redactions = maybe_redact(result, params)
        if redactions:
            result["redactions"] = redactions
        return result
    finally:
        for p in (src_path, wav_path):
            try:
                os.unlink(p)
            except FileNotFoundError:
                pass


def _transcribe_opts(params: dict[str, Any]) -> dict[str, Any]:
    """Resolve per-job transcription options from job params + env defaults.

    ``model`` selects the whisper model per request (falling back to the
    ``ORPHEUS_WORKER_WHISPER_MODEL`` env default); ``language`` forces a
    language code (default ``None`` = auto-detect); ``vocabulary`` (list) and
    ``initial_prompt`` (str) bias recognition toward domain terms.
    """
    model = params.get("model")
    model_size = (
        model
        if isinstance(model, str) and model.strip()
        else os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    )
    lang = params.get("language")
    language = lang.strip() if isinstance(lang, str) and lang.strip() else None
    vocab = params.get("vocabulary")
    vocabulary = vocab if isinstance(vocab, list) else None
    prompt = params.get("initial_prompt")
    initial_prompt = prompt if isinstance(prompt, str) and prompt.strip() else None
    return {
        "model_size": model_size,
        "model_dir": os.environ.get("ORPHEUS_WORKER_WHISPER_DIR") or None,
        "language": language,
        "vocabulary": vocabulary,
        "initial_prompt": initial_prompt,
    }


def _transcribe_one(
    wav_path: Path,
    offset: float,
    word_timestamps: bool = False,
    opts: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Transcribe a single wav, optionally shifting segment timestamps by ``offset``."""
    opts = opts or _transcribe_opts({})
    result = transcribe(wav_path, word_timestamps=word_timestamps, **opts)
    for seg in result.get("segments") or []:
        seg["start"] = float(seg.get("start", 0.0)) + offset
        seg["end"] = float(seg.get("end", 0.0)) + offset
    return result


def _transcribe_wav_full(
    wav_path: Path,
    duration: float,
    chunk_seconds: float,
    word_timestamps: bool,
    topts: dict[str, Any],
    params: dict[str, Any],
    work_dir: str,
    job_id: str,
) -> dict[str, Any]:
    """Transcribe a whole 16kHz-mono wav (single pass, or VAD/fixed chunked for
    long files), returning ``{text, segments, language, duration_seconds}``.

    This is the shared engine for both the default path and per-channel
    transcription; formatting/alignment/redaction are applied by the caller.
    """
    if duration <= chunk_seconds:
        return _transcribe_one(wav_path, offset=0.0, word_timestamps=word_timestamps, opts=topts)

    # Split the file into chunks. "vad" (default) cuts at silence so words are
    # never sliced mid-utterance and silence is skipped; "fixed" reproduces the
    # legacy arithmetic 60 s grid for reproducibility.
    chunking_mode = str(params.get("chunking", "vad")).strip().lower()
    if chunking_mode == "fixed":
        n_chunks = int(duration // chunk_seconds) + (1 if duration % chunk_seconds else 0)
        boundaries = [
            (i * chunk_seconds, min((i + 1) * chunk_seconds, duration)) for i in range(n_chunks)
        ]
    else:
        boundaries = _vad_chunk_boundaries(wav_path, chunk_seconds)
    if len(boundaries) > MAX_CHUNKS:
        raise ParamError(
            f"chunking would produce {len(boundaries)} chunks (> {MAX_CHUNKS}); "
            "increase chunk_seconds"
        )
    all_segments: list[dict] = []
    all_text: list[str] = []
    language = topts["language"] or "en"
    for i, (start, end) in enumerate(boundaries):
        chunk_path = Path(work_dir) / f"{job_id}.chunk{i}.wav"
        try:
            ffmpeg_slice(wav_path, chunk_path, start, end)
            result = transcribe(chunk_path, word_timestamps=word_timestamps, **topts)
        except (FFmpegError, TranscribeError) as e:
            logger.error("worker.chunk_failed", job_id=job_id, chunk=i, err=str(e))
            raise
        finally:
            try:
                os.unlink(chunk_path)
            except FileNotFoundError:
                pass
        for seg in result.get("segments") or []:
            new_seg = {
                "start": float(seg.get("start", 0.0)) + start,
                "end": float(seg.get("end", 0.0)) + start,
                "text": seg.get("text", ""),
            }
            if seg.get("words"):
                new_seg["words"] = [
                    {
                        "start": float(w.get("start", 0.0)) + start,
                        "end": float(w.get("end", 0.0)) + start,
                        "word": w.get("word", ""),
                        "confidence": w.get("confidence", 0.0),
                    }
                    for w in seg["words"]
                ]
            all_segments.append(new_seg)
        text = result.get("text", "").strip()
        if text:
            all_text.append(text)
        if i == 0 and result.get("language"):
            language = result["language"]
    return {
        "text": " ".join(all_text).strip(),
        "segments": all_segments,
        "language": language,
        "duration_seconds": duration,
    }


def _transcribe_per_channel(
    src_path: Path,
    n_channels: int,
    duration: float,
    chunk_seconds: float,
    word_timestamps: bool,
    topts: dict[str, Any],
    params: dict[str, Any],
    work_dir: str,
    job_id: str,
) -> dict[str, Any]:
    """Transcribe each audio channel independently (PRD 13 §4.7).

    Extracts each channel to its own 16kHz mono wav (no cross-channel bleed),
    transcribes it through :func:`_transcribe_wav_full`, tags every segment with
    ``channel``, and returns a merged, time-ordered transcript plus a
    ``channels: [{index, label}]`` map. ``params.channel_labels`` names them.
    """
    raw_labels = params.get("channel_labels")
    labels = raw_labels if isinstance(raw_labels, list) else []
    all_segments: list[dict] = []
    language = topts["language"] or "en"
    channels_map: list[dict[str, Any]] = []
    for ch in range(n_channels):
        ch_wav = Path(work_dir) / f"{job_id}.ch{ch}.wav"
        try:
            extract_channel_16k_mono(src_path, ch_wav, ch)
            ch_dur = _wav_duration(ch_wav)
            r = _transcribe_wav_full(
                ch_wav,
                ch_dur,
                chunk_seconds,
                word_timestamps,
                topts,
                params,
                work_dir,
                f"{job_id}.ch{ch}",
            )
        finally:
            try:
                os.unlink(ch_wav)
            except FileNotFoundError:
                pass
        label = str(labels[ch]) if ch < len(labels) else f"channel_{ch}"
        channels_map.append({"index": ch, "label": label})
        for seg in r.get("segments") or []:
            seg["channel"] = ch
            all_segments.append(seg)
        if ch == 0 and r.get("language"):
            language = r["language"]
    all_segments.sort(key=lambda s: (float(s.get("start", 0.0)), int(s.get("channel", 0))))
    return {
        "text": " ".join(s.get("text", "").strip() for s in all_segments if s.get("text")).strip(),
        "segments": all_segments,
        "language": language,
        "duration_seconds": duration,
        "channels": channels_map,
    }


def _maybe_format(result: dict[str, Any], params: dict) -> None:
    """Apply ITN/smart formatting in place when ``params.formatting.enabled``.

    Runs before redaction so PII regexes see normalized ("written") numbers.
    """
    fmt = params.get("formatting")
    if isinstance(fmt, dict) and fmt.get("enabled"):
        format_transcript(result, fmt)


def _maybe_align(result: dict[str, Any], wav_path: Path, params: dict, job_id: str) -> None:
    """Forced-align word timestamps in place when ``params.alignment == "forced"``.

    Runs before formatting so ITN's span-preserving collapse sees the tightened
    word boundaries. Degrades gracefully: on any alignment failure the whisper
    timings are kept and the job still succeeds (alignment is an enhancement,
    not a correctness requirement).
    """
    if str(params.get("alignment", "")).strip().lower() != "forced":
        return
    from ..align import AlignError, align_transcript

    try:
        align_transcript(wav_path, result, language=result.get("language"))
    except AlignError as e:
        logger.warning("worker.align_failed", job_id=job_id, err=str(e))


# Max segment length for the multilang path; VAD still cuts earlier at pauses,
# which is also where code-switches typically occur.
_MULTILANG_CHUNK_SECONDS = 30.0


def _vad_speech_regions(
    wav_path: Path,
    min_silence_s: float = 0.4,
    min_speech_s: float = 0.4,
    max_region_s: float = 30.0,
) -> list[tuple[float, float]]:
    """Utterance segmentation: return ``[(start, end)]`` speech regions separated
    by silence gaps ≥ ``min_silence_s``. Unlike ``_vad_chunk_boundaries`` (which
    only splits oversized files), this cuts at *every* pause — the natural place a
    speaker switches language — so each region can be transcribed on its own.
    """
    with wave.open(str(wav_path), "rb") as w:
        sr = w.getframerate() or 16000
        audio = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(np.float32)
    duration = len(audio) / sr if sr else 0.0
    frame = max(1, int(_VAD_FRAME_SECONDS * sr))
    n_frames = len(audio) // frame
    if n_frames < 2:
        return [(0.0, duration)]
    frames = audio[: n_frames * frame].reshape(n_frames, frame)
    rms = np.sqrt(np.mean(frames**2, axis=1) + 1e-9)
    threshold = _VAD_ENERGY_RATIO * float(np.sqrt(np.mean(audio**2) + 1e-9))
    is_speech = rms >= threshold
    frame_dur = frame / sr
    min_sil_frames = max(1, int(min_silence_s / frame_dur))

    regions: list[tuple[float, float]] = []
    in_speech = False
    seg_start = 0.0
    silence_run = 0
    for fi in range(n_frames):
        if is_speech[fi]:
            if not in_speech:
                seg_start = fi * frame_dur
                in_speech = True
            silence_run = 0
        elif in_speech:
            silence_run += 1
            if silence_run >= min_sil_frames:
                seg_end = (fi - silence_run + 1) * frame_dur
                if seg_end - seg_start >= min_speech_s:
                    regions.append((seg_start, seg_end))
                in_speech = False
                silence_run = 0
    if in_speech and duration - seg_start >= min_speech_s:
        regions.append((seg_start, duration))
    if not regions:
        return [(0.0, duration)]
    # Cap any over-long region at max_region_s (rare — continuous monologue).
    out: list[tuple[float, float]] = []
    for s, e in regions:
        while e - s > max_region_s:
            out.append((s, s + max_region_s))
            s += max_region_s
        out.append((s, e))
    return out


def _transcribe_multilang(
    wav_path: Path,
    duration: float,
    word_timestamps: bool,
    topts: dict[str, Any],
    work_dir: str,
    job_id: str,
) -> dict[str, Any]:
    """Code-switch transcription: VAD-segment at pauses, transcribe each segment
    independently with **auto language detection** (``language=None``), and tag
    each segment with its detected language.

    Small local langid models are unreliable on real audio, so we let the full
    transcription model (large-v3-turbo on the Modal backend) detect + transcribe
    each segment — accurate per-segment language *and* correct per-language text.
    Adds ``segments[].language`` and a top-level ``languages: [{code, ratio}]``
    summary when >1 language is present; ``language`` stays the duration-dominant
    one (backward compatible).
    """
    boundaries = _vad_speech_regions(wav_path, max_region_s=_MULTILANG_CHUNK_SECONDS)
    if len(boundaries) > MAX_CHUNKS:
        raise ParamError(f"multilang chunking produced {len(boundaries)} chunks (> {MAX_CHUNKS})")
    opts = {**topts, "language": None}  # force per-segment auto-detect
    all_segments: list[dict] = []
    all_text: list[str] = []
    dur_by_lang: dict[str, float] = {}
    for i, (start, end) in enumerate(boundaries):
        chunk_path = Path(work_dir) / f"{job_id}.ml{i}.wav"
        try:
            ffmpeg_slice(wav_path, chunk_path, start, end)
            result = transcribe(chunk_path, word_timestamps=word_timestamps, **opts)
        except (FFmpegError, TranscribeError) as e:
            logger.error("worker.multilang_chunk_failed", job_id=job_id, chunk=i, err=str(e))
            raise
        finally:
            try:
                os.unlink(chunk_path)
            except FileNotFoundError:
                pass
        lang = result.get("language")
        for seg in result.get("segments") or []:
            new_seg: dict[str, Any] = {
                "start": float(seg.get("start", 0.0)) + start,
                "end": float(seg.get("end", 0.0)) + start,
                "text": seg.get("text", ""),
                "language": lang,
            }
            if seg.get("words"):
                new_seg["words"] = [
                    {
                        "start": float(w.get("start", 0.0)) + start,
                        "end": float(w.get("end", 0.0)) + start,
                        "word": w.get("word", ""),
                        "confidence": w.get("confidence", 0.0),
                    }
                    for w in seg["words"]
                ]
            all_segments.append(new_seg)
        text = (result.get("text", "") or "").strip()
        if text:
            all_text.append(text)
        if lang:
            dur_by_lang[lang] = dur_by_lang.get(lang, 0.0) + (end - start)
    dominant = (
        max(dur_by_lang, key=lambda k: dur_by_lang[k])
        if dur_by_lang
        else (topts["language"] or "en")
    )
    out: dict[str, Any] = {
        "text": " ".join(all_text).strip(),
        "segments": all_segments,
        "language": dominant,
        "duration_seconds": duration,
    }
    if len(dur_by_lang) > 1:
        total = sum(dur_by_lang.values()) or 1.0
        out["languages"] = [
            {"code": k, "ratio": round(v / total, 3)}
            for k, v in sorted(dur_by_lang.items(), key=lambda kv: -kv[1])
        ]
    return out


def _wav_duration(path: Path) -> float:
    """Return the duration in seconds of a 16kHz mono wav file."""
    with wave.open(str(path), "rb") as w:
        frames = w.getnframes()
        rate = w.getframerate()
        if rate == 0:
            return 0.0
        return frames / float(rate)


# VAD chunking tunables.
_VAD_FRAME_SECONDS = 0.03  # energy frame size
_VAD_ENERGY_RATIO = 0.35  # frame RMS below this × global RMS = silence
_VAD_MIN_CHUNK_SECONDS = 1.0  # don't cut a chunk shorter than this


def _vad_chunk_boundaries(wav_path: Path, max_chunk_seconds: float) -> list[tuple[float, float]]:
    """Return ``[(start, end)]`` chunks of ≤ ``max_chunk_seconds`` that cut only at
    silence (never inside a speech region), so words aren't sliced mid-utterance.

    Energy VAD (frame RMS vs the file's global RMS) marks silence frames; a chunk
    ends at the last silence seen before it reaches the max length, falling back to
    a hard cut only when speech is continuous past the limit.
    """
    with wave.open(str(wav_path), "rb") as w:
        sr = w.getframerate() or 16000
        raw = w.readframes(w.getnframes())
    audio = np.frombuffer(raw, dtype=np.int16).astype(np.float32)
    duration = len(audio) / sr if sr else 0.0
    if duration <= max_chunk_seconds or len(audio) == 0:
        return [(0.0, duration)]

    frame = max(1, int(_VAD_FRAME_SECONDS * sr))
    n_frames = len(audio) // frame
    if n_frames < 2:
        return [(0.0, duration)]
    frames = audio[: n_frames * frame].reshape(n_frames, frame)
    rms = np.sqrt(np.mean(frames**2, axis=1) + 1e-9)
    global_rms = float(np.sqrt(np.mean(audio**2) + 1e-9))
    threshold = _VAD_ENERGY_RATIO * global_rms
    frame_dur = frame / sr

    boundaries: list[tuple[float, float]] = []
    cur_start = 0.0
    last_silence: float | None = None
    for fi in range(n_frames):
        t = fi * frame_dur
        if rms[fi] < threshold:
            last_silence = t + frame_dur / 2.0  # middle of the silent frame
        if t - cur_start >= max_chunk_seconds:
            if last_silence is not None and last_silence > cur_start + _VAD_MIN_CHUNK_SECONDS:
                cut = last_silence
            else:
                cut = t  # continuous speech past the limit → unavoidable hard cut
            boundaries.append((cur_start, cut))
            cur_start = cut
            last_silence = None
    if cur_start < duration:
        boundaries.append((cur_start, duration))
    return boundaries
