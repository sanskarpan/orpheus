from __future__ import annotations

from typing import Any

import structlog
from arq.connections import RedisSettings
from arq.worker import Worker

from .config import get_settings

logger = structlog.get_logger(__name__)


async def noop_job(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Placeholder job. Real jobs (transcribe, slice, classify) land in Phase 4."""
    logger.info("worker.noop_job.start", job_id=job_id)
    return {"job_id": job_id, "status": "completed"}


async def startup(ctx: dict[str, Any]) -> None:
    logger.info("worker.startup")


async def shutdown(ctx: dict[str, Any]) -> None:
    logger.info("worker.shutdown")


def get_worker() -> Worker:
    settings = get_settings()
    return Worker(
        redis_settings=RedisSettings.from_dsn(settings.redis_url),
        functions=[noop_job],
        on_startup=startup,
        on_shutdown=shutdown,
        max_jobs=settings.worker_concurrency,
    )


def main() -> None:
    from .config import get_settings
    from .logging import configure

    settings = get_settings()
    configure(settings.log_level)
    worker = get_worker()
    logger.info("worker.starting", redis_url=settings.redis_url)
    worker.run()
