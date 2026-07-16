# orpheus-sdk (Python)

A dependency-light, typed Python client for the [Orpheus](https://docs.orpheus.dev)
audio-processing API. Built on [`httpx`](https://www.python-httpx.org/); no other
runtime dependencies.

## Install

```bash
pip install orpheus-sdk
# from a local checkout:
pip install -e packages/sdk-python
```

## Authentication

Orpheus accepts two schemes:

- **API key** (`X-API-Key`) — for server-to-server and CI use. Pass `api_key=`.
- **Keycloak JWT** (`Authorization: Bearer`) — required to mint API keys. Pass
  `bearer_token=`.

Provide exactly one.

```python
from orpheus_sdk import OrpheusClient

client = OrpheusClient(api_key="ak_live_...")
# or, against local dev:
client = OrpheusClient(api_key="ak_test_...", base_url="http://localhost:8080")
```

`OrpheusClient` is a context manager; use `with` (or call `client.close()`) so
the underlying HTTP connection pool is released.

## Uploads

A multipart upload is a three-step dance: create a session, `PUT` each part to
its presigned S3 URL, then report the ETags back with `complete`.

```python
import httpx

session = client.uploads.create(
    filename="interview.wav",
    content_type="audio/wav",
    size_bytes=len(audio_bytes),
    sha256="e3b0c442...",              # optional, verified on completion
    idempotency_key="a-uuid-v4",       # optional, safe retries
)

completed = []
for part in session.parts:
    chunk = audio_bytes[(part.part_number - 1) * session.part_size :][: session.part_size]
    resp = httpx.put(part.url, content=chunk)
    resp.raise_for_status()
    completed.append({"part_number": part.part_number, "etag": resp.headers["ETag"]})

artifact = client.uploads.complete(session.id, parts=completed)
print(artifact.id, artifact.duration_seconds, artifact.codec)

# also: client.uploads.get(id), client.uploads.list(status="completed")
for s in client.uploads.iterate(status="pending"):   # auto-paginates
    print(s.id)
```

## Artifacts

```python
artifact = client.artifacts.get(artifact_id)
signed = client.artifacts.signed_url(artifact_id, expires_in=600)  # 60..3600s
print(signed.url, "valid until", signed.expires_at)

for a in client.artifacts.iterate(content_type="audio/wav"):
    print(a.id)
```

## Jobs

```python
job = client.jobs.create(
    artifact_id=artifact.id,
    processor={"name": "whisper-transcribe", "version": "1.2.0"},
    params={"language": "en", "diarize": True},
    priority=60,
    idempotency_key="job-uuid",
)
print(job.status, job.poll_url)

job = client.jobs.get(job.id)
if job.is_terminal and job.status == "succeeded":
    print(job.result)

client.jobs.cancel(job.id)

# Bulk (up to 500). rejected[] carries per-item validation errors.
batch = client.jobs.bulk_create(
    jobs=[
        {"artifact_id": a1, "processor": {"name": "whisper-transcribe", "version": "1.2.0"}},
        {"artifact_id": a2, "processor": {"name": "demucs-separate", "version": "4.0.0"}},
    ],
    idempotency_key="batch-uuid",
)
print(batch.batch_id, batch.accepted, batch.rejected)

for j in client.jobs.iterate(status="failed", processor="whisper-transcribe"):
    print(j.id, j.error and j.error.message)
```

## Webhooks

```python
hook = client.webhooks.create(
    url="https://example.com/hooks/orpheus",
    subscribed_events=["job.succeeded", "job.failed"],   # or ["*"]
    description="prod pipeline",
)
print("save this secret:", hook.secret)   # only returned once, if generated

client.webhooks.update(hook.id, active=False)
client.webhooks.get(hook.id)
for h in client.webhooks.list():
    print(h.id, h.active)

# Deliveries + replay
deliveries = client.webhooks.list_deliveries(hook.id, status="failed")
for d in deliveries:
    print(d.id, d.event_type, d.last_status_code)
    client.webhooks.replay_delivery(hook.id, d.id)

client.webhooks.delete(hook.id)
```

## API keys

Minting keys requires a Keycloak JWT.

```python
admin = OrpheusClient(bearer_token="<keycloak-jwt>")
key = admin.api_keys.create(name="ci-deploy", scopes=["jobs:read", "jobs:write"])
print(key.secret)   # full token, shown exactly once

for k in admin.api_keys.list():
    print(k.prefix, k.scopes, k.last_used_at)

admin.api_keys.delete(key.id)
```

## Processors (workflows catalog)

```python
for p in client.processors.list():
    print(p.name, p.latest_version, p.active_versions)

proc = client.processors.get("whisper-transcribe")
for v in proc.versions:
    print(v.version, v.status, v.cost_per_second_usd)
    print(v.params_schema)   # JSON Schema for `params`
```

## Usage & audit log

```python
usage = client.usage(period="current")   # or "2026-07"
print(usage.total_usd, usage.gpu_seconds)
for line in usage.breakdown:
    print(line.category, line.amount_usd)

for entry in client.iter_audit_log(action="api_key.create"):
    print(entry.actor_type, entry.action, entry.resource_id)
```

## Error handling

Every non-2xx response is decoded from RFC 7807 `problem+json` and raised as a
typed exception:

```python
from orpheus_sdk import (
    BadRequestError, ConflictError, NotFoundError, RateLimitError, OrpheusAPIError,
)

try:
    client.jobs.create(artifact_id="missing", processor={"name": "x", "version": "1"})
except BadRequestError as e:
    for f in e.errors:                     # field-level details
        print(f.field, f.code, f.message)
except NotFoundError:
    ...
except ConflictError:                       # e.g. idempotency-key reuse
    ...
except RateLimitError as e:
    print("retry after", e.retry_after, "seconds")
except OrpheusAPIError as e:
    print(e.status_code, e.problem and e.problem.title, e.request_id)
```

## Idempotency

`create` / `bulk_create` accept `idempotency_key=`. Replaying the same key with
the same body within 24h returns the original response; a different body yields
`409 Conflict` (raised as `ConflictError`).
