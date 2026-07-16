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


def _job_db(*, status: str = "queued", params: dict | None = None, running: int = 0, claim: bool = True, retry_state: tuple[int, int] = (0, 3)) -> MagicMock:
    db = MagicMock()
    db.fetchrow.return_value = {"org_id": "org-1", "params": params or {}, "status": status}
    db.running_jobs_for_org.return_value = running
    db.claim_job.return_value = claim
    db.job_retry_state.return_value = retry_state
    return db


@pytest.mark.asyncio
async def test_on_message_acks_when_no_processor_in_params() -> None:
    db = _job_db(params={})
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
    db = _job_db(params={"_processor": {"name": "does-not-exist"}})
    worker = Worker(WorkerSettings())
    worker._db = db
    msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()
    db.mark_job_completed.assert_not_called()
    db.mark_job_failed.assert_not_called()
    db.enqueue_outbox.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_defers_when_org_at_capacity() -> None:
    db = _job_db(params={"_processor": {"name": "extract-metadata"}}, running=100)
    worker = Worker(WorkerSettings())
    worker._db = db
    msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
    await worker._on_message(msg)
    msg.nak.assert_awaited_once()  # deferred, not processed
    db.claim_job.assert_not_called()
    db.mark_job_completed.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_skips_terminal_job() -> None:
    db = _job_db(status="dead_letter", params={"_processor": {"name": "extract-metadata"}})
    worker = Worker(WorkerSettings())
    worker._db = db
    msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
    await worker._on_message(msg)
    msg.ack.assert_awaited_once()  # terminal → ack, don't reprocess
    db.claim_job.assert_not_called()


@pytest.mark.asyncio
async def test_on_message_happy_path_marks_completed_and_acks() -> None:
    from orpheus_workers.processors import _REGISTRY

    async def fake_proc(ctx, job_id):  # type: ignore[no-untyped-def]
        return {"duration_seconds": 1.5, "tags": {"artist": ["x"]}}

    original = _REGISTRY.get("extract-metadata")
    _REGISTRY["extract-metadata"] = fake_proc
    try:
        db = _job_db(params={"_processor": {"name": "extract-metadata"}})
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
    db.claim_job.assert_called_once_with("j-1")
    db.mark_job_completed.assert_called_once()
    # cost_usd is attributed from processing wall-time.
    assert "cost_usd" in db.mark_job_completed.call_args.kwargs
    db.mark_job_failed.assert_not_called()
    kwargs = db.enqueue_outbox.call_args.kwargs
    assert kwargs["event_type"] == "job.completed"
    assert kwargs["aggregate_id"] == "j-1"


@pytest.mark.asyncio
async def test_on_message_failure_retries_when_attempts_remain() -> None:
    from orpheus_workers.processors import _REGISTRY

    async def boom(ctx, job_id):  # type: ignore[no-untyped-def]
        raise RuntimeError("mutagen exploded")

    original = _REGISTRY.get("extract-metadata")
    _REGISTRY["extract-metadata"] = boom
    try:
        db = _job_db(params={"_processor": {"name": "extract-metadata"}}, retry_state=(1, 3))
        worker = Worker(WorkerSettings())
        worker._db = db
        msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
        await worker._on_message(msg)
    finally:
        if original is not None:
            _REGISTRY["extract-metadata"] = original
        else:
            _REGISTRY.pop("extract-metadata", None)

    msg.nak.assert_awaited_once()  # redeliver for retry
    msg.term.assert_not_called()
    db.requeue_job_for_retry.assert_called_once_with("j-1")
    db.mark_job_dead_letter.assert_not_called()
    assert db.enqueue_outbox.call_args.kwargs["event_type"] == "job.retry"


@pytest.mark.asyncio
async def test_on_message_failure_dead_letters_when_exhausted() -> None:
    from orpheus_workers.processors import _REGISTRY

    async def boom(ctx, job_id):  # type: ignore[no-untyped-def]
        raise RuntimeError("mutagen exploded")

    original = _REGISTRY.get("extract-metadata")
    _REGISTRY["extract-metadata"] = boom
    try:
        db = _job_db(params={"_processor": {"name": "extract-metadata"}}, retry_state=(3, 3))
        worker = Worker(WorkerSettings())
        worker._db = db
        msg = _msg({"event_type": "job.queued", "payload": {"job_id": "j-1"}})
        await worker._on_message(msg)
    finally:
        if original is not None:
            _REGISTRY["extract-metadata"] = original
        else:
            _REGISTRY.pop("extract-metadata", None)

    msg.term.assert_awaited_once()  # stop redelivery
    msg.nak.assert_not_called()
    db.mark_job_dead_letter.assert_called_once_with("j-1", "mutagen exploded")
    db.requeue_job_for_retry.assert_not_called()
    assert db.enqueue_outbox.call_args.kwargs["event_type"] == "job.dead_letter"
