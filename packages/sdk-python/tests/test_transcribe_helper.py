"""Tests for the high-level upload/transcribe helpers and the job poller."""

from __future__ import annotations

import json as _json

import pytest

from orpheus_sdk import JobFailedError, JobTimeoutError, OrpheusClient
from orpheus_sdk.models import Job

BASE = "https://api.test"

UPLOAD = {
    "id": "up_1",
    "status": "pending",
    "part_size": 8_388_608,
    "parts": [
        {"part_number": 1, "url": "https://r2.example/put1", "expires_at": "2026-01-01T00:00:00Z"}
    ],
    "expires_at": "2026-01-01T00:00:00Z",
    "created_at": "2026-01-01T00:00:00Z",
}

ARTIFACT = {
    "id": "art_1",
    "sha256": "abc",
    "size_bytes": 10,
    "content_type": "audio/wav",
    "codec": "pcm_s16le",
    "duration_seconds": 1.0,
    "sample_rate": 16000,
    "channels": 1,
    "created_at": "2026-01-01T00:00:00Z",
    "filename": "clip.wav",
}


def _job(status: str, result=None, error=None) -> dict:
    return {
        "id": "job_1",
        "artifact_id": "art_1",
        "processor": {"name": "transcribe", "version": "1.0.0"},
        "status": status,
        "attempts": 0,
        "max_retries": 3,
        "created_at": "2026-01-01T00:00:00Z",
        "updated_at": "2026-01-01T00:00:00Z",
        "result": result,
        "error": error,
    }


def _client() -> OrpheusClient:
    return OrpheusClient(api_key="ak_live_test", base_url=BASE)


def test_job_is_terminal_and_succeeded_use_completed():
    assert Job.from_dict(_job("completed")).is_terminal
    assert Job.from_dict(_job("completed")).succeeded
    assert Job.from_dict(_job("dead_letter")).is_terminal
    assert not Job.from_dict(_job("dead_letter")).succeeded
    assert not Job.from_dict(_job("running")).is_terminal


def test_transcribe_uploads_puts_completes_and_polls(httpx_mock, tmp_path):
    wav = tmp_path / "clip.wav"
    wav.write_bytes(b"RIFF\x00\x00\x00\x00WAVEfmt ")

    httpx_mock.add_response(method="POST", url=f"{BASE}/v1/uploads", json=UPLOAD)
    httpx_mock.add_response(
        method="PUT", url="https://r2.example/put1", headers={"ETag": '"etag-1"'}
    )
    httpx_mock.add_response(method="POST", url=f"{BASE}/v1/uploads/up_1/complete", json=ARTIFACT)
    httpx_mock.add_response(method="POST", url=f"{BASE}/v1/jobs", json=_job("queued"))
    httpx_mock.add_response(method="GET", url=f"{BASE}/v1/jobs/job_1", json=_job("running"))
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/v1/jobs/job_1",
        json=_job("completed", result={"text": "hello world"}),
    )

    with _client() as client:
        job = client.transcribe(str(wav), poll_interval=0, language="en", word_timestamps=True)

    assert job.succeeded
    assert job.result["text"] == "hello world"

    requests = httpx_mock.get_requests()
    # the presigned PUT carried the file bytes
    put = next(r for r in requests if r.method == "PUT")
    assert put.content == b"RIFF\x00\x00\x00\x00WAVEfmt "
    # the completion sent the collected etag
    complete = next(r for r in requests if r.url.path == "/v1/uploads/up_1/complete")
    assert _json.loads(complete.content)["parts"] == [{"part_number": 1, "etag": "etag-1"}]
    # the job carried processor + merged transcribe params
    create = next(r for r in requests if r.method == "POST" and r.url.path == "/v1/jobs")
    body = _json.loads(create.content)
    assert body["processor"] == {"name": "transcribe", "version": "1.0.0"}
    assert body["params"] == {"language": "en", "word_timestamps": True}


def test_wait_raises_job_failed(httpx_mock):
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/v1/jobs/job_1",
        json=_job("failed", error={"code": "boom", "message": "kaboom"}),
    )
    with _client() as client:
        with pytest.raises(JobFailedError) as ei:
            client.jobs.wait("job_1", poll_interval=0)
    assert ei.value.job.status == "failed"


def test_wait_times_out(httpx_mock):
    # Always running -> the deadline (0s) trips on the first check.
    httpx_mock.add_response(method="GET", url=f"{BASE}/v1/jobs/job_1", json=_job("running"))
    with _client() as client:
        with pytest.raises(JobTimeoutError):
            client.jobs.wait("job_1", poll_interval=0, timeout=0)
