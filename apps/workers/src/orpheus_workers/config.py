from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict

from orpheus_workers import __version__


class WorkerSettings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="ORPHEUS_WORKER_")

    grpc_port: int = 50051
    http_port: int = 8081
    redis_url: str = "redis://localhost:6379/0"
    log_level: str = "INFO"
    worker_concurrency: int = 4
    worker_version: str = __version__


def get_settings() -> WorkerSettings:
    return WorkerSettings()
