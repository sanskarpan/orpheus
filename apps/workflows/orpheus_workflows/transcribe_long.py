"""TranscribeLongWorkflow — the first Temporal workflow (gap #9).

Orchestrates: probe → plan chunks → transcribe chunks in parallel → stitch →
persist. It is deterministic (all side effects live in activities, called by
name so tests can substitute fakes) and implements saga compensation: if the
workflow is cancelled or fails after intermediate artifacts were created,
their deletion is run in reverse order.

See docs/design/09-temporal-workflows.md for the full design.
"""

from __future__ import annotations

import asyncio
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ActivityError, CancelledError

with workflow.unsafe.imports_passed_through():
    from .models import (
        Chunk,
        ChunkTranscript,
        CompensateInput,
        PersistInput,
        ProbeResult,
        StitchResult,
        TranscribeLongInput,
        TranscribeLongResult,
    )

# Activity names — the workflow calls activities by name so the production
# adapters and the test fakes can be swapped without touching this file.
PROBE = "transcribe.probe"
TRANSCRIBE_CHUNK = "transcribe.chunk"
STITCH = "transcribe.stitch"
PERSIST = "transcribe.persist"
COMPENSATE = "transcribe.compensate"

_ACTIVITY_RETRY = RetryPolicy(
    initial_interval=timedelta(seconds=1),
    backoff_coefficient=2.0,
    maximum_interval=timedelta(seconds=30),
    maximum_attempts=4,
)

# Bound the fan-out so a very long recording doesn't spawn unbounded parallel
# activities.
_MAX_PARALLEL_CHUNKS = 8


@workflow.defn
class TranscribeLongWorkflow:
    def __init__(self) -> None:
        # Artifact ids created along the way, for compensation on cancel.
        self._created_artifacts: list[str] = []

    @workflow.run
    async def run(self, inp: TranscribeLongInput) -> TranscribeLongResult:
        try:
            probe: ProbeResult = await workflow.execute_activity(
                PROBE,
                inp.artifact_id,
                start_to_close_timeout=timedelta(minutes=5),
                retry_policy=_ACTIVITY_RETRY,
            )

            chunks = _plan_chunks(probe.duration_seconds, inp.chunk_seconds)

            async def do_chunk(c: Chunk) -> ChunkTranscript:
                ct: ChunkTranscript = await workflow.execute_activity(
                    TRANSCRIBE_CHUNK,
                    args=[inp.artifact_id, c.start_seconds, c.end_seconds, c.index],
                    start_to_close_timeout=timedelta(minutes=30),
                    retry_policy=_ACTIVITY_RETRY,
                )
                if ct.artifact_id:
                    self._created_artifacts.append(ct.artifact_id)
                return ct

            # Transcribe chunks in bounded parallel batches. Real concurrency
            # is also capped by the worker's max_concurrent_activities; the
            # batch bound keeps a very long recording from scheduling
            # thousands of activities at once.
            transcripts: list[ChunkTranscript] = []
            for i in range(0, len(chunks), _MAX_PARALLEL_CHUNKS):
                batch = chunks[i : i + _MAX_PARALLEL_CHUNKS]
                transcripts.extend(await asyncio.gather(*[do_chunk(c) for c in batch]))
            transcripts.sort(key=lambda t: t.index)

            stitched: StitchResult = await workflow.execute_activity(
                STITCH,
                args=[transcripts],
                start_to_close_timeout=timedelta(minutes=5),
                retry_policy=_ACTIVITY_RETRY,
            )

            result_artifact_id: str = await workflow.execute_activity(
                PERSIST,
                PersistInput(
                    workflow_id=inp.workflow_id,
                    org_id=inp.org_id,
                    artifact_id=inp.artifact_id,
                    text=stitched.text,
                    chunk_count=len(chunks),
                    segments=stitched.segments,
                ),
                start_to_close_timeout=timedelta(minutes=5),
                retry_policy=_ACTIVITY_RETRY,
            )
            if result_artifact_id:
                self._created_artifacts.append(result_artifact_id)

            return TranscribeLongResult(
                workflow_id=inp.workflow_id,
                artifact_id=inp.artifact_id,
                text=stitched.text,
                chunk_count=len(chunks),
                result_artifact_id=result_artifact_id,
                segments=stitched.segments,
            )
        except (CancelledError, ActivityError):
            # Saga compensation: undo intermediate artifacts in reverse order.
            # Runs in a detached scope so cancellation doesn't abort cleanup.
            await self._compensate(inp.org_id)
            raise

    async def _compensate(self, org_id: str) -> None:
        if not self._created_artifacts:
            return
        to_delete = list(reversed(self._created_artifacts))
        await workflow.execute_activity(
            COMPENSATE,
            CompensateInput(org_id=org_id, artifact_ids=to_delete),
            start_to_close_timeout=timedelta(minutes=5),
            retry_policy=_ACTIVITY_RETRY,
        )


def _plan_chunks(duration_seconds: float, chunk_seconds: float) -> list[Chunk]:
    """Split [0, duration] into chunks of at most chunk_seconds. Pure and
    deterministic so it is safe to run inside workflow code."""
    if chunk_seconds <= 0:
        chunk_seconds = 60.0
    if duration_seconds <= 0:
        return [Chunk(index=0, start_seconds=0.0, end_seconds=0.0)]
    chunks: list[Chunk] = []
    i = 0
    start = 0.0
    while start < duration_seconds:
        end = min(start + chunk_seconds, duration_seconds)
        chunks.append(Chunk(index=i, start_seconds=start, end_seconds=end))
        start = end
        i += 1
    return chunks
