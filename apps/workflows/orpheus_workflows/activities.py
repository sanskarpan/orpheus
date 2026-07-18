"""Production activity implementations for the transcribe-long workflow.

Activities are the ONLY place side effects live (DB, S3, ffmpeg, whisper).
They reuse the existing worker primitives (``orpheus_workers``) so there is a
single implementation of probing, slicing, transcription and persistence.

Registered by name (see ``transcribe_long.py``) so tests can substitute fakes.
In production, ``worker.py`` registers these against a Temporal task queue.
"""

from __future__ import annotations

import json
import uuid

from orpheus_workers.config import get_settings
from orpheus_workers.db import WorkerDB
from orpheus_workers.ffmpeg import convert_to_wav_16k_mono
from orpheus_workers.ffmpeg import slice as ffmpeg_slice
from orpheus_workers.s3 import WorkerS3
from orpheus_workers.transcribe import transcribe as run_whisper
from temporalio import activity

from .models import ChunkTranscript, CompensateInput, PersistInput, ProbeResult, StitchResult
from .transcribe_long import COMPENSATE, PERSIST, PROBE, STITCH, TRANSCRIBE_CHUNK

# Module-level singletons; activity workers are long-lived processes.
_settings = get_settings()
_db: WorkerDB | None = None
_s3: WorkerS3 | None = None


def _lazy() -> tuple[WorkerDB, WorkerS3]:
    global _db, _s3
    if _db is None:
        _db = WorkerDB(_settings)
        _db.open()
    if _s3 is None:
        _s3 = WorkerS3(_settings)
    return _db, _s3


def _fetch_artifact(db: WorkerDB, artifact_id: str) -> dict:
    row = db.fetchrow(
        "SELECT s3_bucket, s3_key, duration_seconds, codec FROM artifacts WHERE id = %s",
        artifact_id,
    )
    if row is None:
        raise ValueError(f"artifact {artifact_id} not found")
    return row


@activity.defn(name=PROBE)
async def probe(artifact_id: str) -> ProbeResult:
    db, _ = _lazy()
    row = _fetch_artifact(db, artifact_id)
    dur = float(row["duration_seconds"]) if row["duration_seconds"] is not None else 0.0
    return ProbeResult(duration_seconds=dur, codec=row["codec"])


@activity.defn(name=TRANSCRIBE_CHUNK)
async def transcribe_chunk(
    artifact_id: str, start: float, end: float, index: int
) -> ChunkTranscript:
    import contextlib
    import os
    from pathlib import Path

    db, s3 = _lazy()
    row = _fetch_artifact(db, artifact_id)
    work = Path(_settings.work_dir)
    work.mkdir(parents=True, exist_ok=True)
    suffix = Path(row["s3_key"]).suffix or ".bin"
    src = work / f"wf-{artifact_id}-{index}.src{suffix}"
    sliced = work / f"wf-{artifact_id}-{index}.slice.wav"
    try:
        s3.download_file(row["s3_bucket"], row["s3_key"], str(src))
        # -c copy would need a container-aware cut; convert to wav then slice.
        convert_to_wav_16k_mono(src, sliced)
        if end > start:
            cut = work / f"wf-{artifact_id}-{index}.cut.wav"
            ffmpeg_slice(sliced, cut, start, end)
            sliced = cut
        out = run_whisper(str(sliced))
        if isinstance(out, dict):
            text = out.get("text", "")
            raw_segments = out.get("segments", []) or []
        else:
            text, raw_segments = str(out), []
        # Shift chunk-local timestamps onto the full recording's timeline.
        segments = [
            {
                "start": float(s.get("start", 0.0)) + start,
                "end": float(s.get("end", 0.0)) + start,
                "text": s.get("text", ""),
            }
            for s in raw_segments
        ]
        return ChunkTranscript(
            index=index, start_seconds=start, text=text, segments=segments, artifact_id=None
        )
    finally:
        for p in (src, sliced):
            with contextlib.suppress(FileNotFoundError):
                os.unlink(p)


@activity.defn(name=STITCH)
async def stitch(chunks: list[ChunkTranscript]) -> StitchResult:
    """Merge ordered chunk transcripts into one transcript with a continuous
    segment timeline (not just concatenated text)."""
    ordered = sorted(chunks, key=lambda c: c.index)
    segments: list[dict] = []
    for c in ordered:
        segments.extend(c.segments or [])
    segments.sort(key=lambda s: (s.get("start", 0.0), s.get("end", 0.0)))
    text = " ".join(c.text.strip() for c in ordered if c.text and c.text.strip())
    return StitchResult(text=text, segments=segments)


@activity.defn(name=PERSIST)
async def persist(inp: PersistInput) -> str:
    db, _ = _lazy()
    result_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"transcribe-long:{inp.workflow_id}"))
    # Persist the transcript onto the workflow row (idempotent by workflow_id).
    db.execute(
        """
        UPDATE workflows
        SET status = 'completed', result = %s, updated_at = now()
        WHERE id = %s
        """,
        json.dumps(
            {
                "text": inp.text,
                "segments": inp.segments,
                "chunk_count": inp.chunk_count,
                "result_artifact_id": result_id,
            }
        ),
        inp.workflow_id,
    )
    return result_id


@activity.defn(name=COMPENSATE)
async def compensate(inp: CompensateInput) -> None:
    # Best-effort cleanup of intermediate artifacts on cancel/failure.
    db, _ = _lazy()
    for aid in inp.artifact_ids:
        try:
            db.execute("DELETE FROM artifacts WHERE id = %s AND org_id = %s", aid, inp.org_id)
        except Exception:
            activity.logger.warning("compensate.delete_failed", extra={"artifact_id": aid})


ALL_ACTIVITIES = [probe, transcribe_chunk, stitch, persist, compensate]
