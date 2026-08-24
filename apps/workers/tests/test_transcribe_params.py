"""Unit tests for transcribe param resolution (no whisper model needed)."""

from __future__ import annotations

from orpheus_workers.processors.transcribe import _transcribe_opts


def test_tier_maps_to_model_and_explicit_model_wins() -> None:
    assert _transcribe_opts({"tier": "accurate"})["model_size"] == "large-v3"
    assert _transcribe_opts({"tier": "fast"})["model_size"] == "distil-large-v3"
    assert _transcribe_opts({"tier": "balanced"})["model_size"] == "large-v3-turbo"
    # An explicit model always beats the tier.
    assert _transcribe_opts({"tier": "fast", "model": "large-v3"})["model_size"] == "large-v3"
    # Unknown/empty tier falls back to the env default (tiny.en in tests).
    assert _transcribe_opts({"tier": "nope"})["model_size"] == "tiny.en"
    assert _transcribe_opts({})["model_size"] == "tiny.en"
