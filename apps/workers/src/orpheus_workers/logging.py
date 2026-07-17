from __future__ import annotations

import logging
import sys

import structlog

from .redact import scrub_log_event


def _pii_scrub(_logger, _method, event_dict):
    """structlog processor: drop transcript/result payloads + mask PII in the
    message so no PII ever lands in logs (PRD 08 platform guarantee)."""
    return scrub_log_event(event_dict)


def configure(log_level: str = "INFO") -> None:
    logging.basicConfig(format="%(message)s", stream=sys.stdout, level=log_level)
    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,
            structlog.processors.add_log_level,
            structlog.processors.StackInfoRenderer(),
            structlog.processors.TimeStamper(fmt="iso"),
            structlog.processors.format_exc_info,
            _pii_scrub,
            structlog.processors.JSONRenderer(),
        ],
        wrapper_class=structlog.make_filtering_bound_logger(getattr(logging, log_level)),
        cache_logger_on_first_use=True,
    )
