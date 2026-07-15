"""Validation helpers for untrusted job parameters.

Job ``params`` come from the API/DB as opaque JSON. Feeding them straight
into ffmpeg args or chunk-count arithmetic is a denial-of-service vector:
``float("nan")`` slips past a naive ``end <= start`` guard (all NaN
comparisons are False), ``chunk_seconds=0`` raises ZeroDivisionError, and
a tiny ``chunk_seconds`` on a long file explodes into millions of ffmpeg
+ whisper invocations. These helpers reject non-finite / out-of-range
values up front with a clear ParamError.
"""

from __future__ import annotations

import math
from typing import Any

# Absolute safety caps. These are deliberately generous — they exist to
# stop pathological inputs, not to encode product policy.
MAX_SLICE_SECONDS = 24 * 60 * 60  # 24h
MIN_CHUNK_SECONDS = 1.0
MAX_CHUNK_SECONDS = 60 * 60.0  # 1h
MAX_CHUNKS = 10_000


class ParamError(ValueError):
    """Raised when a job parameter is missing or out of range."""


def _finite_float(params: dict[str, Any], key: str) -> float:
    if key not in params:
        raise ParamError(f"missing required param: {key}")
    try:
        v = float(params[key])
    except (TypeError, ValueError):
        raise ParamError(f"param {key} is not a number") from None
    if not math.isfinite(v):
        raise ParamError(f"param {key} must be finite")
    return v


def parse_time_range(params: dict[str, Any]) -> tuple[float, float]:
    """Return a validated (start_seconds, end_seconds) pair.

    Enforces: both finite, 0 <= start < end, and the span within
    MAX_SLICE_SECONDS. Raises ParamError otherwise.
    """
    start = _finite_float(params, "start_seconds")
    end = _finite_float(params, "end_seconds")
    if start < 0:
        raise ParamError("start_seconds must be >= 0")
    if end <= start:
        raise ParamError("end_seconds must be > start_seconds")
    if end - start > MAX_SLICE_SECONDS:
        raise ParamError(f"slice span exceeds {MAX_SLICE_SECONDS}s cap")
    return start, end


def parse_chunk_seconds(params: dict[str, Any], default: float = 60.0) -> float:
    """Return a validated chunk length in seconds.

    Missing → default. Present values must be finite and within
    [MIN_CHUNK_SECONDS, MAX_CHUNK_SECONDS]. Raises ParamError otherwise.
    """
    if params.get("chunk_seconds") is None:
        return default
    v = _finite_float(params, "chunk_seconds")
    if v < MIN_CHUNK_SECONDS or v > MAX_CHUNK_SECONDS:
        raise ParamError(
            f"chunk_seconds must be within [{MIN_CHUNK_SECONDS}, {MAX_CHUNK_SECONDS}]"
        )
    return v
