from __future__ import annotations

import math
import shutil
import subprocess
from pathlib import Path


class FFmpegError(Exception):
    pass


def find_ffmpeg() -> str:
    p = shutil.which("ffmpeg")
    if p is None:
        raise FFmpegError("ffmpeg not found on PATH; is ffmpeg installed in the worker image?")
    return p


def slice(src: str | Path, dst: str | Path, start_seconds: float, end_seconds: float) -> None:
    """Extract the time range [start, end] from src to dst.

    Uses -c copy (no re-encode) for speed. Raises FFmpegError on
    ffmpeg non-zero exit.
    """
    # Reject non-finite values explicitly: NaN slips past the < guard
    # below (every NaN comparison is False) and would reach ffmpeg args.
    if not (math.isfinite(start_seconds) and math.isfinite(end_seconds)):
        raise FFmpegError("start_seconds and end_seconds must be finite")
    if start_seconds < 0:
        raise FFmpegError(f"start_seconds ({start_seconds}) must be >= 0")
    if end_seconds <= start_seconds:
        raise FFmpegError(f"end_seconds ({end_seconds}) must be > start_seconds ({start_seconds})")
    bin = find_ffmpeg()
    out = subprocess.run(
        [
            bin,
            "-y",
            "-v",
            "error",
            "-i",
            str(src),
            "-ss",
            f"{start_seconds}",
            "-to",
            f"{end_seconds}",
            "-c",
            "copy",
            str(dst),
        ],
        capture_output=True,
        text=True,
        check=False,
        timeout=60,
    )
    if out.returncode != 0:
        raise FFmpegError(f"ffmpeg exited {out.returncode}: {out.stderr.strip()}")


def convert_to_wav_16k_mono(src: str | Path, dst: str | Path) -> None:
    """Convert any audio file to 16kHz mono 16-bit PCM wav, the
    input format whisper prefers. Raises FFmpegError on failure.
    """
    bin = find_ffmpeg()
    out = subprocess.run(
        [
            bin,
            "-y",
            "-v",
            "error",
            "-i",
            str(src),
            "-ar",
            "16000",
            "-ac",
            "1",
            "-c:a",
            "pcm_s16le",
            str(dst),
        ],
        capture_output=True,
        text=True,
        check=False,
        timeout=120,
    )
    if out.returncode != 0:
        raise FFmpegError(f"ffmpeg convert exited {out.returncode}: {out.stderr.strip()}")


def extract_channel_16k_mono(src: str | Path, dst: str | Path, channel_index: int) -> None:
    """Extract a single audio channel to a 16kHz mono 16-bit wav (PRD 13 §4.7).

    Uses a ``pan`` filter to isolate channel ``channel_index`` (0-based) with no
    downmix bleed from the other channels, so per-channel transcription keeps
    speakers fully separated. Raises FFmpegError on failure.
    """
    bin = find_ffmpeg()
    out = subprocess.run(
        [
            bin,
            "-y",
            "-v",
            "error",
            "-i",
            str(src),
            "-af",
            f"pan=mono|c0=c{int(channel_index)}",
            "-ar",
            "16000",
            "-ac",
            "1",
            "-c:a",
            "pcm_s16le",
            str(dst),
        ],
        capture_output=True,
        text=True,
        check=False,
        timeout=120,
    )
    if out.returncode != 0:
        raise FFmpegError(f"ffmpeg channel-extract exited {out.returncode}: {out.stderr.strip()}")


def mute_ranges(src: str | Path, dst: str | Path, ranges: list[tuple[float, float]]) -> None:
    """Write ``src`` to ``dst`` (16-bit PCM wav) with each [start, end] second range
    silenced — audio PII "beep"/mute redaction (PRD 13 §4.5).

    Ranges are applied with a single ``volume=0`` filter gated by an ``enable``
    expression, so any number of spans are muted in one pass. An empty range list
    just transcodes the audio unchanged. Raises FFmpegError on failure.
    """
    bin = find_ffmpeg()
    args = [bin, "-y", "-v", "error", "-i", str(src)]
    if ranges:
        enable = "+".join(f"between(t,{s:.3f},{e:.3f})" for s, e in ranges)
        args += ["-af", f"volume=0:enable='{enable}'"]
    args += ["-c:a", "pcm_s16le", str(dst)]
    out = subprocess.run(args, capture_output=True, text=True, check=False, timeout=300)
    if out.returncode != 0:
        raise FFmpegError(f"ffmpeg mute exited {out.returncode}: {out.stderr.strip()}")


def cut_ranges(src: str | Path, dst: str | Path, ranges: list[tuple[float, float]]) -> None:
    """Write ``src`` to ``dst`` (16-bit PCM wav) with each [start, end] second range
    **removed** and the remaining audio concatenated (PRD 16 filler-word removal).

    Uses ``aselect`` to drop the sample ranges then ``asetpts`` to close the gaps,
    so the output is genuinely shorter (Descript-style), not just silenced. An
    empty range list transcodes unchanged. Raises FFmpegError on failure.
    """
    bin = find_ffmpeg()
    args = [bin, "-y", "-v", "error", "-i", str(src)]
    if ranges:
        # commas inside between() must be escaped within the single -af filter string
        conds = "+".join(f"between(t\\,{s:.3f}\\,{e:.3f})" for s, e in ranges)
        args += ["-af", f"aselect='not({conds})',asetpts=N/SR/TB"]
    args += ["-c:a", "pcm_s16le", str(dst)]
    out = subprocess.run(args, capture_output=True, text=True, check=False, timeout=300)
    if out.returncode != 0:
        raise FFmpegError(f"ffmpeg cut exited {out.returncode}: {out.stderr.strip()}")
