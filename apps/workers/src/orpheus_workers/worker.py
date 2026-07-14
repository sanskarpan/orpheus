from __future__ import annotations

import asyncio
import json
import signal
import time
from typing import Any

import nats
import nats.js
import structlog
from nats.aio.client import Client as NATS
from nats.js import JetStreamContext
from nats.js.errors import NotFoundError
from opentelemetry import trace
from opentelemetry.propagate import extract
from opentelemetry.trace import Status, StatusCode
from prometheus_client import start_http_server

from . import metrics
from .config import WorkerSettings, get_settings
from .db import WorkerDB
from .observability.tracing import init as init_tracing
from .processors import get_processor
from .processors import extract_metadata, probe, slice, transcribe  # noqa: F401  (registers handlers)
from .s3 import WorkerS3

logger = structlog.get_logger(__name__)

JOB_STREAM = "ORPHEUS_JOBS"
JOB_SUBJECTS = "adkil.job.>"
JOB_DURABLE = "orpheus-workers"

tracer = trace.get_tracer("orpheus-workers")


class Worker:
    def __init__(self, settings: WorkerSettings) -> None:
        init_tracing("orpheus-workers")
        self._settings = settings
        self._db: WorkerDB | None = None
        self._s3: WorkerS3 | None = None
        self._nc: NATS | None = None
        self._js: JetStreamContext | None = None
        self._sub: JetStreamContext.PushSubscription | None = None

    async def start(self) -> None:
        settings = self._settings
        self._db = WorkerDB(settings)
        self._db.open()
        self._s3 = WorkerS3(settings)
        self._nc = await nats.connect(settings.nats_url)
        self._js = self._nc.jetstream()
        try:
            await self._js.stream_info(JOB_STREAM)
        except NotFoundError:
            await self._js.add_stream(name=JOB_STREAM, subjects=[JOB_SUBJECTS])
        self._sub = await self._js.subscribe(
            JOB_SUBJECTS,
            cb=self._on_message,
            durable=JOB_DURABLE,
        )
        start_http_server(settings.metrics_port)
        logger.info("worker.metrics_started", port=settings.metrics_port)
        logger.info("worker.started", nats_url=settings.nats_url)

    async def stop(self) -> None:
        if self._sub is not None:
            await self._sub.unsubscribe()
            self._sub = None
        if self._nc is not None:
            await self._nc.drain()
            self._nc = None
        if self._db is not None:
            self._db.close()
            self._db = None
        logger.info("worker.stopped")

    async def _on_message(self, msg: Any) -> None:
        try:
            event = json.loads(msg.data.decode())
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            logger.error("worker.bad_message", err=str(exc))
            await msg.term()
            metrics.JETSTREAM_MESSAGES.labels(result="parse_error").inc()
            return
        event_type = event.get("event_type")
        if event_type in ("job.completed", "job.failed"):
            await msg.ack()
            metrics.JETSTREAM_MESSAGES.labels(result="ack").inc()
            return
        if event_type != "job.queued":
            logger.warning("worker.unknown_event_type", event_type=event_type)
            await msg.ack()
            metrics.JETSTREAM_MESSAGES.labels(result="ack").inc()
            return
        await self._handle_job_queued(event, msg)

    async def _handle_job_queued(self, event: dict[str, Any], msg: Any) -> None:
        job_id = (event.get("payload") or {}).get("job_id")
        if not job_id:
            logger.error("worker.missing_job_id", event_data=event)
            await msg.term()
            metrics.JETSTREAM_MESSAGES.labels(result="term").inc()
            return
        assert self._db is not None
        ctx = {
            "db": self._db,
            "s3": self._s3,
            "work_dir": self._settings.work_dir,
        }
        row = self._db.fetchrow("SELECT org_id, params FROM jobs WHERE id = %s", job_id)
        if row is None:
            logger.error("worker.job_not_found", job_id=job_id)
            await msg.term()
            metrics.JETSTREAM_MESSAGES.labels(result="term").inc()
            return
        params = row["params"] or {}
        if isinstance(params, str):
            params = json.loads(params)
        processor_name = ((params.get("_processor") or {}).get("name") or "").strip()
        if not processor_name:
            logger.warning("worker.no_processor", job_id=job_id)
            await msg.ack()
            metrics.JETSTREAM_MESSAGES.labels(result="ack").inc()
            return
        proc = get_processor(processor_name)
        if proc is None:
            logger.warning(
                "worker.unknown_processor",
                processor=processor_name,
                job_id=job_id,
            )
            await msg.ack()
            metrics.JETSTREAM_MESSAGES.labels(result="ack").inc()
            return
        parent_ctx = extract(event.get("headers") or {})
        start = time.monotonic()
        try:
            with tracer.start_as_current_span(
                "worker.process_job",
                context=parent_ctx,
                attributes={"job.id": job_id, "job.processor": processor_name},
            ) as span:
                try:
                    result = await proc(ctx, job_id)
                except Exception as exc:
                    span.set_status(Status(StatusCode.ERROR, str(exc)))
                    raise
                span.set_status(Status(StatusCode.OK))
            metrics.JOBS_PROCESSED.labels(processor=processor_name, status="completed").inc()
            self._db.mark_job_completed(job_id, result or {})
            self._sync_workflow_status(job_id, params, completed=True, result=result or {})
            self._db.enqueue_outbox(
                org_id=str(row["org_id"]),
                aggregate_id=job_id,
                event_type="job.completed",
                payload={
                    "job_id": job_id,
                    "processor": processor_name,
                    "duration_seconds": (result or {}).get("duration_seconds"),
                },
            )
            await msg.ack()
            metrics.JETSTREAM_MESSAGES.labels(result="ack").inc()
        except Exception as exc:
            metrics.JOBS_PROCESSED.labels(processor=processor_name, status="failed").inc()
            logger.exception(
                "worker.processor_failed",
                job_id=job_id,
                processor=processor_name,
            )
            self._db.mark_job_failed(job_id, str(exc))
            self._sync_workflow_status(job_id, params, completed=False, error=str(exc))
            self._db.enqueue_outbox(
                org_id=str(row["org_id"]),
                aggregate_id=job_id,
                event_type="job.failed",
                payload={
                    "job_id": job_id,
                    "processor": processor_name,
                    "error": str(exc),
                },
            )
            await msg.nak()
            metrics.JETSTREAM_MESSAGES.labels(result="nak").inc()
        finally:
            metrics.JOB_PROCESSING_DURATION.labels(processor=processor_name).observe(
                time.monotonic() - start
            )

    def _sync_workflow_status(
        self,
        job_id: str,
        params: dict[str, Any],
        *,
        completed: bool,
        result: dict[str, Any] | None = None,
        error: str | None = None,
    ) -> None:
        assert self._db is not None
        if not isinstance(params, dict):
            return
        wf_id = params.get("_workflow_id")
        if not wf_id:
            return
        new_status = "completed" if completed else "failed"
        payload = json.dumps(result) if completed and result is not None else None
        try:
            if completed:
                self._db.execute(
                    "UPDATE workflows SET status = %s, result = %s, updated_at = now() "
                    "WHERE current_job_id = %s",
                    new_status,
                    payload,
                    job_id,
                )
            else:
                self._db.execute(
                    "UPDATE workflows SET status = %s, error = %s, updated_at = now() "
                    "WHERE current_job_id = %s",
                    new_status,
                    error,
                    job_id,
                )
        except Exception:
            logger.warning(
                "worker.workflow_sync_failed",
                workflow_id=wf_id,
                job_id=job_id,
                exc_info=True,
            )


async def run() -> None:
    from .logging import configure

    settings = get_settings()
    configure(settings.log_level)
    worker = Worker(settings)
    await worker.start()
    stop = asyncio.Event()
    loop = asyncio.get_event_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    await stop.wait()
    await worker.stop()


def main() -> None:
    asyncio.run(run())


if __name__ == "__main__":
    main()
