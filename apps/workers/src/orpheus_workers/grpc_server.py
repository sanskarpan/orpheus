from __future__ import annotations

import structlog

import grpc

from orpheus.v1 import common_pb2, jobs_pb2, jobs_pb2_grpc

from .config import get_settings

logger = structlog.get_logger(__name__)


class WorkerServicer(jobs_pb2_grpc.WorkerServiceServicer):
    def __init__(self, version: str) -> None:
        self._version = version

    async def Ping(
        self, request: jobs_pb2.PingRequest, context: grpc.aio.ServicerContext
    ) -> jobs_pb2.PingResponse:
        return jobs_pb2.PingResponse(worker_version=self._version)

    async def GetJobStatus(
        self, request: jobs_pb2.GetJobStatusRequest, context: grpc.aio.ServicerContext
    ) -> jobs_pb2.GetJobStatusResponse:
        return jobs_pb2.GetJobStatusResponse(
            job_id=request.job_id, status=common_pb2.JOB_STATUS_UNSPECIFIED
        )


async def serve() -> None:
    settings = get_settings()
    server = grpc.aio.server()
    jobs_pb2_grpc.add_WorkerServiceServicer_to_server(
        WorkerServicer(settings.worker_version), server
    )
    server.add_insecure_port(f"0.0.0.0:{settings.grpc_port}")
    logger.info("grpc_server.starting", port=settings.grpc_port)
    await server.start()
    await server.wait_for_termination()


def main() -> None:
    import asyncio
    from .config import get_settings
    from .logging import configure

    settings = get_settings()
    configure(settings.log_level)
    asyncio.run(serve())


if __name__ == "__main__":
    main()
