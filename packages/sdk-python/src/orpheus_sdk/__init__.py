"""Orpheus API Python SDK.

A dependency-light, typed client for the Orpheus audio-processing API.

    from orpheus_sdk import OrpheusClient

    with OrpheusClient(api_key="ak_live_...") as client:
        for job in client.jobs.iterate(status="succeeded"):
            print(job.id, job.cost_usd)
"""

from __future__ import annotations

__version__ = "0.2.0"

from .client import DEFAULT_BASE_URL, OrpheusClient
from .errors import (
    AuthenticationError,
    BadRequestError,
    ConflictError,
    ErrorField,
    NotFoundError,
    OrpheusAPIError,
    OrpheusConnectionError,
    OrpheusError,
    PayloadTooLargeError,
    PermissionDeniedError,
    Problem,
    RateLimitError,
    ServerError,
)
from .models import (
    APIKey,
    Artifact,
    AuditLog,
    BulkJobsResponse,
    BulkRejection,
    Job,
    JobError,
    Page,
    Part,
    Processor,
    ProcessorRef,
    ProcessorSummary,
    ProcessorVersion,
    SignedURL,
    UploadSession,
    Usage,
    UsageBreakdown,
    UsagePeriod,
    WebhookDelivery,
    WebhookEndpoint,
)

__all__ = [
    "__version__",
    "OrpheusClient",
    "DEFAULT_BASE_URL",
    # errors
    "OrpheusError",
    "OrpheusConnectionError",
    "OrpheusAPIError",
    "BadRequestError",
    "AuthenticationError",
    "PermissionDeniedError",
    "NotFoundError",
    "ConflictError",
    "PayloadTooLargeError",
    "RateLimitError",
    "ServerError",
    "Problem",
    "ErrorField",
    # models
    "Part",
    "UploadSession",
    "Artifact",
    "SignedURL",
    "ProcessorRef",
    "Job",
    "JobError",
    "BulkJobsResponse",
    "BulkRejection",
    "WebhookEndpoint",
    "WebhookDelivery",
    "APIKey",
    "Processor",
    "ProcessorVersion",
    "ProcessorSummary",
    "Usage",
    "UsagePeriod",
    "UsageBreakdown",
    "AuditLog",
    "Page",
]
