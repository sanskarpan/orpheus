"""Processors are the work units the Arq job function routes to.

The registry is built by importing the processor modules in this
package. Adding a new processor is a 2-line change: define the
function, then register it with @register_processor.
"""

from __future__ import annotations

from typing import Any, Awaitable, Callable

ProcessorFn = Callable[[dict[str, Any], str], Awaitable[dict[str, Any]]]

_REGISTRY: dict[str, ProcessorFn] = {}


def register_processor(job_type: str) -> Callable[[ProcessorFn], ProcessorFn]:
    def decorator(fn: ProcessorFn) -> ProcessorFn:
        _REGISTRY[job_type] = fn
        return fn

    return decorator


def get_processor(job_type: str) -> ProcessorFn | None:
    return _REGISTRY.get(job_type)


def list_processors() -> list[str]:
    return sorted(_REGISTRY)
