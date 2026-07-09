# `orpheus-contracts`

Shared API contracts for Orpheus.

## Phase 0

Empty. Phase 1+ will add:

- OpenAPI 3.1 spec (sourced from the FastAPI app, versioned).
- AsyncAPI 3.0 spec (for webhook event schemas).
- Generated Python data classes from the OpenAPI spec.
- Reusable Pydantic models for shared types (pagination, errors,
  `Problem` details per RFC 7807).

## Layout

```
packages/contracts/
├── pyproject.toml
├── README.md
├── src/orpheus_contracts/
│   ├── __init__.py
│   ├── pagination.py     # Cursor pagination
│   ├── errors.py         # RFC 7807 Problem Details
│   ├── events.py         # Webhook event schemas (Phase 1+)
│   └── openapi/          # Generated OpenAPI spec + helpers (Phase 1+)
└── tests/
```
