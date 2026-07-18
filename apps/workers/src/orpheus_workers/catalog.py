"""Sync in-code processor manifests into the DB catalog.

The worker's registered processors each declare a `ProcessorManifest`
(see `processors.__init__`). `sync_catalog` upserts those manifests into
the `processors` and `processor_versions` tables so the code is the single
source of truth for the catalog the API validates job submissions against.

It runs once at worker startup and again whenever a NATS control message
is received (hot-reload), so processor metadata can be refreshed across the
fleet without a redeploy. Writes go through the worker DB connection, which
runs as the service role (``app.is_service = true``), so RLS permits catalog
writes.
"""

from __future__ import annotations

import json
from typing import Any

import structlog

from .processors import ProcessorManifest, list_manifests

logger = structlog.get_logger(__name__)


def sync_catalog(db: Any) -> int:
    """Upsert every registered processor manifest into the DB catalog.

    Idempotent: re-running with unchanged manifests is a no-op update.
    Returns the number of processors synced.
    """
    manifests = list_manifests()
    with db.conn() as c, c.cursor() as cur:
        for m in manifests:
            _upsert_one(cur, m)
    logger.info("catalog.synced", processors=len(manifests))
    return len(manifests)


def _upsert_one(cur: Any, m: ProcessorManifest) -> None:
    cur.execute(
        """
        INSERT INTO processors
            (name, display_name, description, tier, timeout_seconds, max_retries,
             cost_per_job_usd, input_schema, output_schema)
        VALUES (%s, %s, %s, %s::processor_tier, %s, %s, %s, %s::jsonb, %s::jsonb)
        ON CONFLICT (name) DO UPDATE SET
            display_name     = EXCLUDED.display_name,
            description      = EXCLUDED.description,
            tier             = EXCLUDED.tier,
            timeout_seconds  = EXCLUDED.timeout_seconds,
            max_retries      = EXCLUDED.max_retries,
            cost_per_job_usd = EXCLUDED.cost_per_job_usd,
            input_schema     = EXCLUDED.input_schema,
            output_schema    = EXCLUDED.output_schema
        RETURNING id
        """,
        (
            m.name,
            m.display_name,
            m.description,
            m.tier,
            m.timeout_seconds,
            m.max_retries,
            m.cost_per_job_usd,
            json.dumps(m.input_schema),
            json.dumps(m.output_schema),
        ),
    )
    processor_id = cur.fetchone()[0]

    cur.execute(
        """
        INSERT INTO processor_versions
            (processor_id, version, model_id, model_version_id, cacheable,
             slo_p95_seconds, slo_p99_seconds, manifest)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s::jsonb)
        ON CONFLICT (processor_id, version) DO UPDATE SET
            model_id         = EXCLUDED.model_id,
            model_version_id = EXCLUDED.model_version_id,
            cacheable        = EXCLUDED.cacheable,
            slo_p95_seconds  = EXCLUDED.slo_p95_seconds,
            slo_p99_seconds  = EXCLUDED.slo_p99_seconds,
            manifest         = EXCLUDED.manifest
        """,
        (
            processor_id,
            m.version,
            m.model_id,
            m.model_version_id,
            m.cacheable,
            m.slo_p95_seconds,
            m.slo_p99_seconds,
            json.dumps(
                {
                    "tier": m.tier,
                    "timeout_seconds": m.timeout_seconds,
                    "max_retries": m.max_retries,
                    "cost_per_job_usd": m.cost_per_job_usd,
                }
            ),
        ),
    )
