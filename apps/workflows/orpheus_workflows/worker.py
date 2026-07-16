"""Temporal worker entrypoint for Orpheus workflows.

Connects to a Temporal server (ORPHEUS_TEMPORAL_ADDRESS, default
localhost:7233) and serves the TranscribeLongWorkflow + its activities on the
``orpheus-transcribe`` task queue. The Go API starts workflows on this queue
via a Temporal client (see docs/design/09-temporal-workflows.md).

Run: ``uv run --package orpheus-workflows python -m orpheus_workflows.worker``
"""

from __future__ import annotations

import asyncio
import os

import structlog
from temporalio.client import Client
from temporalio.worker import Worker

from .activities import ALL_ACTIVITIES
from .transcribe_long import TranscribeLongWorkflow

logger = structlog.get_logger(__name__)

TASK_QUEUE = "orpheus-transcribe"


async def main() -> None:
    address = os.environ.get("ORPHEUS_TEMPORAL_ADDRESS", "localhost:7233")
    namespace = os.environ.get("ORPHEUS_TEMPORAL_NAMESPACE", "default")
    client = await Client.connect(address, namespace=namespace)
    logger.info("workflows.worker.connected", address=address, namespace=namespace)
    async with Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[TranscribeLongWorkflow],
        activities=ALL_ACTIVITIES,
    ):
        logger.info("workflows.worker.started", task_queue=TASK_QUEUE)
        # Run until cancelled.
        await asyncio.Event().wait()


if __name__ == "__main__":
    asyncio.run(main())
