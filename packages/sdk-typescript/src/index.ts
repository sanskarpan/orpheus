/**
 * Orpheus API TypeScript SDK.
 *
 * ```ts
 * import { OrpheusClient } from '@orpheus/sdk';
 *
 * const client = new OrpheusClient({ apiKey: 'ak_live_...' });
 * const job = await client.jobs.create({
 *   artifact_id,
 *   processor: { name: 'whisper-transcribe', version: '1.2.0' },
 * });
 * ```
 */

export { OrpheusClient } from './client';
export type { OrpheusClientOptions, FetchLike } from './client';

export {
  OrpheusError,
  OrpheusConnectionError,
  OrpheusAPIError,
  BadRequestError,
  AuthenticationError,
  PermissionDeniedError,
  NotFoundError,
  ConflictError,
  PayloadTooLargeError,
  RateLimitError,
  ServerError,
} from './errors';

export * from './types';
