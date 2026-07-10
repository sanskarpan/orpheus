from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict

from orpheus_workers import __version__


class WorkerSettings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="ORPHEUS_WORKER_")

    grpc_port: int = 50051
    http_port: int = 8081
    nats_url: str = "nats://localhost:4222"
    log_level: str = "INFO"
    worker_concurrency: int = 4
    worker_version: str = __version__
    database_url: str = "postgres://orpheus:orpheus@localhost:5432/orpheus?sslmode=disable"
    s3_endpoint: str = "http://localhost:9000"
    s3_access_key: str = "orpheus"
    s3_secret_key: str = "orpheus-dev-secret"
    s3_bucket: str = "orpheus-uploads"
    work_dir: str = "/tmp/orpheus-workers"


def get_settings() -> WorkerSettings:
    return WorkerSettings()
