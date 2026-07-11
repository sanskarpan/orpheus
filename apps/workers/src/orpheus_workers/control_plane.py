from __future__ import annotations

import structlog
from fastapi import FastAPI, Response

from .config import get_settings

logger = structlog.get_logger(__name__)


def create_app() -> FastAPI:
    settings = get_settings()
    app = FastAPI(title="orpheus-workers control plane", version=settings.worker_version)

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok", "version": settings.worker_version}

    @app.get("/ready")
    async def ready() -> dict[str, str]:
        return {"status": "ready"}

    @app.get("/metrics")
    async def metrics() -> Response:
        from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

        return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)

    return app


def main() -> None:
    import uvicorn
    from .config import get_settings
    from .logging import configure

    settings = get_settings()
    configure(settings.log_level)
    logger.info("control_plane.starting", port=settings.http_port)
    uvicorn.run(create_app(), host="0.0.0.0", port=settings.http_port, log_config=None)


if __name__ == "__main__":
    main()
