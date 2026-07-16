# @orpheus/sdk (TypeScript)

A fetch-based, fully-typed TypeScript client for the
[Orpheus](https://docs.orpheus.dev) audio-processing API. Zero runtime
dependencies — it uses the platform `fetch` (Node 18+, Deno, Bun, browsers,
edge runtimes).

## Install

```bash
npm install @orpheus/sdk
```

## Authentication

Provide exactly one of:

- `apiKey` — an Orpheus API key (`ak_live_...`), sent as `X-API-Key`. For
  server-to-server / CI.
- `bearerToken` — a Keycloak JWT, sent as `Authorization: Bearer`. Required to
  mint API keys.

```ts
import { OrpheusClient } from '@orpheus/sdk';

const client = new OrpheusClient({ apiKey: process.env.ORPHEUS_API_KEY });
// against local dev:
const dev = new OrpheusClient({
  apiKey: 'ak_test_...',
  baseUrl: 'http://localhost:8080',
});
```

## Uploads

Create a session, `PUT` each part to its presigned URL, then report the ETags.

```ts
const session = await client.uploads.create(
  { filename: 'interview.wav', content_type: 'audio/wav', size_bytes: bytes.length },
  { idempotencyKey: crypto.randomUUID() },
);

const parts = [];
for (const part of session.parts) {
  const start = (part.part_number - 1) * session.part_size;
  const chunk = bytes.subarray(start, start + session.part_size);
  const res = await fetch(part.url, { method: 'PUT', body: chunk });
  parts.push({ part_number: part.part_number, etag: res.headers.get('etag')! });
}

const artifact = await client.uploads.complete(session.id, parts);
console.log(artifact.id, artifact.duration_seconds, artifact.codec);

await client.uploads.get(session.id);
const page = await client.uploads.list({ status: 'completed', limit: 100 });
```

## Artifacts

```ts
const artifact = await client.artifacts.get(artifactId);
const signed = await client.artifacts.signedUrl(artifactId, { expiresIn: 600 });
console.log(signed.url, 'valid until', signed.expires_at);

const artifacts = await client.artifacts.list({ content_type: 'audio/wav' });
```

## Jobs

```ts
const job = await client.jobs.create(
  {
    artifact_id: artifact.id,
    processor: { name: 'whisper-transcribe', version: '1.2.0' },
    params: { language: 'en', diarize: true },
    priority: 60,
  },
  { idempotencyKey: crypto.randomUUID() },
);

const fresh = await client.jobs.get(job.id);
if (fresh.status === 'succeeded') console.log(fresh.result);

await client.jobs.cancel(job.id);

// Bulk (up to 500); `rejected` carries per-item validation errors.
const batch = await client.jobs.bulkCreate(
  [
    { artifact_id: a1, processor: { name: 'whisper-transcribe', version: '1.2.0' } },
    { artifact_id: a2, processor: { name: 'demucs-separate', version: '4.0.0' } },
  ],
  { idempotencyKey: crypto.randomUUID() },
);
console.log(batch.batch_id, batch.accepted, batch.rejected);

const jobs = await client.jobs.list({ status: 'failed', processor: 'whisper-transcribe' });
```

## Webhooks

```ts
const hook = await client.webhooks.create({
  url: 'https://example.com/hooks/orpheus',
  subscribed_events: ['job.succeeded', 'job.failed'], // or ['*']
  description: 'prod pipeline',
});
console.log('save this secret:', hook.secret); // returned once, if generated

await client.webhooks.update(hook.id, { active: false });
await client.webhooks.get(hook.id);
const hooks = await client.webhooks.list();

const deliveries = await client.webhooks.listDeliveries(hook.id, { status: 'failed' });
for (const d of deliveries.data) {
  await client.webhooks.replayDelivery(hook.id, d.id);
}

await client.webhooks.delete(hook.id);
```

## API keys

Minting keys requires a Keycloak JWT.

```ts
const admin = new OrpheusClient({ bearerToken: keycloakJwt });
const key = await admin.apiKeys.create({ name: 'ci-deploy', scopes: ['jobs:read', 'jobs:write'] });
console.log(key.secret); // full token, shown exactly once

const keys = await admin.apiKeys.list();
await admin.apiKeys.delete(key.id);
```

## Processors (workflows catalog)

```ts
const processors = await client.processors.list();
const whisper = await client.processors.get('whisper-transcribe');
for (const v of whisper.versions) {
  console.log(v.version, v.status, v.cost_per_second_usd, v.params_schema);
}
```

## Usage & audit log

```ts
const usage = await client.usage({ period: 'current' }); // or '2026-07'
console.log(usage.total_usd, usage.gpu_seconds);

const log = await client.auditLog({ action: 'api_key.create', limit: 50 });
```

## Error handling

Every non-2xx response is decoded from RFC 7807 `problem+json` into a typed
error subclass.

```ts
import {
  BadRequestError,
  NotFoundError,
  ConflictError,
  RateLimitError,
  OrpheusAPIError,
} from '@orpheus/sdk';

try {
  await client.jobs.create({ artifact_id: 'missing', processor: { name: 'x', version: '1' } });
} catch (err) {
  if (err instanceof BadRequestError) {
    for (const f of err.errors) console.log(f.field, f.code, f.message);
  } else if (err instanceof RateLimitError) {
    console.log('retry after', err.retryAfter, 'seconds');
  } else if (err instanceof NotFoundError || err instanceof ConflictError) {
    // ...
  } else if (err instanceof OrpheusAPIError) {
    console.log(err.statusCode, err.problem?.title, err.requestId);
  }
}
```

## Pagination

List responses are `Page<T>` with `data`, `has_more`, and `next_cursor`. To walk
every page, feed `next_cursor` back as `cursor`:

```ts
let cursor: string | undefined;
do {
  const page = await client.jobs.list({ status: 'succeeded', cursor });
  for (const job of page.data) console.log(job.id);
  cursor = page.next_cursor ?? undefined;
} while (cursor);
```

## Idempotency

`create` / `bulkCreate` take `{ idempotencyKey }`. Replaying the same key with
the same body within 24h returns the original response; a different body yields
`409` (raised as `ConflictError`).
```
