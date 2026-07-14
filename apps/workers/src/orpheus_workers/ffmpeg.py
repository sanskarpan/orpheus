from __future__ import annotations

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
