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


def test_reference_text_replaces_segments_before_align(monkeypatch, tmp_path):
    import wave

    from orpheus_workers.processors import transcribe as T

    p = tmp_path / "a.wav"
    with wave.open(str(p), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"\x00\x00" * 16000)  # 1 second

    captured = {}

    def fake_align(wav_path, result, language=None):
        captured["text"] = result["text"]
        captured["segments"] = result["segments"]

    monkeypatch.setattr("orpheus_workers.align.align_transcript", fake_align)
    result = {
        "text": "asr output",
        "language": "en",
        "segments": [{"start": 0.0, "end": 1.0, "text": "asr output", "words": []}],
    }
    T._maybe_align(result, p, {"alignment": "forced", "reference_text": "the known script"}, "job1")
    assert captured["text"] == "the known script"
    assert captured["segments"][0]["text"] == "the known script"
    assert captured["segments"][0]["end"] == 1.0  # from the wav duration
