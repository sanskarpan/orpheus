import pytest

from orpheus_workers.worker import noop_job


@pytest.mark.asyncio
async def test_noop_job_returns_completed() -> None:
    result = await noop_job(ctx={}, job_id="test-1")
    assert result == {"job_id": "test-1", "status": "completed"}
