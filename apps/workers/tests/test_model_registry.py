"""Tests for the S3-backed model registry with checksum verification (Phase 4)."""

from __future__ import annotations

from pathlib import Path

import pytest

from orpheus_workers.model_registry import (
    ModelChecksumError,
    ModelNotFoundError,
    ModelRegistry,
    sha256_file,
)


class FakeS3:
    """In-memory S3: upload copies bytes in; download copies them back out."""

    def __init__(self) -> None:
        self.blobs: dict[tuple[str, str], bytes] = {}

    def upload_file(self, bucket, key, src, content_type=None):
        data = Path(src).read_bytes()
        self.blobs[(bucket, key)] = data
        return len(data)

    def download_file(self, bucket, key, dest):
        Path(dest).write_bytes(self.blobs[(bucket, key)])


class FakeDB:
    def __init__(self) -> None:
        self.rows: dict[tuple[str, str], dict] = {}

    def execute(self, sql, *args):
        # Positional args match the INSERT column order in ModelRegistry.register.
        name, version, framework, bucket, key, sha, size = args
        self.rows[(name, version)] = {
            "s3_bucket": bucket,
            "s3_key": key,
            "sha256": sha,
            "size_bytes": size,
        }

    def fetchrow(self, sql, *args):
        name, version = args
        return self.rows.get((name, version))


def _make_registry(tmp_path: Path) -> tuple[ModelRegistry, FakeS3, FakeDB]:
    s3, db = FakeS3(), FakeDB()
    reg = ModelRegistry(db, s3, cache_dir=tmp_path / "cache")
    return reg, s3, db


def test_register_records_sha256_and_uploads(tmp_path: Path) -> None:
    reg, s3, db = _make_registry(tmp_path)
    model = tmp_path / "model.bin"
    model.write_bytes(b"fake-weights-" * 1000)

    meta = reg.register("whisper", "tiny.en", model, framework="faster-whisper", bucket="b")
    assert meta["sha256"] == sha256_file(model)
    assert meta["size_bytes"] == model.stat().st_size
    assert ("b", meta["s3_key"]) in s3.blobs
    assert db.rows[("whisper", "tiny.en")]["sha256"] == meta["sha256"]


def test_resolve_downloads_and_verifies(tmp_path: Path) -> None:
    reg, _, _ = _make_registry(tmp_path)
    model = tmp_path / "model.bin"
    model.write_bytes(b"weights\x00\x01\x02" * 500)
    reg.register("m", "1", model, bucket="b")

    got = reg.resolve("m", "1")
    assert got.exists()
    assert sha256_file(got) == sha256_file(model)


def test_resolve_unregistered_raises(tmp_path: Path) -> None:
    reg, _, _ = _make_registry(tmp_path)
    with pytest.raises(ModelNotFoundError):
        reg.resolve("nope", "1")


def test_resolve_tamper_rejected(tmp_path: Path) -> None:
    reg, s3, db = _make_registry(tmp_path)
    model = tmp_path / "model.bin"
    model.write_bytes(b"good-weights" * 100)
    meta = reg.register("m", "1", model, bucket="b")

    # Corrupt the stored blob after registration (bytes no longer match sha256).
    s3.blobs[("b", meta["s3_key"])] = b"TAMPERED" * 100
    with pytest.raises(ModelChecksumError):
        reg.resolve("m", "1")
    # The bad blob must not be left in the cache.
    cached = tmp_path / "cache" / "m" / "1" / "model.bin"
    assert not cached.exists()


def test_resolve_cache_hit_no_redownload(tmp_path: Path) -> None:
    reg, s3, _ = _make_registry(tmp_path)
    model = tmp_path / "model.bin"
    model.write_bytes(b"cacheable" * 100)
    meta = reg.register("m", "1", model, bucket="b")

    first = reg.resolve("m", "1")
    # Remove the blob from S3 entirely; a cache hit must not need it.
    del s3.blobs[("b", meta["s3_key"])]
    second = reg.resolve("m", "1")
    assert first == second and second.exists()
