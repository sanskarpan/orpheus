"""Forced-alignment acceptance test (#298).

Runs the real MMS_FA forced-alignment path (Modal backend) on a synthesized
speech clip and asserts the returned word boundaries are structurally valid:
present, count-matching, monotonic, non-overlapping, and within the audio
duration. This is a regression/acceptance guard for the alignment seam — not a
labeled-corpus accuracy benchmark, which needs ground-truth word timings.

Gated: requires ORPHEUS_ALIGN_ACCEPT=1, the Modal align env
(ORPHEUS_MODAL_ALIGN_URL / ORPHEUS_MODAL_ALIGN_TOKEN) and espeak-ng, so hermetic
CI is unaffected. Run locally with the deployment env sourced:

    ORPHEUS_ALIGN_ACCEPT=1 uv run --package orpheus-workers \
        python -m pytest apps/workers/tests/test_align_acceptance.py -v
"""

from __future__ import annotations

import os
import shutil
import subprocess
import wave

import pytest

from orpheus_workers.align import align_transcript


def _wav_duration(path: str) -> float:
    with wave.open(path) as w:
        return w.getnframes() / float(w.getframerate())


@pytest.mark.skipif(
    os.getenv("ORPHEUS_ALIGN_ACCEPT") != "1"
    or not os.getenv("ORPHEUS_MODAL_ALIGN_URL")
    or not os.getenv("ORPHEUS_MODAL_ALIGN_TOKEN")
    or shutil.which("espeak-ng") is None,
    reason="set ORPHEUS_ALIGN_ACCEPT=1 + Modal align env; needs espeak-ng",
)
def test_forced_alignment_produces_valid_word_boundaries(tmp_path):
    phrase = "the quick brown fox jumps over the lazy dog"
    wav = tmp_path / "clip.wav"
    subprocess.run(["espeak-ng", "-w", str(wav), phrase], check=True)
    duration = _wav_duration(str(wav))

    transcript = {
        "text": phrase,
        "language": "en",
        "segments": [{"start": 0.0, "end": duration, "text": phrase, "words": []}],
    }
    align_transcript(str(wav), transcript, language="en")

    # The seam marks the transcript forced-aligned and replaces the words.
    assert transcript.get("alignment") == "forced"
    words = transcript["segments"][0]["words"]
    assert len(words) == len(phrase.split()), f"got {len(words)} words, want {len(phrase.split())}"

    eps = 0.25  # small tolerance at the boundaries
    prev_end = 0.0
    for w in words:
        assert 0.0 <= w["start"] <= w["end"] <= duration + eps, f"bad word timing: {w}"
        assert w["start"] >= prev_end - eps, f"non-monotonic word after {prev_end}: {w}"
        prev_end = w["end"]

    # Alignment should span a meaningful fraction of the clip (not collapse to 0).
    assert words[-1]["end"] >= duration * 0.5
