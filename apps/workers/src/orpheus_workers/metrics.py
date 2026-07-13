"""Prometheus metrics for the Python worker.

Process-wide collectors are registered with the default registry
on first import. The worker process exposes them at :8082/metrics.
"""
from __future__ import annotations

from prometheus_client import Counter, Histogram

JOBS_PROCESSED = Counter(
    "orpheus_jobs_processed_total",
    "Total jobs processed by the worker, labeled by processor and status (completed/failed).",
    ["processor", "status"],
)

JOB_PROCESSING_DURATION = Histogram(
    "orpheus_job_processing_duration_seconds",
    "Time from JetStream message received to ack/nak, labeled by processor.",
    ["processor"],
    buckets=(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600),
)

JETSTREAM_MESSAGES = Counter(
    "orpheus_jetstream_messages_total",
    "Total JetStream messages handled, labeled by result (ack/nak/term/parse_error).",
    ["result"],
)

S3_OPERATIONS = Counter(
    "orpheus_s3_operations_total",
    "Total S3 operations performed, labeled by op (download/upload) and result (success/error).",
    ["op", "result"],
)
