# ADR-0005: Model Versioning is First-Class

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Replicate, Modal, AssemblyAI, Deepgram all version their models.
Reproducibility, audit, A/B testing, deprecation, and enterprise sales
all require it.

## Decision

`ModelVersion` is a first-class entity. Every job pins a
`(processor_name, processor_version)` pair. Results record the exact
`model_version_id` (a content-addressed bundle identifier) that produced
them.

```python
class ProcessorManifest(BaseModel):
    name: str = Field(pattern=r"^[a-z][a-z0-9_]+(\.[a-z0-9_]+)+$")
    version: str = Field(pattern=r"^\d+\.\d+\.\d+$")
    model_id: str | None
    model_version_id: str | None
    # ... (input/output schemas, SLO, cost, resource tier)
```

Versions are **immutable** — a registered `1.4.2` cannot change. Bugfix =
`1.4.3`. Breaking input schema = `2.0.0`. The registry rejects republishing
the same version with different bytes.

## Consequences

- Can sell to enterprise (SOC 2 / regulated industries require it).
- Can A/B test models with sticky per-tenant bucketing.
- Can deprecate old versions on a schedule.
- Audit trail always shows which model produced which result.
- Schema slightly more complex. Worth it.

## Alternatives Considered

- **Unversioned processors** — what we had in the legacy prototype. Loses
  reproducibility, blocks A/B testing, blocks enterprise sales.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §5.3 (Module boundaries),
  §1.1–1.4 (Processor plugin model)
