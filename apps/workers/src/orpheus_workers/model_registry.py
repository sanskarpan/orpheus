"""S3-backed model registry with checksum verification (Phase 4).

Model weights (whisper/pyannote/…) are stored in S3 and recorded in the
``model_registry`` table with a sha256. `resolve` downloads a model to a local
cache and verifies its sha256 before returning the path, so a corrupted or
tampered blob is never loaded. `register` publishes a local model file.

The registry is a global catalog (read-public, service-write); the worker DB
connection runs as the service role, so writes are permitted.
"""

from __future__ import annotations

import hashlib
import os
from pathlib import Path
from typing import Any

import structlog

logger = structlog.get_logger(__name__)

_CHUNK = 1 << 20  # 1 MiB


class ModelNotFoundError(Exception):
    """No registry row for the requested (name, version)."""


class ModelChecksumError(Exception):
    """A downloaded model's sha256 did not match the registry — refuse to load."""


def sha256_file(path: str | Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(_CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


class ModelRegistry:
    def __init__(self, db: Any, s3: Any, cache_dir: str | Path) -> None:
        self._db = db
        self._s3 = s3
        self._cache = Path(cache_dir)

    def register(
        self,
        name: str,
        version: str,
        local_path: str | Path,
        *,
        framework: str = "",
        bucket: str,
        key: str | None = None,
    ) -> dict[str, Any]:
        """Publish a local model file: upload to S3 and record it with its sha256."""
        local_path = Path(local_path)
        digest = sha256_file(local_path)
        size = local_path.stat().st_size
        if key is None:
            key = f"models/{name}/{version}/{local_path.name}"
        self._s3.upload_file(bucket, key, str(local_path), content_type="application/octet-stream")
        self._db.execute(
            """
            INSERT INTO model_registry (name, version, framework, s3_bucket, s3_key, sha256, size_bytes)
            VALUES (%s, %s, %s, %s, %s, %s, %s)
            ON CONFLICT (name, version) DO UPDATE SET
                framework  = EXCLUDED.framework,
                s3_bucket  = EXCLUDED.s3_bucket,
                s3_key     = EXCLUDED.s3_key,
                sha256     = EXCLUDED.sha256,
                size_bytes = EXCLUDED.size_bytes
            """,
            name,
            version,
            framework,
            bucket,
            key,
            digest,
            size,
        )
        logger.info("model_registry.registered", name=name, version=version, sha256=digest, size=size)
        return {"name": name, "version": version, "sha256": digest, "size_bytes": size, "s3_key": key}

    def resolve(self, name: str, version: str) -> Path:
        """Return a local path to the verified model, downloading + caching if needed.

        Raises ModelNotFoundError if unregistered, ModelChecksumError if the
        bytes don't match the registered sha256.
        """
        row = self._db.fetchrow(
            "SELECT s3_bucket, s3_key, sha256 FROM model_registry WHERE name = %s AND version = %s",
            name,
            version,
        )
        if row is None:
            raise ModelNotFoundError(f"model {name}:{version} not in registry")
        expected = row["sha256"]
        dest = self._cache / name / version / Path(row["s3_key"]).name
        dest.parent.mkdir(parents=True, exist_ok=True)

        # Cache hit: verify the cached copy still matches before trusting it.
        if dest.exists() and sha256_file(dest) == expected:
            logger.info("model_registry.cache_hit", name=name, version=version)
            return dest

        self._s3.download_file(row["s3_bucket"], row["s3_key"], str(dest))
        actual = sha256_file(dest)
        if actual != expected:
            # Never leave a bad blob in the cache.
            try:
                os.unlink(dest)
            except FileNotFoundError:
                pass
            raise ModelChecksumError(
                f"model {name}:{version} sha256 mismatch (expected {expected}, got {actual})"
            )
        logger.info("model_registry.resolved", name=name, version=version, sha256=actual)
        return dest
