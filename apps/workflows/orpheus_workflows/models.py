"""Serializable inputs/outputs for the transcribe-long workflow.

Temporal serializes these across the workflow/activity boundary, so they are
plain dataclasses (JSON-friendly) with no behaviour.
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class TranscribeLongInput:
    workflow_id: str
    org_id: str
    artifact_id: str
    chunk_seconds: float = 60.0


@dataclass
class ProbeResult:
    duration_seconds: float
    codec: str | None = None


@dataclass
class Chunk:
    index: int
    start_seconds: float
    end_seconds: float


@dataclass
class ChunkTranscript:
    index: int
    start_seconds: float
    text: str
    # id of any intermediate artifact created for this chunk, so it can be
    # compensated (deleted) on cancellation.
    artifact_id: str | None = None


@dataclass
class TranscribeLongResult:
    workflow_id: str
    artifact_id: str
    text: str
    chunk_count: int
    result_artifact_id: str | None = None


@dataclass
class PersistInput:
    workflow_id: str
    org_id: str
    artifact_id: str
    text: str
    chunk_count: int


@dataclass
class CompensateInput:
    org_id: str
    artifact_ids: list[str] = field(default_factory=list)
