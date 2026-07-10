from __future__ import annotations

import structlog
from arq.connections import RedisSettings
from arq.worker import Worker

from .config import WorkerSettings, get_settings
from .db import WorkerDB
from .processors import get_processor
from .processors import extract_metadata  # noqa: F401  (registers handler)
from .s3 import WorkerS3

logger = structlog.get_logger(__name__)


async def dispatch_job(ctx: dict, job_id: str) -> dict:
    db = ctx["db"]
    row = db.fetchrow("SELECT org_id, params FROM jobs WHERE id = %s", job_id)
    if row is None:
        raise ValueError(f"job {job_id} not found")
    org_id = str(row["org_id"])
    params = row["params"] or {}
    processor = (params.get("_processor") or {}).get("name") or ""
    proc = get_processor(processor) if processor else None
    if proc is None:
        logger.warning(
            "worker.unknown_processor",
            job_id=job_id,
            processor=processor,
        )
        return await noop_job(ctx, job_id)
    try:
        result = await proc(ctx, job_id)
    except Exception as e:
        logger.exception("worker.processor_failed", job_id=job_id, processor=processor)
        db.mark_job_failed(job_id, str(e))
        db.enqueue_outbox(
            org_id=org_id,
            aggregate_id=job_id,
            event_type="job.failed",
            payload={
                "job_id": job_id,
                "processor": processor,
                "error": str(e),
            },
        )
        raise
    db.mark_job_completed(job_id, result)
    db.enqueue_outbox(
        org_id=org_id,
        aggregate_id=job_id,
        event_type="job.completed",
        payload={
            "job_id": job_id,
            "processor": processor,
            "duration_seconds": result.get("duration_seconds"),
            "tags_count": len(result.get("tags") or {}),
        },
    )
    return result


async def noop_job(ctx: dict, job_id: str) -> dict:
    """Legacy fallback for unknown processors."""
    logger.info("worker.noop_job.start", job_id=job_id)
    return {"job_id": job_id, "status": "completed"}


async def startup(ctx: dict) -> None:
    settings: WorkerSettings = ctx["settings"]
    db = WorkerDB(settings)
    s3 = WorkerS3(settings)
    db.open()
    ctx["db"] = db
    ctx["s3"] = s3
    ctx["work_dir"] = settings.work_dir
    logger.info("worker.startup", work_dir=settings.work_dir)


async def shutdown(ctx: dict) -> None:
    db: WorkerDB | None = ctx.get("db")
    if db is not None:
        db.close()
    logger.info("worker.shutdown")


def get_worker() -> Worker:
    settings = get_settings()
    return Worker(
        redis_settings=RedisSettings.from_dsn(settings.redis_url),
        functions=[dispatch_job, noop_job],
        on_startup=startup,
        on_shutdown=shutdown,
        max_jobs=settings.worker_concurrency,
        ctx={"settings": settings},
    )


def main() -> None:
    from .logging import configure

    settings = get_settings()
    configure(settings.log_level)
    worker = get_worker()
    logger.info("worker.starting", redis_url=settings.redis_url)
    worker.run()
