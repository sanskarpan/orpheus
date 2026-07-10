import json
from unittest.mock import AsyncMock, MagicMock

import pytest

from orpheus_workers.config import WorkerSettings
from orpheus_workers.worker import Worker


def _msg(event: dict) -> MagicMock:
    msg = MagicMock()
    msg.data = json.dumps(event).encode()
    msg.ack = AsyncMock()
    msg.nak = AsyncMock()
    msg.term = AsyncMock()
    return msg


@pytest.mark.asyncio
async def test_on_message_acks_unknown_event_type() -> None:
    worker = Worker(WorkerSettings())
    msg = _msg({"event_type": "job.unknown"})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()
    msg.nak.assert_not_called()
    msg.term.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_acks_completed_event() -> None:
    worker = Worker(WorkerSettings())
    msg = _msg({"event_type": "job.completed", "payload": {"job_id": "x"}})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()
    msg.nak.assert_not_called()
    msg.term.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_acks_failed_event() -> None:
    worker = Worker(WorkerSettings())
    msg = _msg({"event_type": "job.failed", "payload": {"job_id": "x"}})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()
    msg.nak.assert_not_called()
    msg.term.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_terms_malformed_json() -> None:
    worker = Worker(WorkerSettings())
    msg = MagicMock()
    msg.data = b"not json"
    msg.ack = AsyncMock()
    msg.nak = AsyncMock()
    msg.term = AsyncMock()
    await worker._on_message(msg)
    msg.term.assert_awaited_once()
    msg.ack.assert_not_called()
    msg.nak.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_terms_missing_job_id() -> None:
    worker = Worker(WorkerSettings())
    msg = _msg({"event_type": "job.queued", "payload": {}})
    await worker._on_message(msg)
    msg.term.assert_awaited_once()
    msg.ack.assert_not_called()
    msg.nak.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_terms_job_not_found() -> None:
    db = MagicMock()
    db.fetchrow.return_value = None
    worker = Worker(WorkerSettings())
    worker._db = db
    msg = _msg({"event_type": "job.queued", "payload": {"job_id": "missing"}})
    await worker._on_message(msg)
    db.fetchrow.assert_called_once()
    msg.term.assert_awaited_once()
    msg.ack.assert_not_called()
    msg.nak.assert_not_called()
    db.mark_job_completed.assert_not_called()
    db.mark_job_failed.assert_not_called()
    db.enqueue_outbox.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_acks_when_no_processor_in_params() -> None:
    db = MagicMock()
    db.fetchrow.return_value = {"org_id": "org-1", "params": {}}
    worker = Worker(WorkerSettings())
    worker._db = db
    msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()
    msg.nak.assert_not_called()
    msg.term.assert_not_called()
    db.mark_job_completed.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_acks_when_processor_unknown() -> None:
    db = MagicMock()
    db.fetchrow.return_value = {
        "org_id": "org-1",
        "params": {"_processor": {"name": "does-not-exist"}},
    }
    worker = Worker(WorkerSettings())
    worker._db = db
    msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()
    db.mark_job_completed.assert_not_called()
    db.mark_job_failed.assert_not_called()
    db.enqueue_outbox.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_happy_path_marks_completed_and_acks() -> None:
    from orpheus_workers.processors import _REGISTRY

    async def fake_proc(ctx, job_id):  # type: ignore[no-untyped-def]
        return {"duration_seconds": 1.5, "tags": {"artist": ["x"]}}

    original = _REGISTRY.get("extract-metadata")
    _REGISTRY["extract-metadata"] = fake_proc
    try:
        db = MagicMock()
        db.fetchrow.return_value = {
            "org_id": "org-1",
            "params": {"_processor": {"name": "extract-metadata"}},
        }
        worker = Worker(WorkerSettings())
        worker._db = db
        msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
        await worker._on_message(msg)
    finally:
        if original is not None:
            _REGISTRY["extract-metadata"] = original
        else:
            _REGISTRY.pop("extract-metadata", None)

    msg.ack.assert_awaited_once()
    msg.nak.assert_not_called()
    msg.term.assert_not_called()
    db.mark_job_completed.assert_called_once()
    db.mark_job_failed.assert_not_called()
    db.enqueue_outbox.assert_called_once()
    kwargs = db.enqueue_outbox.call_args.kwargs
    assert kwargs["event_type"] == "job.completed"
    assert kwargs["aggregate_id"] == "j-1"
    assert kwargs["org_id"] == "org-1"


@pytest.mark.asyncio
async def test_on_message_processor_failure_marks_failed_and_naks() -> None:
    from orpheus_workers.processors import _REGISTRY

    async def boom(ctx, job_id):  # type: ignore[no-untyped-def]
        raise RuntimeError("mutagen exploded")

    original = _REGISTRY.get("extract-metadata")
    _REGISTRY["extract-metadata"] = boom
    try:
        db = MagicMock()
        db.fetchrow.return_value = {
            "org_id": "org-1",
            "params": {"_processor": {"name": "extract-metadata"}},
        }
        worker = Worker(WorkerSettings())
        worker._db = db
        msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
        await worker._on_message(msg)
    finally:
        if original is not None:
            _REGISTRY["extract-metadata"] = original
        else:
            _REGISTRY.pop("extract-metadata", None)

    msg.nak.assert_awaited_once()
    msg.ack.assert_not_called()
    msg.term.assert_not_called()
    db.mark_job_failed.assert_called_once_with("j-1", "mutagen exploded")
    db.enqueue_outbox.assert_called_once()
    kwargs = db.enqueue_outbox.call_args.kwargs
    assert kwargs["event_type"] == "job.failed"
    assert kwargs["payload"]["error"] == "mutagen exploded"
