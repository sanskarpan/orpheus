"""Typed exceptions mapped from Orpheus RFC 7807 ``problem+json`` responses.

Every non-2xx HTTP response is decoded into a :class:`Problem` and raised as the
most specific :class:`OrpheusAPIError` subclass for its status code. Transport
level failures (DNS, TLS, connection reset, timeout) raise
:class:`OrpheusConnectionError` instead.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional


@dataclass
class ErrorField:
    """A single field-level validation error embedded in a :class:`Problem`."""

    field: str
    code: str
    message: str

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "ErrorField":
        return cls(
            field=data.get("field", ""),
            code=data.get("code", ""),
            message=data.get("message", ""),
        )


@dataclass
class Problem:
    """RFC 7807 problem details returned by the Orpheus API."""

    type: str
    title: str
    status: int
    detail: Optional[str] = None
    instance: Optional[str] = None
    errors: List[ErrorField] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "Problem":
        return cls(
            type=data.get("type", "about:blank"),
            title=data.get("title", ""),
            status=int(data.get("status", 0) or 0),
            detail=data.get("detail"),
            instance=data.get("instance"),
            errors=[ErrorField.from_dict(e) for e in data.get("errors", []) or []],
        )


class OrpheusError(Exception):
    """Base class for every error raised by the SDK."""


class OrpheusConnectionError(OrpheusError):
    """Raised when the request never reached the server (network/timeout)."""


class OrpheusAPIError(OrpheusError):
    """Raised for any non-2xx HTTP response.

    Attributes:
        status_code: The HTTP status code.
        problem: The parsed :class:`Problem`, when the body was ``problem+json``.
        request_id: The ``X-Request-Id`` response header, if present.
        response_headers: All response headers (lower-cased keys).
    """

    def __init__(
        self,
        status_code: int,
        problem: Optional[Problem] = None,
        request_id: Optional[str] = None,
        response_headers: Optional[Dict[str, str]] = None,
    ) -> None:
        self.status_code = status_code
        self.problem = problem
        self.request_id = request_id
        self.response_headers = response_headers or {}
        title = problem.title if problem else "HTTP error"
        detail = f": {problem.detail}" if problem and problem.detail else ""
        super().__init__(f"[{status_code}] {title}{detail}")

    @property
    def errors(self) -> List[ErrorField]:
        return self.problem.errors if self.problem else []


class BadRequestError(OrpheusAPIError):
    """400 - request validation failed; inspect :attr:`errors`."""


class AuthenticationError(OrpheusAPIError):
    """401 - missing or invalid credentials."""


class PermissionDeniedError(OrpheusAPIError):
    """403 - authenticated but not permitted (e.g. insufficient scopes)."""


class NotFoundError(OrpheusAPIError):
    """404 - resource does not exist or is not visible to the org."""


class ConflictError(OrpheusAPIError):
    """409 - state conflict, e.g. idempotency-key reuse with a different body."""


class PayloadTooLargeError(OrpheusAPIError):
    """413 - the requested upload exceeds the org's maximum artifact size."""


class RateLimitError(OrpheusAPIError):
    """429 - rate limit exceeded. See :attr:`retry_after`."""

    @property
    def retry_after(self) -> Optional[int]:
        raw = self.response_headers.get("retry-after")
        if raw is None:
            return None
        try:
            return int(raw)
        except ValueError:
            return None


class ServerError(OrpheusAPIError):
    """5xx - the server failed to fulfil an apparently valid request."""


_STATUS_TO_ERROR = {
    400: BadRequestError,
    401: AuthenticationError,
    403: PermissionDeniedError,
    404: NotFoundError,
    409: ConflictError,
    413: PayloadTooLargeError,
    429: RateLimitError,
}


def error_from_response(
    status_code: int,
    body: Any,
    headers: Dict[str, str],
) -> OrpheusAPIError:
    """Build the most specific :class:`OrpheusAPIError` for a response."""
    problem: Optional[Problem] = None
    if isinstance(body, dict):
        try:
            problem = Problem.from_dict(body)
        except Exception:  # noqa: BLE001 - never let error parsing mask the error
            problem = None

    lowered = {k.lower(): v for k, v in headers.items()}
    request_id = lowered.get("x-request-id")

    cls = _STATUS_TO_ERROR.get(status_code)
    if cls is None:
        cls = ServerError if status_code >= 500 else OrpheusAPIError
    return cls(
        status_code=status_code,
        problem=problem,
        request_id=request_id,
        response_headers=lowered,
    )
