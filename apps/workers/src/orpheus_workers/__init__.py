"""Orpheus worker plane.

Three runnable entry points:
  - orpheus_workers.control_plane  (FastAPI; /health, /ready, /metrics)
  - orpheus_workers.grpc_server    (gRPC; WorkerService.Ping, GetJobStatus)
  - orpheus_workers.worker          (arq worker; consumes jobs from Redis)
"""

__version__ = "0.1.0"
