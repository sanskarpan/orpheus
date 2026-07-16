"""Typed response models for the Orpheus API.

These are intentionally lightweight dataclasses built with ``from_dict``
constructors. They cover the fields the SDK cares about and preserve everything
the server sent under :attr:`_raw`, so forward-compatible fields are never lost.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Generic, List, Optional, TypeVar

T = TypeVar("T")


def _dict(data: Optional[Dict[str, Any]]) -> Dict[str, Any]:
    return data if isinstance(data, dict) else {}


@dataclass
class Part:
    part_number: int
    url: str
    expires_at: str
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Part":
        return cls(
            part_number=d["part_number"],
            url=d["url"],
            expires_at=d["expires_at"],
            _raw=d,
        )


@dataclass
class UploadSession:
    id: str
    status: str
    part_size: int
    parts: List[Part]
    expires_at: str
    created_at: str
    fields: Dict[str, str] = field(default_factory=dict)
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "UploadSession":
        return cls(
            id=d["id"],
            status=d["status"],
            part_size=d["part_size"],
            parts=[Part.from_dict(p) for p in d.get("parts", []) or []],
            expires_at=d["expires_at"],
            created_at=d["created_at"],
            fields=_dict(d.get("fields")),
            _raw=d,
        )


@dataclass
class Artifact:
    id: str
    sha256: str
    size_bytes: int
    content_type: str
    codec: str
    duration_seconds: float
    sample_rate: int
    channels: int
    created_at: str
    filename: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Artifact":
        return cls(
            id=d["id"],
            sha256=d["sha256"],
            size_bytes=d["size_bytes"],
            content_type=d["content_type"],
            codec=d["codec"],
            duration_seconds=d["duration_seconds"],
            sample_rate=d["sample_rate"],
            channels=d["channels"],
            created_at=d["created_at"],
            filename=d.get("filename"),
            _raw=d,
        )


@dataclass
class SignedURL:
    url: str
    expires_at: str
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "SignedURL":
        return cls(url=d["url"], expires_at=d["expires_at"], _raw=d)


@dataclass
class ProcessorRef:
    name: str
    version: str

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "ProcessorRef":
        return cls(name=d["name"], version=d["version"])

    def to_dict(self) -> Dict[str, str]:
        return {"name": self.name, "version": self.version}


@dataclass
class JobError:
    code: Optional[str] = None
    message: Optional[str] = None

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "JobError":
        return cls(code=d.get("code"), message=d.get("message"))


@dataclass
class Job:
    id: str
    artifact_id: str
    processor: ProcessorRef
    status: str
    attempts: int
    max_retries: int
    created_at: str
    updated_at: str
    params: Dict[str, Any] = field(default_factory=dict)
    result: Optional[Dict[str, Any]] = None
    error: Optional[JobError] = None
    cost_usd: Optional[float] = None
    started_at: Optional[str] = None
    completed_at: Optional[str] = None
    poll_url: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Job":
        err = d.get("error")
        return cls(
            id=d["id"],
            artifact_id=d["artifact_id"],
            processor=ProcessorRef.from_dict(d["processor"]),
            status=d["status"],
            attempts=d["attempts"],
            max_retries=d["max_retries"],
            created_at=d["created_at"],
            updated_at=d["updated_at"],
            params=_dict(d.get("params")),
            result=d.get("result"),
            error=JobError.from_dict(err) if isinstance(err, dict) else None,
            cost_usd=d.get("cost_usd"),
            started_at=d.get("started_at"),
            completed_at=d.get("completed_at"),
            poll_url=d.get("poll_url"),
            _raw=d,
        )

    @property
    def is_terminal(self) -> bool:
        return self.status in ("succeeded", "failed", "canceled")


@dataclass
class BulkRejection:
    index: int
    reason: str
    code: Optional[str] = None

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "BulkRejection":
        return cls(index=d["index"], reason=d["reason"], code=d.get("code"))


@dataclass
class BulkJobsResponse:
    batch_id: str
    accepted: List[str]
    rejected: List[BulkRejection]
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "BulkJobsResponse":
        return cls(
            batch_id=d["batch_id"],
            accepted=list(d.get("accepted", []) or []),
            rejected=[BulkRejection.from_dict(r) for r in d.get("rejected", []) or []],
            _raw=d,
        )


@dataclass
class WebhookEndpoint:
    id: str
    url: str
    subscribed_events: List[str]
    active: bool
    created_at: str
    description: Optional[str] = None
    secret: Optional[str] = None
    updated_at: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "WebhookEndpoint":
        return cls(
            id=d["id"],
            url=d["url"],
            subscribed_events=list(d.get("subscribed_events", []) or []),
            active=d["active"],
            created_at=d["created_at"],
            description=d.get("description"),
            secret=d.get("secret"),
            updated_at=d.get("updated_at"),
            _raw=d,
        )


@dataclass
class WebhookDelivery:
    id: str
    webhook_id: str
    event_id: str
    event_type: str
    status: str
    attempt_count: int
    created_at: str
    last_attempt_at: Optional[str] = None
    last_status_code: Optional[int] = None
    last_error: Optional[str] = None
    payload_url: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "WebhookDelivery":
        return cls(
            id=d["id"],
            webhook_id=d["webhook_id"],
            event_id=d["event_id"],
            event_type=d["event_type"],
            status=d["status"],
            attempt_count=d["attempt_count"],
            created_at=d["created_at"],
            last_attempt_at=d.get("last_attempt_at"),
            last_status_code=d.get("last_status_code"),
            last_error=d.get("last_error"),
            payload_url=d.get("payload_url"),
            _raw=d,
        )


@dataclass
class APIKey:
    id: str
    name: str
    prefix: str
    scopes: List[str]
    created_at: str
    secret: Optional[str] = None
    expires_at: Optional[str] = None
    last_used_at: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "APIKey":
        return cls(
            id=d["id"],
            name=d["name"],
            prefix=d["prefix"],
            scopes=list(d.get("scopes", []) or []),
            created_at=d["created_at"],
            secret=d.get("secret"),
            expires_at=d.get("expires_at"),
            last_used_at=d.get("last_used_at"),
            _raw=d,
        )


@dataclass
class ProcessorVersion:
    version: str
    status: str
    params_schema: Dict[str, Any] = field(default_factory=dict)
    input_schema: Dict[str, Any] = field(default_factory=dict)
    output_schema: Dict[str, Any] = field(default_factory=dict)
    default_params: Dict[str, Any] = field(default_factory=dict)
    released_at: Optional[str] = None
    deprecated_at: Optional[str] = None
    sunset_at: Optional[str] = None
    gpu_memory_mb: Optional[int] = None
    cost_per_second_usd: Optional[float] = None
    max_audio_duration_seconds: Optional[float] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "ProcessorVersion":
        return cls(
            version=d["version"],
            status=d["status"],
            params_schema=_dict(d.get("params_schema")),
            input_schema=_dict(d.get("input_schema")),
            output_schema=_dict(d.get("output_schema")),
            default_params=_dict(d.get("default_params")),
            released_at=d.get("released_at"),
            deprecated_at=d.get("deprecated_at"),
            sunset_at=d.get("sunset_at"),
            gpu_memory_mb=d.get("gpu_memory_mb"),
            cost_per_second_usd=d.get("cost_per_second_usd"),
            max_audio_duration_seconds=d.get("max_audio_duration_seconds"),
            _raw=d,
        )


@dataclass
class Processor:
    name: str
    display_name: str
    versions: List[ProcessorVersion]
    description: Optional[str] = None
    category: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Processor":
        return cls(
            name=d["name"],
            display_name=d["display_name"],
            versions=[ProcessorVersion.from_dict(v) for v in d.get("versions", []) or []],
            description=d.get("description"),
            category=d.get("category"),
            _raw=d,
        )


@dataclass
class ProcessorSummary:
    name: str
    display_name: str
    latest_version: str
    active_versions: List[str]
    description: Optional[str] = None
    category: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "ProcessorSummary":
        return cls(
            name=d["name"],
            display_name=d["display_name"],
            latest_version=d["latest_version"],
            active_versions=list(d.get("active_versions", []) or []),
            description=d.get("description"),
            category=d.get("category"),
            _raw=d,
        )


@dataclass
class UsagePeriod:
    start: str
    end: str

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "UsagePeriod":
        return cls(start=d["start"], end=d["end"])


@dataclass
class UsageBreakdown:
    category: str
    amount_usd: float
    quantity: Optional[float] = None
    unit: Optional[str] = None

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "UsageBreakdown":
        return cls(
            category=d["category"],
            amount_usd=d["amount_usd"],
            quantity=d.get("quantity"),
            unit=d.get("unit"),
        )


@dataclass
class Usage:
    org_id: str
    period: UsagePeriod
    jobs_count: int
    gpu_seconds: float
    storage_gb_days: float
    egress_gb: float
    total_usd: float
    breakdown: List[UsageBreakdown] = field(default_factory=list)
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Usage":
        return cls(
            org_id=d["org_id"],
            period=UsagePeriod.from_dict(d["period"]),
            jobs_count=d["jobs_count"],
            gpu_seconds=d["gpu_seconds"],
            storage_gb_days=d["storage_gb_days"],
            egress_gb=d["egress_gb"],
            total_usd=d["total_usd"],
            breakdown=[UsageBreakdown.from_dict(b) for b in d.get("breakdown", []) or []],
            _raw=d,
        )


@dataclass
class AuditLog:
    id: str
    org_id: str
    actor_type: str
    action: str
    resource_type: str
    created_at: str
    actor_id: Optional[str] = None
    resource_id: Optional[str] = None
    ip: Optional[str] = None
    user_agent: Optional[str] = None
    metadata: Optional[Dict[str, Any]] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "AuditLog":
        return cls(
            id=d["id"],
            org_id=d["org_id"],
            actor_type=d["actor_type"],
            action=d["action"],
            resource_type=d["resource_type"],
            created_at=d["created_at"],
            actor_id=d.get("actor_id"),
            resource_id=d.get("resource_id"),
            ip=d.get("ip"),
            user_agent=d.get("user_agent"),
            metadata=d.get("metadata"),
            _raw=d,
        )


@dataclass
class Page(Generic[T]):
    """One page of a cursor-paginated list.

    Iterate the whole collection with :meth:`OrpheusClient` list helpers, or step
    manually by passing :attr:`next_cursor` back as the ``cursor`` argument.
    """

    data: List[T]
    has_more: bool
    next_cursor: Optional[str] = None
    _raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    def __iter__(self):
        return iter(self.data)

    def __len__(self) -> int:
        return len(self.data)

    @classmethod
    def from_dict(cls, d: Dict[str, Any], item_from_dict) -> "Page[T]":
        return cls(
            data=[item_from_dict(x) for x in d.get("data", []) or []],
            has_more=bool(d.get("has_more", False)),
            next_cursor=d.get("next_cursor"),
            _raw=d,
        )
