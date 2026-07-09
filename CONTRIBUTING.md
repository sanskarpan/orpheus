# Contributing

Thank you for considering contributing to Orpheus.

## Workflow

1. Create a feature branch: `feat/<name>`, `fix/<name>`, or `chore/<name>`.
2. Write code + tests.
3. Run `make check` locally (lint + format + type-check + tests).
4. Open a Pull Request targeting `main`.
5. CI must pass and at least one approval is required.
6. Squash-merge once approved.

## Commit message format

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <subject>

<body>

<footer>
```

Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`, `perf`,
`ci`, `build`. Subject line ≤ 72 chars, imperative mood.

Examples:

```
feat(api): add /v1/uploads endpoint with presigned URL issuance
fix(workers): make cleanup_old_artifacts idempotent on retry
docs(adr): add ADR-0013 on rate limiting strategy
```

## Code style

- **Python:** ruff (lint + format), pyright (type-check). Strict mode
  for new code.
- **TypeScript:** biome (lint + format), tsc (type-check).
- All settings live in `pyproject.toml` and `package.json` at the repo
  root.

## Testing

- Unit tests: `pytest` + `hypothesis` for Python, `vitest` for TS.
- Integration tests: `testcontainers-python` for ephemeral Postgres,
  Redis, MinIO, etc.
- Coverage: 80%+ on critical paths, 60%+ overall.
- Mutation testing (`mutmut`) on auth, billing, RLS.

See [`docs/architecture/PRODUCTION_DESIGN.md`](../architecture/PRODUCTION_DESIGN.md)
§14 for the full testing strategy.

## Architecture decisions

Significant changes to architecture require an ADR. See
[`docs/adr/0000-template.md`](../adr/0000-template.md). Open the ADR as
a PR alongside (or before) the code change.

## Security

- Never commit secrets. Use environment variables.
- Never log PII without redaction.
- All worker pods must run under gVisor (Phase 2+).
- Report security issues privately to the maintainers; do not open
  public issues for vulnerabilities.

## License

By contributing, you agree that your contributions will be licensed
under the Apache License 2.0.
