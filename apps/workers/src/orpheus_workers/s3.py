from __future__ import annotations

import os
from typing import Any

import boto3
from botocore.client import Config

from . import metrics
from .config import WorkerSettings


class WorkerS3:
    def __init__(self, settings: WorkerSettings) -> None:
        self._client = boto3.client(
            "s3",
            endpoint_url=settings.s3_endpoint,
            aws_access_key_id=settings.s3_access_key,
            aws_secret_access_key=settings.s3_secret_key,
            config=Config(signature_version="s3v4"),
        )

    def download_file(self, bucket: str, key: str, dest: str) -> None:
        try:
            self._client.download_file(bucket, key, dest)
        except Exception:
            metrics.S3_OPERATIONS.labels(op="download", result="error").inc()
            raise
        metrics.S3_OPERATIONS.labels(op="download", result="success").inc()

    def upload_file(self, bucket: str, key: str, src: str, content_type: str | None = None) -> int:
        extra_args: dict[str, Any] = {}
        if content_type is not None:
            extra_args["ContentType"] = content_type
        try:
            self._client.upload_file(src, bucket, key, ExtraArgs=extra_args or None)
        except Exception:
            metrics.S3_OPERATIONS.labels(op="upload", result="error").inc()
            raise
        metrics.S3_OPERATIONS.labels(op="upload", result="success").inc()
        return os.path.getsize(src)
