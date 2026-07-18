"""Tests for processor catalog sync (Phase 2)."""

from __future__ import annotations

import os
from contextlib import contextmanager

import psycopg
import pytest

import orpheus_workers.worker  # noqa: F401 — registers all processors
from orpheus_workers.catalog import sync_catalog
from orpheus_workers.processors import list_manifests


class _FakeCursor:
    def __init__(self) -> None:
        self.executed: list[str] = []
        self._id = 0

    def execute(self, sql: str, params=None) -> None:
        self.executed.append(sql)

    def fetchone(self):
        self._id += 1
        return (f"proc-{self._id}",)

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class _FakeConn:
    def __init__(self, cur: _FakeCursor) -> None:
        self._cur = cur

    def cursor(self):
        return self._cur


class _FakeDB:
    def __init__(self) -> None:
        self.cur = _FakeCursor()

    @contextmanager
    def conn(self):
        yield _FakeConn(self.cur)


def test_sync_catalog_upserts_every_manifest() -> None:
    db = _FakeDB()
    n = sync_catalog(db)
    assert n == len(list_manifests())
    inserts_proc = [s for s in db.cur.executed if "INSERT INTO processors" in s]
    inserts_ver = [s for s in db.cur.executed if "INSERT INTO processor_versions" in s]
    # One processor upsert and one version upsert per manifest.
    assert len(inserts_proc) == n
    assert len(inserts_ver) == n
    # Upserts, not blind inserts.
    assert all("ON CONFLICT (name) DO UPDATE" in s for s in inserts_proc)
    assert all("ON CONFLICT (processor_id, version) DO UPDATE" in s for s in inserts_ver)


@pytest.mark.skipif(
    not os.getenv("ORPHEUS_TEST_DATABASE_URL"), reason="ORPHEUS_TEST_DATABASE_URL not set"
)
def test_sync_catalog_real_db_idempotent() -> None:
    dsn = os.environ["ORPHEUS_TEST_DATABASE_URL"]

    # Minimal real-DB shim mirroring WorkerDB.conn (service role + commit).
    class RealDB:
        def __init__(self, dsn: str) -> None:
            self._conn = psycopg.connect(dsn, autocommit=False)

        @contextmanager
        def conn(self):
            with self._conn.cursor() as cur:
                cur.execute("SET LOCAL app.is_service = 'true'")
            try:
                yield self._conn
                self._conn.commit()
            except Exception:
                self._conn.rollback()
                raise

    db = RealDB(dsn)
    n1 = sync_catalog(db)
    n2 = sync_catalog(db)  # second run must be a clean no-op update
    assert n1 == n2 == len(list_manifests())

    with db.conn() as c, c.cursor() as cur:
        cur.execute("SELECT count(*) FROM processors WHERE name = 'convert-to-wav'")
        assert cur.fetchone()[0] == 1
        cur.execute(
            """SELECT p.tier, pv.model_id
               FROM processors p JOIN processor_versions pv ON pv.processor_id = p.id
               WHERE p.name = 'transcribe' AND pv.version = '1.0.0'"""
        )
        row = cur.fetchone()
        assert row is not None and row[0] == "cpu_medium" and row[1] == "whisper"
