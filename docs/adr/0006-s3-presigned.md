# ADR-0006: S3 Multipart Upload via Presigned URLs (No Bytes Through API)

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Audio is MB–GB. Routing bytes through the API tier is a self-imposed DoS:
the API has to terminate TLS, allocate buffers, hold connections, and run
garbage collection for data the client could ship directly to object
storage.

## Decision

The client upload flow:

1. `POST /v1/uploads` — declares filename, content_type, size, intent.
2. API returns **presigned S3 multipart URLs** (one per part) plus an
   `upload_id`.
3. Client `PUT`s parts **directly to S3** (5+ MB per part).
4. `POST /v1/uploads/{id}/complete` — client sends back the per-part
   ETags.
5. API verifies all parts are present, computes the SHA-256, runs a
   format probe, creates the `Artifact` row, and writes an outbox event.

Bytes never touch the API tier. The SDK (Python and TypeScript) handles
the multipart choreography transparently.

## Consequences

- API tier scales independently of upload bandwidth.
- Egress is S3 → CloudFront, not from our infra.
- The S3 path is the only one that scales with media volume.
- Hard cap on upload size: 1 GB in Phase 1 (configurable).
- Client complexity is real; the SDK owns it.

## Alternatives Considered

- **Bytes through the API** — what the legacy prototype did. Self-imposed
  DoS, bad.
- **tus protocol for resumable uploads** — adds a stateful server
  component. The S3 multipart approach is already resumable (parts can
  fail and be re-uploaded). Defer tus.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §6.1 (Upload + transcribe flow),
  §8.3 (Endpoint inventory)
- AWS S3 docs, [Multipart upload overview](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html)
