# `packages/`

Shared libraries and contracts. Each subdirectory is a self-contained
package with its own `pyproject.toml` (or `package.json`).

## Layout

```
packages/
├── contracts/        # OpenAPI spec, AsyncAPI spec, Protobuf (Phase 1+)
├── proto/            # gRPC / Protobuf definitions (Phase 1+)
├── sdk-python/       # Python SDK (Phase 1+)
├── sdk-typescript/   # TypeScript SDK (Phase 1+)
└── sdks/             # Generated SDKs from OpenAPI (Phase 1+)
```

## Phase 0

Empty. ADRs and design docs only.
