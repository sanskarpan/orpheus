"""Tests for the ingest.url processor (PRD 09): SSRF checks + fetch."""

from __future__ import annotations

import hashlib
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from orpheus_workers.processors.ingest_url import SSRFError, check_ssrf, ingest_url

PAYLOAD = b"RIFF-fake-audio-bytes-" * 100


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(PAYLOAD)))
        self.end_headers()
        self.wfile.write(PAYLOAD)

    def log_message(self, *a):  # silence
        pass


@pytest.fixture
def http_server():
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        yield f"http://127.0.0.1:{srv.server_address[1]}/audio.wav"
    finally:
        srv.shutdown()
        srv.server_close()  # release the listening socket (else a ResourceWarning fails CI)
        t.join(timeout=2)


def test_check_ssrf_blocks(monkeypatch):
    monkeypatch.delenv("ORPHEUS_INGEST_ALLOW_HTTP", raising=False)
    monkeypatch.delenv("ORPHEUS_INGEST_ALLOW_PRIVATE", raising=False)
    with pytest.raises(SSRFError):
        check_ssrf("http://example.com/x")  # http not allowed
    with pytest.raises(SSRFError):
        check_ssrf("https://127.0.0.1/x")  # loopback
    with pytest.raises(SSRFError):
        check_ssrf("https://169.254.169.254/latest/meta-data")  # cloud metadata


class FakeDB:
    def __init__(self, url, s3_key, expected=""):
        self.url = url
        self.s3_key = s3_key
        self.expected = expected
        self.updates = []
        self.events = []
        self.inserted = False

    def fetchrow(self, sql, *args):
        if "org_id, params FROM jobs" in sql:
            return {
                "org_id": "org-1",
                "params": {
                    "upload_session_id": "sess-1",
                    "url": self.url,
                    "s3_bucket": "b",
                    "s3_key": self.s3_key,
                    "content_type": "audio/wav",
                    "expected_sha256": self.expected,
                },
            }
        if "INSERT INTO artifacts" in sql:
            self.inserted = True
            return {"id": "art-ingested"}
        raise AssertionError(sql)

    def execute(self, sql, *args):
        self.updates.append((sql, args))

    def enqueue_outbox(self, org_id, aggregate_id, event_type, payload):
        self.events.append((event_type, payload))


class FakeS3:
    def __init__(self):
        self.uploaded = {}

    def upload_file(self, bucket, key, src, content_type=None):
        from pathlib import Path

        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_ingest_url_fetches_and_completes(tmp_path, http_server, monkeypatch):
    monkeypatch.setenv("ORPHEUS_INGEST_ALLOW_HTTP", "1")
    monkeypatch.setenv("ORPHEUS_INGEST_ALLOW_PRIVATE", "1")
    db = FakeDB(http_server, "uploads/org-1/x.wav")
    s3 = FakeS3()
    ctx = {"db": db, "s3": s3, "work_dir": str(tmp_path), "bucket": "b"}

    res = await ingest_url(ctx, "job-1")
    assert res["size_bytes"] == len(PAYLOAD)
    assert res["sha256"] == hashlib.sha256(PAYLOAD).hexdigest()
    assert res["artifact_id"] == "art-ingested"
    assert s3.uploaded["uploads/org-1/x.wav"] == PAYLOAD
    # Session marked ready + upload.completed emitted.
    assert any("status='completed'" in u[0] for u in db.updates)
    assert db.events and db.events[0][0] == "upload.completed"


async def test_ingest_url_sha_mismatch_fails(tmp_path, http_server, monkeypatch):
    monkeypatch.setenv("ORPHEUS_INGEST_ALLOW_HTTP", "1")
    monkeypatch.setenv("ORPHEUS_INGEST_ALLOW_PRIVATE", "1")
    db = FakeDB(http_server, "uploads/org-1/y.wav", expected="deadbeef")
    ctx = {"db": db, "s3": FakeS3(), "work_dir": str(tmp_path), "bucket": "b"}
    with pytest.raises(ValueError):
        await ingest_url(ctx, "job-1")
    assert any("fetch_status='failed'" in u[0] for u in db.updates)
