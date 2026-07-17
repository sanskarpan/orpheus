"""Tests for the export.bundle processor (PRD 02).

No S3/DB: fakes stand in for both. We assert the zip contains the artifact
bytes and the embedded result.json, the bundle row is flipped to ready, and a
bundle.ready event is emitted. A failure path marks the bundle failed.
"""

from __future__ import annotations

import json
import shutil
import tempfile
import zipfile
from pathlib import Path

import pytest

from orpheus_workers.processors.export_bundle import export_bundle


class FakeS3:
    def __init__(self, artifact_bytes: bytes):
        self.artifact_bytes = artifact_bytes
        self.uploaded: dict[str, str] = {}

    def download_file(self, bucket: str, key: str, dest: str) -> None:
        Path(dest).write_bytes(self.artifact_bytes)

    def upload_file(self, bucket: str, key: str, src: str, content_type: str | None = None) -> int:
        # Copy the produced zip aside — the processor deletes the original
        # right after upload — so the test can inspect it.
        size = Path(src).stat().st_size
        kept = Path(tempfile.mkdtemp()) / "bundle.zip"
        shutil.copy(src, kept)
        self.uploaded[key] = str(kept)
        return size


class FakeDB:
    def __init__(self, org_id, bundle_id, items, artifacts, result_docs):
        self.org_id = org_id
        self.bundle_id = bundle_id
        self.items = items
        self.artifacts = artifacts
        self.result_docs = result_docs
        self.updates: list[tuple] = []
        self.events: list[tuple] = []

    def fetchrow(self, sql, *args):
        if "FROM jobs" in sql:
            return {"org_id": self.org_id, "params": {"bundle_id": self.bundle_id}}
        if "FROM bundles" in sql:
            return {"id": self.bundle_id, "org_id": self.org_id, "result_docs": self.result_docs}
        if "FROM artifacts" in sql:
            return self.artifacts.get(args[0])
        raise AssertionError(f"unexpected fetchrow: {sql}")

    def fetchall(self, sql, *args):
        assert "FROM bundle_items" in sql
        return self.items

    def execute(self, sql, *args):
        self.updates.append((sql, args))

    def enqueue_outbox(self, org_id, aggregate_id, event_type, payload):
        self.events.append((event_type, payload))


async def test_export_bundle_builds_zip(tmp_path):
    art_id = "11111111-1111-1111-1111-111111111111"
    db = FakeDB(
        org_id="org-1",
        bundle_id="bnd-1",
        items=[{"artifact_id": art_id, "path_in_zip": "audio.wav"}],
        artifacts={art_id: {"s3_bucket": "b", "s3_key": "k/audio.wav"}},
        result_docs={"job-x.result.json": {"text": "hi"}},
    )
    s3 = FakeS3(artifact_bytes=b"RIFFfake-wav-bytes")
    ctx = {"db": db, "s3": s3, "bucket": "orpheus-uploads", "work_dir": str(tmp_path)}

    res = await export_bundle(ctx, "job-1")
    assert res["artifact_count"] == 2
    assert res["size_bytes"] > 0

    # Bundle flipped to ready + event emitted.
    assert any("status = 'ready'" in u[0] for u in db.updates)
    assert db.events and db.events[0][0] == "bundle.ready"

    # The produced zip contains the artifact bytes and the result.json.
    zip_src = s3.uploaded["bundles/org-1/bnd-1.zip"]
    with zipfile.ZipFile(zip_src) as zf:
        names = set(zf.namelist())
        assert "audio.wav" in names
        assert "job-x.result.json" in names
        assert zf.read("audio.wav") == b"RIFFfake-wav-bytes"
        assert json.loads(zf.read("job-x.result.json")) == {"text": "hi"}


async def test_export_bundle_marks_failed_on_error(tmp_path):
    art_id = "22222222-2222-2222-2222-222222222222"

    class BoomS3(FakeS3):
        def download_file(self, bucket, key, dest):
            raise RuntimeError("s3 down")

    db = FakeDB(
        org_id="org-2",
        bundle_id="bnd-2",
        items=[{"artifact_id": art_id, "path_in_zip": "a.wav"}],
        artifacts={art_id: {"s3_bucket": "b", "s3_key": "k/a.wav"}},
        result_docs={},
    )
    s3 = BoomS3(artifact_bytes=b"x")
    ctx = {"db": db, "s3": s3, "bucket": "orpheus-uploads", "work_dir": str(tmp_path)}

    with pytest.raises(RuntimeError):
        await export_bundle(ctx, "job-2")
    assert any("status = 'failed'" in u[0] for u in db.updates)
    assert db.events and db.events[0][0] == "bundle.failed"
