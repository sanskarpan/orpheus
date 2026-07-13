from prometheus_client import REGISTRY

from orpheus_workers import metrics


def _find_sample(sample_name: str, labels: dict[str, str]) -> bool:
    for family in REGISTRY.collect():
        for sample in family.samples:
            if sample.name == sample_name and sample.labels == labels:
                return True
    return False


def test_jobs_processed_registered() -> None:
    metrics.JOBS_PROCESSED.labels(processor="probe", status="completed").inc()
    assert _find_sample(
        "orpheus_jobs_processed_total",
        {"processor": "probe", "status": "completed"},
    )


def test_jobs_processed_failed_label_registered() -> None:
    metrics.JOBS_PROCESSED.labels(processor="slice", status="failed").inc()
    assert _find_sample(
        "orpheus_jobs_processed_total",
        {"processor": "slice", "status": "failed"},
    )


def test_jetstream_messages_registered() -> None:
    metrics.JETSTREAM_MESSAGES.labels(result="ack").inc()
    metrics.JETSTREAM_MESSAGES.labels(result="nak").inc()
    metrics.JETSTREAM_MESSAGES.labels(result="term").inc()
    metrics.JETSTREAM_MESSAGES.labels(result="parse_error").inc()
    for result in ("ack", "nak", "term", "parse_error"):
        assert _find_sample("orpheus_jetstream_messages_total", {"result": result})


def test_s3_operations_registered() -> None:
    metrics.S3_OPERATIONS.labels(op="download", result="success").inc()
    metrics.S3_OPERATIONS.labels(op="upload", result="error").inc()
    assert _find_sample(
        "orpheus_s3_operations_total",
        {"op": "download", "result": "success"},
    )
    assert _find_sample(
        "orpheus_s3_operations_total",
        {"op": "upload", "result": "error"},
    )


def test_job_processing_duration_observed() -> None:
    metrics.JOB_PROCESSING_DURATION.labels(processor="probe").observe(0.1)
    assert _find_sample(
        "orpheus_job_processing_duration_seconds_count",
        {"processor": "probe"},
    )
