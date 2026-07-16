"""Tests for untrusted-parameter validation (DoS/crash hardening)."""

from __future__ import annotations

import pytest

from orpheus_workers.validation import (
    MAX_CHUNK_SECONDS,
    ParamError,
    parse_chunk_seconds,
    parse_time_range,
)


class TestParseTimeRange:
    def test_valid(self):
        assert parse_time_range({"start_seconds": 1, "end_seconds": 5}) == (1.0, 5.0)

    @pytest.mark.parametrize(
        "params",
        [
            {"start_seconds": "nan", "end_seconds": "inf"},  # non-finite bypass
            {"start_seconds": 5, "end_seconds": 1},  # inverted
            {"start_seconds": 5, "end_seconds": 5},  # zero-length
            {"start_seconds": -1, "end_seconds": 5},  # negative start
            {"start_seconds": 0, "end_seconds": 10**9},  # over the span cap
            {"end_seconds": 5},  # missing start
            {"start_seconds": "abc", "end_seconds": 5},  # non-numeric
        ],
    )
    def test_rejects(self, params):
        with pytest.raises(ParamError):
            parse_time_range(params)


class TestParseChunkSeconds:
    def test_default_when_missing(self):
        assert parse_chunk_seconds({}, default=60.0) == 60.0

    def test_valid(self):
        assert parse_chunk_seconds({"chunk_seconds": 30}) == 30.0

    @pytest.mark.parametrize(
        "value",
        [0, -5, "nan", "inf", 0.0001, MAX_CHUNK_SECONDS + 1],
    )
    def test_rejects(self, value):
        with pytest.raises(ParamError):
            parse_chunk_seconds({"chunk_seconds": value})


class TestFfmpegSliceGuards:
    """The finite/range guards in ffmpeg.slice run before find_ffmpeg(),
    so they are testable without the ffmpeg binary installed."""

    def test_rejects_nan_before_invoking_ffmpeg(self):
        from orpheus_workers.ffmpeg import FFmpegError
        from orpheus_workers.ffmpeg import slice as ffmpeg_slice

        with pytest.raises(FFmpegError):
            ffmpeg_slice("a.wav", "b.wav", float("nan"), float("inf"))

    def test_rejects_negative_start(self):
        from orpheus_workers.ffmpeg import FFmpegError
        from orpheus_workers.ffmpeg import slice as ffmpeg_slice

        with pytest.raises(FFmpegError):
            ffmpeg_slice("a.wav", "b.wav", -1.0, 5.0)
