"""ingest.url processor — SSRF-safe fetch of a source URL into S3 (PRD 09).

Validates the URL (https only by default; blocks private/loopback/link-local/
metadata addresses), streams the body with a hard 1 GiB cap, verifies an
optional expected sha256, uploads to the session's S3 key, creates the
artifact, and flips the upload session to ready. On any failure the session is
marked fetch_status='failed' and the job fails (retryable).
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import os
import socket
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import httpx

from . import register_processor

MAX_BYTES = 1 << 30  # 1 GiB
MAX_REDIRECTS = 5


class SSRFError(ValueError):
    pass


def _allow_http() -> bool:
    return os.environ.get("ORPHEUS_INGEST_ALLOW_HTTP", "").lower() in ("1", "true")


def _allow_private() -> bool:
    # Test/dev escape hatch (e.g. fetch from a local fixture server).
    return os.environ.get("ORPHEUS_INGEST_ALLOW_PRIVATE", "").lower() in ("1", "true")


def check_ssrf(url: str) -> None:
    """Raise SSRFError if the URL scheme or any resolved IP is disallowed."""
    u = urlparse(url)
    if u.scheme == "https" or (u.scheme == "http" and _allow_http()):
        pass
    else:
        raise SSRFError(f"scheme {u.scheme!r} not allowed")
    host = u.hostname
    if not host:
        raise SSRFError("url has no host")
    if _allow_private():
        return
    port = u.port or (443 if u.scheme == "https" else 80)
    try:
        infos = socket.getaddrinfo(host, port, proto=socket.IPPROTO_TCP)
    except OSError as exc:
        raise SSRFError(f"host does not resolve: {exc}") from exc
    for info in infos:
        ip = ipaddress.ip_address(info[4][0])
        if (
            ip.is_private
            or ip.is_loopback
            or ip.is_link_local
            or ip.is_multicast
            or ip.is_reserved
            or ip.is_unspecified
        ):
            raise SSRFError(f"blocked address {ip}")


def _fetch(url: str, dest: Path) -> tuple[int, str]:
    """Fetch url to dest with SSRF checks (incl. per-redirect), a size cap, and
    a running sha256. Returns (size, sha256_hex)."""
    hasher = hashlib.sha256()
    size = 0
    redirects = 0
    current = url
    with httpx.Client(follow_redirects=False, timeout=60.0) as client:
        while True:
            check_ssrf(current)
            with client.stream("GET", current) as resp:
                if resp.is_redirect:
                    redirects += 1
                    if redirects > MAX_REDIRECTS:
                        raise SSRFError("too many redirects")
                    loc = resp.headers.get("location", "")
                    if not loc:
                        raise SSRFError("redirect without location")
                    current = str(httpx.URL(current).join(loc))
                    continue
                resp.raise_for_status()
                clen = resp.headers.get("content-length")
                if clen and int(clen) > MAX_BYTES:
                    raise ValueError("source exceeds 1 GiB cap")
                with open(dest, "wb") as f:
                    for chunk in resp.iter_bytes():
                        size += len(chunk)
                        if size > MAX_BYTES:
                            raise ValueError("source exceeds 1 GiB cap")
                        hasher.update(chunk)
                        f.write(chunk)
                return size, hasher.hexdigest()


@register_processor(
    "ingest.url",
    display_name="Ingest URL",
    description="Fetch audio from an SSRF-checked URL into a new artifact.",
    tier="cpu_tiny",
    timeout_seconds=300,
    cost_per_job_usd=0.0005,
    model_id="fetch",
    model_version_id="fetch-1",
)
async def ingest_url(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT org_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = job["params"] or {}
    if isinstance(params, str):
        params = json.loads(params)
    org_id = job["org_id"]
    session_id = params["upload_session_id"]
    url = params["url"]
    bucket = params["s3_bucket"]
    key = params["s3_key"]
    content_type = params.get("content_type") or "application/octet-stream"
    expected = (params.get("expected_sha256") or "").strip().lower()

    work_dir.mkdir(parents=True, exist_ok=True)
    local = work_dir / f"ingest-{job_id}.bin"
    try:
        check_ssrf(url)
        size, sha = _fetch(url, local)
        if expected and sha != expected:
            raise ValueError("sha256 mismatch (integrity check failed)")
        s3.upload_file(bucket, key, str(local), content_type)
        artifact = db.fetchrow(
            """
            INSERT INTO artifacts (org_id, upload_session_id, s3_bucket, s3_key, sha256, size_bytes, content_type, probe_status)
            VALUES (%s,%s,%s,%s,%s,%s,%s,'pending'::probe_status)
            RETURNING id::text
            """,
            org_id,
            session_id,
            bucket,
            key,
            sha,
            size,
            content_type,
        )
        db.execute(
            "UPDATE upload_sessions SET status='completed'::upload_status, fetch_status='ready', bytes_fetched=%s, size_bytes=%s, completed_at=now() WHERE id=%s",
            size,
            size,
            session_id,
        )
        db.enqueue_outbox(
            org_id=org_id,
            aggregate_id=session_id,
            event_type="upload.completed",
            payload={"upload_id": session_id, "artifact_id": artifact["id"], "source": "url"},
        )
        return {"artifact_id": artifact["id"], "size_bytes": size, "sha256": sha}
    except Exception as exc:
        db.execute(
            "UPDATE upload_sessions SET fetch_status='failed', fetch_error=%s WHERE id=%s",
            str(exc)[:500],
            session_id,
        )
        raise
    finally:
        local.unlink(missing_ok=True)
