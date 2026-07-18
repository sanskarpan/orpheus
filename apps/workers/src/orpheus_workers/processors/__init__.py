"""Processors are the work units the job consumer routes to.

The registry is built by importing the processor modules in this
package. Each processor declares an in-code **manifest** (tier, timeout,
retries, cost, version, model, i/o schema) alongside its function, so
the code — not a hand-written seed migration — is the source of truth
for the processor catalog. `catalog.sync_catalog` upserts these manifests
into the DB `processors` / `processor_versions` tables at worker startup
and on a NATS control signal (hot-reload).

Adding a processor is still a small change: define the function and
register it with @register_processor, optionally overriding manifest
fields.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable

ProcessorFn = Callable[[dict[str, Any], str], Awaitable[dict[str, Any]]]


@dataclass(frozen=True)
class ProcessorManifest:
    """Self-describing metadata for a processor, mirrored into the DB catalog.

    Fields map onto columns of `processors` (name..output_schema) and
    `processor_versions` (version, model_id, model_version_id, cacheable,
    slo_*). `tier` must be a member of the `processor_tier` enum
    (cpu_tiny/cpu_small/cpu_medium/cpu_large/gpu_a10g/gpu_a100).
    """

    name: str
    display_name: str
    description: str = ""
    tier: str = "cpu_tiny"
    timeout_seconds: int = 300
    max_retries: int = 3
    cost_per_job_usd: float = 0.0
    version: str = "1.0.0"
    model_id: str = "builtin"
    model_version_id: str = "builtin-1"
    cacheable: bool = True
    input_schema: dict[str, Any] = field(default_factory=dict)
    output_schema: dict[str, Any] = field(default_factory=dict)
    slo_p95_seconds: float | None = None
    slo_p99_seconds: float | None = None


_REGISTRY: dict[str, ProcessorFn] = {}
_MANIFESTS: dict[str, ProcessorManifest] = {}


def register_processor(
    job_type: str,
    *,
    display_name: str | None = None,
    description: str = "",
    tier: str = "cpu_tiny",
    timeout_seconds: int = 300,
    max_retries: int = 3,
    cost_per_job_usd: float = 0.0,
    version: str = "1.0.0",
    model_id: str = "builtin",
    model_version_id: str = "builtin-1",
    cacheable: bool = True,
    input_schema: dict[str, Any] | None = None,
    output_schema: dict[str, Any] | None = None,
    slo_p95_seconds: float | None = None,
    slo_p99_seconds: float | None = None,
) -> Callable[[ProcessorFn], ProcessorFn]:
    """Register a processor function and its manifest under ``job_type``."""

    def decorator(fn: ProcessorFn) -> ProcessorFn:
        _REGISTRY[job_type] = fn
        _MANIFESTS[job_type] = ProcessorManifest(
            name=job_type,
            display_name=display_name or job_type,
            description=description,
            tier=tier,
            timeout_seconds=timeout_seconds,
            max_retries=max_retries,
            cost_per_job_usd=cost_per_job_usd,
            version=version,
            model_id=model_id,
            model_version_id=model_version_id,
            cacheable=cacheable,
            input_schema=input_schema or {},
            output_schema=output_schema or {},
            slo_p95_seconds=slo_p95_seconds,
            slo_p99_seconds=slo_p99_seconds,
        )
        return fn

    return decorator


def get_processor(job_type: str) -> ProcessorFn | None:
    return _REGISTRY.get(job_type)


def list_processors() -> list[str]:
    return sorted(_REGISTRY)


def get_manifest(job_type: str) -> ProcessorManifest | None:
    return _MANIFESTS.get(job_type)


def list_manifests() -> list[ProcessorManifest]:
    return [_MANIFESTS[k] for k in sorted(_MANIFESTS)]
