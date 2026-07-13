from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path
from typing import Any


class FFprobeError(Exception):
    pass


def find_ffprobe() -> str:
    """Return the path to the ffprobe binary, or raise if not on PATH."""
    p = shutil.which("ffprobe")
    if p is None:
        raise FFprobeError("ffprobe not found on PATH; is ffmpeg installed in the worker image?")
    return p


def probe(path: str | Path) -> dict[str, Any]:
    """Run ffprobe and return the parsed JSON output.

    Raises FFprobeError if ffprobe is missing, exits non-zero, or
    returns no audio stream.
    """
    bin = find_ffprobe()
    out = subprocess.run(
        [bin, "-v", "error", "-show_format", "-show_streams", "-of", "json", str(path)],
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )
    if out.returncode != 0:
        raise FFprobeError(f"ffprobe exited {out.returncode}: {out.stderr.strip()}")
    try:
        data = json.loads(out.stdout)
    except json.JSONDecodeError as e:
        raise FFprobeError(f"ffprobe returned invalid JSON: {e}") from e
    return data


def extract_audio_metadata(data: dict[str, Any]) -> dict[str, Any]:
    """Pull the audio-stream + format fields we care about.

    Returns a dict with keys: codec (str|None), sample_rate (int|None),
    channels (int|None), duration_seconds (float|None). Missing
    fields are None.
    """
    streams = data.get("streams") or []
    audio = next((s for s in streams if s.get("codec_type") == "audio"), None)
    fmt = data.get("format") or {}
    out: dict[str, Any] = {
        "codec": (audio or {}).get("codec_name") if audio else None,
        "sample_rate": int(audio["sample_rate"]) if audio and audio.get("sample_rate") else None,
        "channels": int(audio["channels"]) if audio and audio.get("channels") else None,
        "duration_seconds": float(fmt["duration"]) if fmt.get("duration") else None,
    }
    return out
