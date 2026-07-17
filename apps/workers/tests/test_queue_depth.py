"""Tests for the JetStream queue-depth gauge (Phase 2)."""

from __future__ import annotations

from prometheus_client import REGISTRY

from orpheus_workers.worker import record_queue_depth


class _Info:
    def __init__(self, num_pending: int) -> None:
        self.num_pending = num_pending


class _FakeJS:
    def __init__(self, pending: int | None = None, raises: Exception | None = None) -> None:
        self._pending = pending
        self._raises = raises
        self.calls: list[tuple[str, str]] = []

    async def consumer_info(self, stream: str, consumer: str):
        self.calls.append((stream, consumer))
        if self._raises is not None:
            raise self._raises
        return _Info(self._pending or 0)


def _gauge(stream: str, consumer: str) -> float | None:
    return REGISTRY.get_sample_value(
        "orpheus_jetstream_pending_messages",
        {"stream": stream, "consumer": consumer},
    )


async def test_record_queue_depth_sets_gauge() -> None:
    js = _FakeJS(pending=7)
    got = await record_queue_depth(js, "S1", "C1")
    assert got == 7
    assert js.calls == [("S1", "C1")]
    assert _gauge("S1", "C1") == 7.0


async def test_record_queue_depth_updates_on_repeat() -> None:
    js = _FakeJS(pending=3)
    await record_queue_depth(js, "S2", "C2")
    assert _gauge("S2", "C2") == 3.0
    js._pending = 0
    await record_queue_depth(js, "S2", "C2")
    assert _gauge("S2", "C2") == 0.0


async def test_record_queue_depth_swallows_errors() -> None:
    js = _FakeJS(raises=RuntimeError("consumer not found"))
    got = await record_queue_depth(js, "S3", "C3")
    assert got is None  # never raises; observability must not crash the worker
    assert _gauge("S3", "C3") is None
