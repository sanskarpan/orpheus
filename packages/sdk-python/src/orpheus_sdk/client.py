"""Synchronous Orpheus API client built on ``httpx``.

Usage::

    from orpheus_sdk import OrpheusClient

    client = OrpheusClient(api_key="ak_live_...")
    session = client.uploads.create(
        filename="clip.wav", content_type="audio/wav", size_bytes=1_048_576
    )

The client authenticates with an API key (``X-API-Key``) by default. Pass
``bearer_token`` instead to authenticate with a Keycloak JWT (required for
minting API keys). Exactly one of ``api_key`` / ``bearer_token`` must be given.
"""

from __future__ import annotations

import platform
from typing import Any, Dict, Iterator, List, Optional, Union

import httpx

from . import __version__
from .errors import OrpheusConnectionError, error_from_response
from .models import (
    APIKey,
    Artifact,
    AuditLog,
    BulkJobsResponse,
    Job,
    Page,
    Processor,
    ProcessorSummary,
    SignedURL,
    UploadSession,
    Usage,
    WebhookDelivery,
    WebhookEndpoint,
)

DEFAULT_BASE_URL = "https://api.orpheus.dev"
DEFAULT_TIMEOUT = 30.0
JSON = Dict[str, Any]


def _prune(d: JSON) -> JSON:
    """Drop keys whose value is ``None`` so we never send explicit nulls."""
    return {k: v for k, v in d.items() if v is not None}


class _Transport:
    """Thin request/response layer: auth, JSON, error mapping, idempotency."""

    def __init__(
        self,
        base_url: str,
        headers: Dict[str, str],
        timeout: float,
        http_client: Optional[httpx.Client],
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._owns_client = http_client is None
        self._client = http_client or httpx.Client(timeout=timeout)
        self._default_headers = headers

    def request(
        self,
        method: str,
        path: str,
        *,
        params: Optional[JSON] = None,
        json: Optional[JSON] = None,
        idempotency_key: Optional[str] = None,
    ) -> Optional[Any]:
        headers = dict(self._default_headers)
        if idempotency_key is not None:
            headers["Idempotency-Key"] = idempotency_key

        url = self._base_url + path
        clean_params = _prune(params) if params else None
        try:
            response = self._client.request(
                method,
                url,
                params=clean_params,
                json=json,
                headers=headers,
            )
        except httpx.HTTPError as exc:  # transport-level failure
            raise OrpheusConnectionError(str(exc)) from exc

        if response.status_code == 204 or not response.content:
            if response.is_success:
                return None
            raise error_from_response(response.status_code, None, dict(response.headers))

        try:
            body = response.json()
        except ValueError:
            body = None

        if not response.is_success:
            raise error_from_response(response.status_code, body, dict(response.headers))
        return body

    def close(self) -> None:
        if self._owns_client:
            self._client.close()


class _UploadsAPI:
    def __init__(self, t: _Transport) -> None:
        self._t = t

    def create(
        self,
        *,
        filename: str,
        content_type: str,
        size_bytes: int,
        sha256: Optional[str] = None,
        idempotency_key: Optional[str] = None,
    ) -> UploadSession:
        body = _prune(
            {
                "filename": filename,
                "content_type": content_type,
                "size_bytes": size_bytes,
                "sha256": sha256,
            }
        )
        data = self._t.request("POST", "/v1/uploads", json=body, idempotency_key=idempotency_key)
        return UploadSession.from_dict(data)

    def get(self, upload_id: str) -> UploadSession:
        data = self._t.request("GET", f"/v1/uploads/{upload_id}")
        return UploadSession.from_dict(data)

    def complete(self, upload_id: str, *, parts: List[JSON]) -> Artifact:
        """Finalize an upload. ``parts`` is a list of ``{part_number, etag}``."""
        data = self._t.request("POST", f"/v1/uploads/{upload_id}/complete", json={"parts": parts})
        return Artifact.from_dict(data)

    def list(
        self,
        *,
        status: Optional[str] = None,
        created_after: Optional[str] = None,
        created_before: Optional[str] = None,
        limit: Optional[int] = None,
        cursor: Optional[str] = None,
    ) -> Page[UploadSession]:
        params = {
            "status": status,
            "created_after": created_after,
            "created_before": created_before,
            "limit": limit,
            "cursor": cursor,
        }
        data = self._t.request("GET", "/v1/uploads", params=params)
        return Page.from_dict(data, UploadSession.from_dict)

    def iterate(self, **kwargs: Any) -> Iterator[UploadSession]:
        yield from _paginate(self.list, **kwargs)


class _ArtifactsAPI:
    def __init__(self, t: _Transport) -> None:
        self._t = t

    def get(self, artifact_id: str) -> Artifact:
        data = self._t.request("GET", f"/v1/artifacts/{artifact_id}")
        return Artifact.from_dict(data)

    def signed_url(self, artifact_id: str, *, expires_in: Optional[int] = None) -> SignedURL:
        data = self._t.request(
            "GET",
            f"/v1/artifacts/{artifact_id}/signed-url",
            params={"expires_in": expires_in},
        )
        return SignedURL.from_dict(data)

    def list(
        self,
        *,
        content_type: Optional[str] = None,
        created_after: Optional[str] = None,
        created_before: Optional[str] = None,
        limit: Optional[int] = None,
        cursor: Optional[str] = None,
    ) -> Page[Artifact]:
        params = {
            "content_type": content_type,
            "created_after": created_after,
            "created_before": created_before,
            "limit": limit,
            "cursor": cursor,
        }
        data = self._t.request("GET", "/v1/artifacts", params=params)
        return Page.from_dict(data, Artifact.from_dict)

    def iterate(self, **kwargs: Any) -> Iterator[Artifact]:
        yield from _paginate(self.list, **kwargs)


class _JobsAPI:
    def __init__(self, t: _Transport) -> None:
        self._t = t

    def create(
        self,
        *,
        artifact_id: str,
        processor: Union[JSON, "object"],
        params: Optional[JSON] = None,
        priority: Optional[int] = None,
        idempotency_key: Optional[str] = None,
    ) -> Job:
        """Submit a job. ``processor`` is ``{"name": ..., "version": ...}``."""
        proc = processor.to_dict() if hasattr(processor, "to_dict") else processor
        body = _prune(
            {
                "artifact_id": artifact_id,
                "processor": proc,
                "params": params,
                "priority": priority,
            }
        )
        data = self._t.request("POST", "/v1/jobs", json=body, idempotency_key=idempotency_key)
        return Job.from_dict(data)

    def bulk_create(
        self, *, jobs: List[JSON], idempotency_key: Optional[str] = None
    ) -> BulkJobsResponse:
        data = self._t.request(
            "POST", "/v1/jobs/bulk", json={"jobs": jobs}, idempotency_key=idempotency_key
        )
        return BulkJobsResponse.from_dict(data)

    def get(self, job_id: str) -> Job:
        data = self._t.request("GET", f"/v1/jobs/{job_id}")
        return Job.from_dict(data)

    def cancel(self, job_id: str) -> Job:
        data = self._t.request("POST", f"/v1/jobs/{job_id}/cancel")
        return Job.from_dict(data)

    def list(
        self,
        *,
        status: Optional[str] = None,
        processor: Optional[str] = None,
        artifact_id: Optional[str] = None,
        created_after: Optional[str] = None,
        created_before: Optional[str] = None,
        limit: Optional[int] = None,
        cursor: Optional[str] = None,
    ) -> Page[Job]:
        params = {
            "status": status,
            "processor": processor,
            "artifact_id": artifact_id,
            "created_after": created_after,
            "created_before": created_before,
            "limit": limit,
            "cursor": cursor,
        }
        data = self._t.request("GET", "/v1/jobs", params=params)
        return Page.from_dict(data, Job.from_dict)

    def iterate(self, **kwargs: Any) -> Iterator[Job]:
        yield from _paginate(self.list, **kwargs)


class _WebhooksAPI:
    def __init__(self, t: _Transport) -> None:
        self._t = t

    def create(
        self,
        *,
        url: str,
        subscribed_events: List[str],
        description: Optional[str] = None,
        secret: Optional[str] = None,
        idempotency_key: Optional[str] = None,
    ) -> WebhookEndpoint:
        body = _prune(
            {
                "url": url,
                "subscribed_events": subscribed_events,
                "description": description,
                "secret": secret,
            }
        )
        data = self._t.request("POST", "/v1/webhooks", json=body, idempotency_key=idempotency_key)
        return WebhookEndpoint.from_dict(data)

    def get(self, webhook_id: str) -> WebhookEndpoint:
        data = self._t.request("GET", f"/v1/webhooks/{webhook_id}")
        return WebhookEndpoint.from_dict(data)

    def update(
        self,
        webhook_id: str,
        *,
        url: Optional[str] = None,
        subscribed_events: Optional[List[str]] = None,
        description: Optional[str] = None,
        active: Optional[bool] = None,
    ) -> WebhookEndpoint:
        body = _prune(
            {
                "url": url,
                "subscribed_events": subscribed_events,
                "description": description,
                "active": active,
            }
        )
        data = self._t.request("PATCH", f"/v1/webhooks/{webhook_id}", json=body)
        return WebhookEndpoint.from_dict(data)

    def delete(self, webhook_id: str) -> None:
        self._t.request("DELETE", f"/v1/webhooks/{webhook_id}")

    def list(self) -> Page[WebhookEndpoint]:
        data = self._t.request("GET", "/v1/webhooks")
        return Page.from_dict(data, WebhookEndpoint.from_dict)

    def list_deliveries(
        self,
        webhook_id: str,
        *,
        event_type: Optional[str] = None,
        status: Optional[str] = None,
        limit: Optional[int] = None,
        cursor: Optional[str] = None,
    ) -> Page[WebhookDelivery]:
        params = {
            "event_type": event_type,
            "status": status,
            "limit": limit,
            "cursor": cursor,
        }
        data = self._t.request("GET", f"/v1/webhooks/{webhook_id}/deliveries", params=params)
        return Page.from_dict(data, WebhookDelivery.from_dict)

    def replay_delivery(self, webhook_id: str, delivery_id: str) -> WebhookDelivery:
        data = self._t.request("POST", f"/v1/webhooks/{webhook_id}/deliveries/{delivery_id}/replay")
        return WebhookDelivery.from_dict(data)


class _APIKeysAPI:
    def __init__(self, t: _Transport) -> None:
        self._t = t

    def create(
        self,
        *,
        name: str,
        scopes: List[str],
        expires_at: Optional[str] = None,
        idempotency_key: Optional[str] = None,
    ) -> APIKey:
        """Mint an API key. Requires a Keycloak JWT (``bearer_token``).

        The full ``secret`` is present on the returned object exactly once.
        """
        body = _prune({"name": name, "scopes": scopes, "expires_at": expires_at})
        data = self._t.request("POST", "/v1/api-keys", json=body, idempotency_key=idempotency_key)
        return APIKey.from_dict(data)

    def list(self) -> Page[APIKey]:
        data = self._t.request("GET", "/v1/api-keys")
        return Page.from_dict(data, APIKey.from_dict)

    def delete(self, api_key_id: str) -> None:
        self._t.request("DELETE", f"/v1/api-keys/{api_key_id}")


class _ProcessorsAPI:
    """Catalog of available processing operations ("workflows") and versions."""

    def __init__(self, t: _Transport) -> None:
        self._t = t

    def list(self) -> Page[ProcessorSummary]:
        data = self._t.request("GET", "/v1/processors")
        return Page.from_dict(data, ProcessorSummary.from_dict)

    def get(self, name: str) -> Processor:
        data = self._t.request("GET", f"/v1/processors/{name}")
        return Processor.from_dict(data)


class OrpheusClient:
    """Top-level Orpheus API client.

    Args:
        api_key: An Orpheus API key (``ak_live_...`` / ``ak_test_...``). Sent as
            the ``X-API-Key`` header.
        bearer_token: A Keycloak JWT. Sent as ``Authorization: Bearer``. Use this
            (not ``api_key``) when minting API keys.
        base_url: API base URL. Defaults to ``https://api.orpheus.dev``.
        timeout: Per-request timeout in seconds.
        http_client: An existing ``httpx.Client`` to reuse. If given, ``timeout``
            is ignored and the caller owns the client's lifecycle.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        *,
        bearer_token: Optional[str] = None,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = DEFAULT_TIMEOUT,
        http_client: Optional[httpx.Client] = None,
    ) -> None:
        if bool(api_key) == bool(bearer_token):
            raise ValueError("Provide exactly one of api_key or bearer_token.")

        headers = {
            "Accept": "application/json",
            "User-Agent": (
                f"orpheus-sdk-python/{__version__} "
                f"httpx/{httpx.__version__} python/{platform.python_version()}"
            ),
        }
        if api_key:
            headers["X-API-Key"] = api_key
        else:
            headers["Authorization"] = f"Bearer {bearer_token}"

        self._t = _Transport(base_url, headers, timeout, http_client)

        self.uploads = _UploadsAPI(self._t)
        self.artifacts = _ArtifactsAPI(self._t)
        self.jobs = _JobsAPI(self._t)
        self.webhooks = _WebhooksAPI(self._t)
        self.api_keys = _APIKeysAPI(self._t)
        self.processors = _ProcessorsAPI(self._t)

    # -- system endpoints ---------------------------------------------------

    def usage(self, *, period: Optional[str] = None) -> Usage:
        """Get billing-period usage. ``period`` is ``"current"`` or ``"YYYY-MM"``."""
        data = self._t.request("GET", "/v1/usage", params={"period": period})
        return Usage.from_dict(data)

    def audit_log(
        self,
        *,
        action: Optional[str] = None,
        actor_id: Optional[str] = None,
        resource_type: Optional[str] = None,
        created_after: Optional[str] = None,
        created_before: Optional[str] = None,
        limit: Optional[int] = None,
        cursor: Optional[str] = None,
    ) -> Page[AuditLog]:
        params = {
            "action": action,
            "actor_id": actor_id,
            "resource_type": resource_type,
            "created_after": created_after,
            "created_before": created_before,
            "limit": limit,
            "cursor": cursor,
        }
        data = self._t.request("GET", "/v1/audit-log", params=params)
        return Page.from_dict(data, AuditLog.from_dict)

    def iter_audit_log(self, **kwargs: Any) -> Iterator[AuditLog]:
        yield from _paginate(self.audit_log, **kwargs)

    # -- lifecycle ----------------------------------------------------------

    def close(self) -> None:
        self._t.close()

    def __enter__(self) -> "OrpheusClient":
        return self

    def __exit__(self, *_exc: Any) -> None:
        self.close()


def _paginate(list_fn: Any, **kwargs: Any) -> Iterator[Any]:
    """Drive a cursor-paginated ``list`` method until exhaustion."""
    cursor = kwargs.pop("cursor", None)
    while True:
        page = list_fn(cursor=cursor, **kwargs)
        for item in page.data:
            yield item
        if not page.has_more or not page.next_cursor:
            return
        cursor = page.next_cursor
