"""Tests for in-code processor manifests (Phase 2)."""

from __future__ import annotations

import orpheus_workers.worker  # noqa: F401 — imports all processors (registers them)
from orpheus_workers.processors import (
    get_manifest,
    list_manifests,
    list_processors,
    register_processor,
)

_VALID_TIERS = {"cpu_tiny", "cpu_small", "cpu_medium", "cpu_large", "gpu_a10g", "gpu_a100"}


def test_every_processor_has_a_manifest() -> None:
    procs = list_processors()
    assert procs, "no processors registered"
    for name in procs:
        m = get_manifest(name)
        assert m is not None, f"{name} has no manifest"
        assert m.name == name
        assert m.tier in _VALID_TIERS, f"{name} tier {m.tier!r} not a processor_tier"
        assert m.timeout_seconds > 0
        assert m.version


def test_list_manifests_sorted_and_complete() -> None:
    names = [m.name for m in list_manifests()]
    assert names == sorted(names)
    assert set(names) == set(list_processors())


def test_known_manifest_values() -> None:
    conv = get_manifest("convert-to-wav")
    assert conv is not None
    assert conv.display_name == "Convert to WAV"
    assert conv.tier == "cpu_tiny"
    assert conv.timeout_seconds == 180
    assert conv.model_id == "ffmpeg"

    summ = get_manifest("text.summarize")
    assert summ is not None
    assert summ.cacheable is False  # summaries are not cache-reusable


def test_register_processor_defaults() -> None:
    @register_processor("test.dummy-manifest")
    async def _dummy(ctx, job_id):  # pragma: no cover - never invoked
        return {}

    m = get_manifest("test.dummy-manifest")
    assert m is not None
    assert m.display_name == "test.dummy-manifest"  # defaults to name
    assert m.tier == "cpu_tiny"
    assert m.max_retries == 3
    assert m.cacheable is True
