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

    def mark_job_completed(self, job_id: str, result: dict[str, Any]) -> None:
        self.execute(
            "UPDATE jobs SET status = 'completed'::job_status, result = %s, completed_at = now() WHERE id = %s",
            json.dumps(result),
            job_id,
        )

    def mark_job_failed(self, job_id: str, error: str) -> None:
        self.execute(
            "UPDATE jobs SET status = 'failed'::job_status, result = %s, completed_at = now() WHERE id = %s",
            json.dumps({"error": error}),
            job_id,
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
