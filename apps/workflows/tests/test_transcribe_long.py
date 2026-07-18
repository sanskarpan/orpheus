"""End-to-end tests for TranscribeLongWorkflow.

These run the workflow against Temporal's embedded time-skipping test server
with FAKE activities registered under the same names the workflow calls, so we
exercise the real orchestration (parallel fan-out, retry policy, saga
compensation) deterministically and fast — no Postgres/S3/whisper needed.
"""

from __future__ import annotations

import uuid

import pytest
from orpheus_workflows.models import (
    ChunkTranscript,
    CompensateInput,
    PersistInput,
    StitchResult,
    TranscribeLongInput,
)
from orpheus_workflows.transcribe_long import (
    COMPENSATE,
    PERSIST,
    PROBE,
    STITCH,
    TRANSCRIBE_CHUNK,
    TranscribeLongWorkflow,
    _plan_chunks,
)
from temporalio import activity
from temporalio.client import WorkflowFailureError
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

pytestmark = pytest.mark.asyncio


# Shared mutable state the fake activities record into (activities run outside
# the workflow sandbox, so globals are fine here).
class Recorder:
    def __init__(self) -> None:
        self.chunk_calls: dict[int, int] = {}
        self.compensated: list[str] = []
        self.persist_should_fail = False
        self.chunk_fail_times = 0


def _fakes(rec: Recorder):
    @activity.defn(name=PROBE)
    async def probe(artifact_id: str):
        from orpheus_workflows.models import ProbeResult

        return ProbeResult(duration_seconds=185.0, codec="pcm_s16le")

    @activity.defn(name=TRANSCRIBE_CHUNK)
    async def transcribe_chunk(artifact_id: str, start: float, end: float, index: int):
        rec.chunk_calls[index] = rec.chunk_calls.get(index, 0) + 1
        # Chunk 0 fails the first N times to exercise the retry policy.
        if index == 0 and rec.chunk_calls[index] <= rec.chunk_fail_times:
            raise ApplicationError("transient chunk failure", non_retryable=False)
        # One segment per chunk, timestamped on the absolute timeline.
        return ChunkTranscript(
            index=index,
            start_seconds=start,
            text=f"[{index}]",
            segments=[{"start": start, "end": end, "text": f"[{index}]"}],
            artifact_id=f"chunk-{index}",
        )

    @activity.defn(name=STITCH)
    async def stitch(chunks: list[ChunkTranscript]):
        ordered = sorted(chunks, key=lambda c: c.index)
        segments: list[dict] = []
        for c in ordered:
            segments.extend(c.segments or [])
        return StitchResult(text=" ".join(c.text for c in ordered), segments=segments)

    @activity.defn(name=PERSIST)
    async def persist(inp: PersistInput):
        if rec.persist_should_fail:
            raise ApplicationError("persist blew up", non_retryable=True)
        return f"result-{inp.workflow_id}"

    @activity.defn(name=COMPENSATE)
    async def compensate(inp: CompensateInput):
        rec.compensated.extend(inp.artifact_ids)

    return [probe, transcribe_chunk, stitch, persist, compensate]


async def _run(env: WorkflowEnvironment, rec: Recorder, inp: TranscribeLongInput):
    tq = f"tq-{uuid.uuid4()}"
    async with Worker(
        env.client,
        task_queue=tq,
        workflows=[TranscribeLongWorkflow],
        activities=_fakes(rec),
    ):
        return await env.client.execute_workflow(
            TranscribeLongWorkflow.run, inp, id=f"wf-{uuid.uuid4()}", task_queue=tq
        )


async def test_plan_chunks_pure():
    chunks = _plan_chunks(185.0, 60.0)
    assert [(c.start_seconds, c.end_seconds) for c in chunks] == [
        (0.0, 60.0),
        (60.0, 120.0),
        (120.0, 180.0),
        (180.0, 185.0),
    ]


async def test_transcribe_long_workflow_e2e():
    """All server-backed scenarios share ONE embedded environment (the
    server startup is the slow part) — happy path, retry-then-succeed, and
    saga compensation on failure."""
    async with await WorkflowEnvironment.start_time_skipping() as env:
        # 1) Happy path — 185s / 60s → 4 chunks, stitched in order.
        rec = Recorder()
        res = await _run(
            env,
            rec,
            TranscribeLongInput(workflow_id="w1", org_id="o1", artifact_id="a1", chunk_seconds=60),
        )
        assert res.chunk_count == 4
        assert res.text == "[0] [1] [2] [3]"
        assert res.result_artifact_id == "result-w1"
        # Segments flow through stitch onto a continuous absolute timeline.
        assert [(s["start"], s["end"]) for s in res.segments] == [
            (0.0, 60.0),
            (60.0, 120.0),
            (120.0, 180.0),
            (180.0, 185.0),
        ]

        # 2) Retry — chunk 0 fails twice then succeeds; the workflow still
        #    completes because the activity retry policy re-runs it.
        rec = Recorder()
        rec.chunk_fail_times = 2
        res = await _run(
            env,
            rec,
            TranscribeLongInput(workflow_id="w2", org_id="o1", artifact_id="a1", chunk_seconds=60),
        )
        assert res.chunk_count == 4
        assert rec.chunk_calls[0] == 3  # 2 failures + 1 success

        # 3) Saga compensation — persist fails AFTER chunk artifacts exist;
        #    the workflow fails and the chunk artifacts are deleted in reverse.
        rec = Recorder()
        rec.persist_should_fail = True
        with pytest.raises(WorkflowFailureError):
            await _run(
                env,
                rec,
                TranscribeLongInput(
                    workflow_id="w3", org_id="o1", artifact_id="a1", chunk_seconds=60
                ),
            )
        assert rec.compensated == ["chunk-3", "chunk-2", "chunk-1", "chunk-0"]
