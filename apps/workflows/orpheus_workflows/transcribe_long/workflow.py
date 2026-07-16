"""SKELETON — NON-FUNCTIONAL. Do not wire this into any deployment.

Temporal workflow definition for ``transcribe-long`` (gap #9).

This file exists to pin the *interface* and the *determinism contract* for the
Temporal migration described in ``docs/design/09-temporal-workflows.md``. It
deliberately does not run:

  * ``temporalio`` is NOT a dependency of this workspace yet.
  * Every step raises ``NotImplementedError``.
  * The ``@workflow.defn`` / ``@activity.defn`` decorators below are provided by
    a local stub (see ``_temporal_stub``) so this module can be *read* and
    *imported for reference* without the real SDK — but it must NOT be
    registered with a worker.

DETERMINISM CONTRACT (enforced once real): workflow code may use only
``workflow.now()``, ``workflow.random()``, ``workflow.get_version()`` and
activity calls. No wall clock, no ``os``, no direct network, no direct S3/DB.
All side effects live in activities (``orpheus_workflows.activities``), which
are thin adapters over the existing processors in
``orpheus_workers.processors``.
"""

from __future__ import annotations

from dataclasses import dataclass, field

# ---------------------------------------------------------------------------
# Local stub so this file can be imported for reference WITHOUT temporalio.
# When the real SDK lands, delete this block and use:
#     from temporalio import workflow
# ---------------------------------------------------------------------------
try:  # pragma: no cover - reference-only import shim
    from temporalio import workflow  # type: ignore
except ImportError:  # pragma: no cover

    class _WorkflowStub:
        """Minimal stand-in so decorators resolve when temporalio is absent.

        This is a SCAFFOLD ONLY. It provides no durability, no determinism
        guarantees, and no execution. Never register a workflow that uses it.
        """

        @staticmethod
        def defn(cls=None, **_kwargs):
            def wrap(c):
                c.__orpheus_scaffold__ = True
                return c

            return wrap(cls) if cls is not None else wrap

        @staticmethod
        def run(fn):
            fn.__orpheus_workflow_run__ = True
            return fn

        @staticmethod
        def signal(fn=None, **_kwargs):
            def wrap(f):
                f.__orpheus_workflow_signal__ = True
                return f

            return wrap(fn) if fn is not None else wrap

    workflow = _WorkflowStub()  # type: ignore


# ---------------------------------------------------------------------------
# Typed workflow I/O (these are real and stable — the SDK serializes them).
# ---------------------------------------------------------------------------
@dataclass
class TranscribeLongInput:
    workflow_id: str  # == workflows.id (also derives the Temporal ID)
    org_id: str
    artifact_key: str  # S3 key of the source audio
    model_version_id: str  # pinned model (ADR-0005 reproducibility)
    params: dict = field(default_factory=dict)
    chunk_seconds: int = 600  # target chunk length before fan-out
    max_parallel_chunks: int = 4  # per-tenant GPU bulkhead


@dataclass
class TranscribeLongResult:
    text: str
    segments: list  # [{start, end, text, ...}] with global offsets applied
    language: str
    duration_seconds: float


# ---------------------------------------------------------------------------
# Workflow definition — CONTROL FLOW ONLY. Every step is a TODO.
# ---------------------------------------------------------------------------
@workflow.defn(name="TranscribeLongWorkflow")
class TranscribeLongWorkflow:
    def __init__(self) -> None:
        # Reverse-order compensation stack: list of (activity_name, args).
        self._compensations: list[tuple[str, object]] = []
        self._cancel_requested: bool = False
        self._cancel_reason: str | None = None

    @workflow.run
    async def run(self, wf_input: TranscribeLongInput) -> TranscribeLongResult:
        # STEP 1 — probe (CPU activity) → decide chunking.
        # STEP 2 — slice into deterministic S3 keys; push compensation.
        # STEP 3 — bounded fan-out transcribe_chunk on the 'gpu-transcribe'
        #          task queue (honor max_parallel_chunks).
        # STEP 4 — stitch with global offsets (deterministic).
        # STEP 5 — persist_result (CAS) + emit 'workflow.completed' outbox event.
        # On cancel at any point: run self._compensations in REVERSE order.
        #
        # See docs/design/09-temporal-workflows.md §2.1-2.3 for the full flow.
        raise NotImplementedError(
            "SCAFFOLD: TranscribeLongWorkflow.run is not implemented. "
            "See docs/design/09-temporal-workflows.md build checklist."
        )

    @workflow.signal(name="cancel")
    async def cancel(self, reason: str = "user_requested") -> None:
        # Durable signal handler. Records intent; the run loop observes it,
        # cancels in-flight activities, and runs compensations in reverse.
        self._cancel_requested = True
        self._cancel_reason = reason
        raise NotImplementedError(
            "SCAFFOLD: cancel signal handling is not implemented. "
            "See docs/design/09-temporal-workflows.md §2.2."
        )


# ---------------------------------------------------------------------------
# Pure helper — MUST stay deterministic (safe to call from workflow code).
# Real implementation + unit tests go in transcribe_long/planning.py.
# ---------------------------------------------------------------------------
def plan_chunks(duration_seconds: float, target_seconds: int) -> list[tuple[float, float]]:
    """Return [(start, end), ...] chunk boundaries. Pure, deterministic.

    TODO: implement in planning.py with overlap handling for word boundaries.
    """
    raise NotImplementedError(
        "SCAFFOLD: plan_chunks belongs in transcribe_long/planning.py with tests."
    )
