from __future__ import annotations

import json
from contextlib import contextmanager
from typing import Any, Iterator

import structlog
from psycopg.rows import dict_row
from psycopg_pool import ConnectionPool

from .config import WorkerSettings

logger = structlog.get_logger(__name__)


class WorkerDB:
    def __init__(self, settings: WorkerSettings) -> None:
        self._pool = ConnectionPool(
            conninfo=settings.database_url,
            min_size=1,
            max_size=settings.worker_concurrency,
        )

    def open(self) -> None:
        self._pool.wait()

    def close(self) -> None:
        self._pool.close()

    @contextmanager
    def conn(self) -> Iterator[Any]:
        with self._pool.connection() as c:
            original_autocommit = c.autocommit
            c.autocommit = False
            try:
                with c.cursor() as cur:
                    cur.execute("SET LOCAL app.is_service = 'true'")
                yield c
                c.commit()
            except Exception:
                c.rollback()
                raise
            finally:
                c.autocommit = original_autocommit

    def fetchrow(self, sql: str, *args: Any) -> Any:
        with self.conn() as c, c.cursor(row_factory=dict_row) as cur:
            cur.execute(sql, args)
            return cur.fetchone()

    def execute(self, sql: str, *args: Any) -> None:
        with self.conn() as c, c.cursor() as cur:
            cur.execute(sql, args)

    def mark_job_completed(
        self, job_id: str, result: dict[str, Any], cost_usd: float = 0.0
    ) -> None:
        self.execute(
            "UPDATE jobs SET status = 'completed'::job_status, result = %s, cost_usd = %s, completed_at = now() WHERE id = %s",
            json.dumps(result),
            cost_usd,
            job_id,
        )

    def populate_result_cache(self, job_id: str, result: dict[str, Any]) -> None:
        """Populate the content-addressed cache (PRD 01) from a completed job.

        The cache key + hashes were computed once in the API and stored on the
        job as ``cache_meta`` (``{ck,ih,ph,mv}``), so the worker copies them
        verbatim — read and write can never disagree on the key. A no-op when
        the job carries no ``cache_meta``. Runs with the service role, so RLS
        is satisfied and ``org_id`` comes from the job row.
        """
        self.execute(
            """
            INSERT INTO job_result_cache (
                org_id, cache_key, input_hash, params_hash, model_version_id,
                source_job_id, result
            )
            SELECT
                org_id,
                decode(cache_meta->>'ck', 'hex'),
                cache_meta->>'ih',
                cache_meta->>'ph',
                cache_meta->>'mv',
                id,
                %s::jsonb
            FROM jobs
            WHERE id = %s AND cache_meta IS NOT NULL
            ON CONFLICT (org_id, cache_key) DO UPDATE
                SET result = EXCLUDED.result,
                    source_job_id = EXCLUDED.source_job_id
            """,
            json.dumps(result),
            job_id,
        )

    def mark_job_failed(self, job_id: str, error: str) -> None:
        self.execute(
            "UPDATE jobs SET status = 'failed'::job_status, result = %s, completed_at = now() WHERE id = %s",
            json.dumps({"error": error}),
            job_id,
        )

    # ── job-lifecycle orchestration (concurrency, retry, dead-letter) ──

    def claim_job(self, job_id: str) -> bool:
        """Atomically move a queued job to 'running', stamp started_at, and
        bump attempts. Returns True if this worker won the claim; False if
        the job was not 'queued' (already claimed/terminal), so a redelivery
        can't double-process."""
        row = self.fetchrow(
            """
            UPDATE jobs
            SET status = 'running'::job_status, started_at = now(), attempts = attempts + 1
            WHERE id = %s AND status = 'queued'::job_status
            RETURNING id
            """,
            job_id,
        )
        return row is not None

    def running_jobs_for_org(self, org_id: str) -> int:
        row = self.fetchrow(
            "SELECT count(*) AS n FROM jobs WHERE org_id = %s AND status = 'running'::job_status",
            org_id,
        )
        return int(row["n"]) if row else 0

    def job_retry_state(self, job_id: str) -> tuple[int, int]:
        """Return (attempts, max_retries) for a job."""
        row = self.fetchrow("SELECT attempts, max_retries FROM jobs WHERE id = %s", job_id)
        if row is None:
            return (0, 0)
        return (int(row["attempts"]), int(row["max_retries"]))

    def requeue_job_for_retry(self, job_id: str) -> None:
        """Return a running job to the queue so a redelivery can re-claim it."""
        self.execute(
            "UPDATE jobs SET status = 'queued'::job_status WHERE id = %s AND status = 'running'::job_status",
            job_id,
        )

    def mark_job_dead_letter(self, job_id: str, error: str) -> None:
        self.execute(
            "UPDATE jobs SET status = 'dead_letter'::job_status, result = %s, completed_at = now() WHERE id = %s",
            json.dumps({"error": error, "dead_letter": True}),
            job_id,
        )

    def mark_artifact_probed(
        self,
        artifact_id: str,
        codec: str | None,
        sample_rate: int | None,
        channels: int | None,
        duration_seconds: float | None,
    ) -> None:
        self.execute(
            """
            UPDATE artifacts
            SET probe_status = 'completed'::probe_status,
                codec = %s,
                sample_rate = %s,
                channels = %s,
                duration_seconds = %s
            WHERE id = %s
            """,
            codec,
            sample_rate,
            channels,
            duration_seconds,
            artifact_id,
        )

    def mark_artifact_probe_failed(self, artifact_id: str) -> None:
        self.execute(
            "UPDATE artifacts SET probe_status = 'failed'::probe_status WHERE id = %s",
            artifact_id,
        )

    def insert_artifact(
        self,
        artifact_id: str,
        org_id: str,
        s3_bucket: str,
        s3_key: str,
        content_type: str,
        size_bytes: int,
        probe_status: str = "pending",
    ) -> None:
        self.execute(
            """
            INSERT INTO artifacts (
                id, org_id, upload_session_id, s3_bucket, s3_key,
                sha256, size_bytes, content_type, probe_status, created_at
            )
            VALUES (
                %s, %s, NULL, %s, %s,
                '', %s, %s, %s::probe_status, now()
            )
            ON CONFLICT DO NOTHING
            """,
            artifact_id,
            org_id,
            s3_bucket,
            s3_key,
            size_bytes,
            content_type,
            probe_status,
        )

    def enqueue_outbox(
        self,
        org_id: str,
        aggregate_id: str,
        event_type: str,
        payload: dict[str, Any],
    ) -> None:
        self.execute(
            """
            INSERT INTO outbox (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)
            VALUES (gen_random_uuid(), %s, 'job', %s, %s, %s, '{}'::jsonb)
            """,
            org_id,
            aggregate_id,
            event_type,
            json.dumps(payload),
        )
