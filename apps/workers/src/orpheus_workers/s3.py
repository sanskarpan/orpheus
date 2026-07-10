from __future__ import annotations

import boto3
from botocore.client import Config

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
        self._client.download_file(bucket, key, dest)
