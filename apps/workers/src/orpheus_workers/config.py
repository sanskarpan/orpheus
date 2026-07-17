from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict

from orpheus_workers import __version__


class WorkerSettings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="ORPHEUS_WORKER_")

    grpc_port: int = 50051
    http_port: int = 8081
    metrics_port: int = 8082
    nats_url: str = "nats://localhost:4222"
    log_level: str = "INFO"
    worker_concurrency: int = 4
    # Max jobs a single org may have running at once; excess are deferred
    # (redelivered later) so one tenant can't monopolise the worker pool.
    per_org_concurrency: int = 8
    # Cost rate applied to job wall-clock seconds to populate jobs.cost_usd
    # (a coarse CPU-second price; GPU tiers override later). 0 disables it.
    cost_usd_per_second: float = 0.00005
    # How often (seconds) to poll the JetStream consumer for pending-message
    # depth and publish it as the orpheus_jetstream_pending_messages gauge.
    queue_depth_poll_seconds: float = 15.0
    worker_version: str = __version__
    database_url: str = "postgres://orpheus:orpheus@localhost:5432/orpheus?sslmode=disable"
    s3_endpoint: str = "http://localhost:9000"
    s3_access_key: str = "orpheus"
    s3_secret_key: str = "orpheus-dev-secret"
    s3_bucket: str = "orpheus-uploads"
    work_dir: str = "/tmp/orpheus-workers"


def get_settings() -> WorkerSettings:
    return WorkerSettings()
