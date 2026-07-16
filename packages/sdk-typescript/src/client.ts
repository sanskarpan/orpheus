/**
 * Fetch-based client for the Orpheus API.
 *
 * Runs anywhere the global `fetch` exists (Node 18+, Deno, Bun, browsers,
 * edge runtimes). Pass a custom `fetch` in options to override.
 */

import type {
  APIKey,
  Artifact,
  AuditLog,
  BulkJobsResponse,
  CompletedPart,
  CreateAPIKeyRequest,
  CreateJobRequest,
  CreateUploadRequest,
  CreateWebhookRequest,
  Job,
  ListArtifactsParams,
  ListAuditLogParams,
  ListDeliveriesParams,
  ListJobsParams,
  ListUploadsParams,
  Page,
  Processor,
  ProcessorSummary,
  SignedURL,
  UpdateWebhookRequest,
  UploadSession,
  Usage,
  WebhookDelivery,
  WebhookEndpoint,
} from './types';

import {
  errorFromResponse,
  OrpheusConnectionError,
} from './errors';

export type FetchLike = (
  input: string,
  init?: RequestInit,
) => Promise<Response>;

export interface OrpheusClientOptions {
  /** API key (`ak_live_...`). Sent as `X-API-Key`. */
  apiKey?: string;
  /** Keycloak JWT. Sent as `Authorization: Bearer`. Required to mint API keys. */
  bearerToken?: string;
  /** API base URL. Defaults to `https://api.orpheus.dev`. */
  baseUrl?: string;
  /** Per-request timeout in milliseconds. Defaults to 30_000. */
  timeoutMs?: number;
  /** Extra headers merged into every request. */
  defaultHeaders?: Record<string, string>;
  /** Custom fetch implementation. Defaults to the global `fetch`. */
  fetch?: FetchLike;
}

const DEFAULT_BASE_URL = 'https://api.orpheus.dev';
const DEFAULT_TIMEOUT_MS = 30_000;

interface RequestOptions {
  query?: Record<string, unknown>;
  body?: unknown;
  idempotencyKey?: string;
}

export class OrpheusClient {
  readonly uploads: UploadsAPI;
  readonly artifacts: ArtifactsAPI;
  readonly jobs: JobsAPI;
  readonly webhooks: WebhooksAPI;
  readonly apiKeys: APIKeysAPI;
  /** Catalog of processing operations ("workflows") and their versions. */
  readonly processors: ProcessorsAPI;

  private readonly baseUrl: string;
  private readonly timeoutMs: number;
  private readonly headers: Record<string, string>;
  private readonly fetchImpl: FetchLike;

  constructor(options: OrpheusClientOptions) {
    const hasKey = Boolean(options.apiKey);
    const hasToken = Boolean(options.bearerToken);
    if (hasKey === hasToken) {
      throw new Error('Provide exactly one of apiKey or bearerToken.');
    }

    this.baseUrl = (options.baseUrl ?? DEFAULT_BASE_URL).replace(/\/+$/, '');
    this.timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;

    const resolvedFetch = options.fetch ?? (globalThis.fetch as FetchLike | undefined);
    if (!resolvedFetch) {
      throw new Error(
        'No fetch implementation found. Use Node 18+, or pass options.fetch.',
      );
    }
    this.fetchImpl = resolvedFetch;

    this.headers = {
      Accept: 'application/json',
      'User-Agent': 'orpheus-sdk-typescript/0.2.0',
      ...(options.defaultHeaders ?? {}),
    };
    if (options.apiKey) {
      this.headers['X-API-Key'] = options.apiKey;
    } else {
      this.headers['Authorization'] = `Bearer ${options.bearerToken}`;
    }

    const request = this.request.bind(this);
    this.uploads = new UploadsAPI(request);
    this.artifacts = new ArtifactsAPI(request);
    this.jobs = new JobsAPI(request);
    this.webhooks = new WebhooksAPI(request);
    this.apiKeys = new APIKeysAPI(request);
    this.processors = new ProcessorsAPI(request);
  }

  /** Get billing-period usage. `period` is `"current"` or `"YYYY-MM"`. */
  usage(params: { period?: string } = {}): Promise<Usage> {
    return this.request<Usage>('GET', '/v1/usage', { query: params });
  }

  /** List audit-log entries for the org, newest first. */
  auditLog(params: ListAuditLogParams = {}): Promise<Page<AuditLog>> {
    return this.request<Page<AuditLog>>('GET', '/v1/audit-log', { query: params });
  }

  async request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    const url = this.baseUrl + path + buildQuery(opts.query);

    const headers: Record<string, string> = { ...this.headers };
    let init: RequestInit = { method, headers };
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }
    if (opts.idempotencyKey !== undefined) {
      headers['Idempotency-Key'] = opts.idempotencyKey;
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    init.signal = controller.signal;

    let response: Response;
    try {
      response = await this.fetchImpl(url, init);
    } catch (err) {
      const reason =
        (err as Error)?.name === 'AbortError'
          ? `request timed out after ${this.timeoutMs}ms`
          : String((err as Error)?.message ?? err);
      throw new OrpheusConnectionError(reason, err);
    } finally {
      clearTimeout(timer);
    }

    if (response.status === 204) {
      return undefined as T;
    }

    const text = await response.text();
    let body: unknown = undefined;
    if (text) {
      try {
        body = JSON.parse(text);
      } catch {
        body = text;
      }
    }

    if (!response.ok) {
      throw errorFromResponse(response.status, body, response.headers);
    }
    return body as T;
  }
}

type RequestFn = <T>(method: string, path: string, opts?: RequestOptions) => Promise<T>;

function buildQuery(query?: Record<string, unknown>): string {
  if (!query) return '';
  const parts: string[] = [];
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null) continue;
    parts.push(`${encodeURIComponent(key)}=${encodeURIComponent(String(value))}`);
  }
  return parts.length ? `?${parts.join('&')}` : '';
}

// ---------------------------------------------------------------------------
// Resource namespaces
// ---------------------------------------------------------------------------

class UploadsAPI {
  constructor(private readonly request: RequestFn) {}

  create(
    body: CreateUploadRequest,
    opts: { idempotencyKey?: string } = {},
  ): Promise<UploadSession> {
    return this.request<UploadSession>('POST', '/v1/uploads', {
      body,
      idempotencyKey: opts.idempotencyKey,
    });
  }

  get(id: string): Promise<UploadSession> {
    return this.request<UploadSession>('GET', `/v1/uploads/${id}`);
  }

  complete(id: string, parts: CompletedPart[]): Promise<Artifact> {
    return this.request<Artifact>('POST', `/v1/uploads/${id}/complete`, {
      body: { parts },
    });
  }

  list(params: ListUploadsParams = {}): Promise<Page<UploadSession>> {
    return this.request<Page<UploadSession>>('GET', '/v1/uploads', { query: params });
  }
}

class ArtifactsAPI {
  constructor(private readonly request: RequestFn) {}

  get(id: string): Promise<Artifact> {
    return this.request<Artifact>('GET', `/v1/artifacts/${id}`);
  }

  signedUrl(id: string, params: { expiresIn?: number } = {}): Promise<SignedURL> {
    return this.request<SignedURL>('GET', `/v1/artifacts/${id}/signed-url`, {
      query: { expires_in: params.expiresIn },
    });
  }

  list(params: ListArtifactsParams = {}): Promise<Page<Artifact>> {
    return this.request<Page<Artifact>>('GET', '/v1/artifacts', { query: params });
  }
}

class JobsAPI {
  constructor(private readonly request: RequestFn) {}

  create(body: CreateJobRequest, opts: { idempotencyKey?: string } = {}): Promise<Job> {
    return this.request<Job>('POST', '/v1/jobs', {
      body,
      idempotencyKey: opts.idempotencyKey,
    });
  }

  bulkCreate(
    jobs: CreateJobRequest[],
    opts: { idempotencyKey?: string } = {},
  ): Promise<BulkJobsResponse> {
    return this.request<BulkJobsResponse>('POST', '/v1/jobs/bulk', {
      body: { jobs },
      idempotencyKey: opts.idempotencyKey,
    });
  }

  get(id: string): Promise<Job> {
    return this.request<Job>('GET', `/v1/jobs/${id}`);
  }

  cancel(id: string): Promise<Job> {
    return this.request<Job>('POST', `/v1/jobs/${id}/cancel`);
  }

  list(params: ListJobsParams = {}): Promise<Page<Job>> {
    return this.request<Page<Job>>('GET', '/v1/jobs', { query: params });
  }
}

class WebhooksAPI {
  constructor(private readonly request: RequestFn) {}

  create(
    body: CreateWebhookRequest,
    opts: { idempotencyKey?: string } = {},
  ): Promise<WebhookEndpoint> {
    return this.request<WebhookEndpoint>('POST', '/v1/webhooks', {
      body,
      idempotencyKey: opts.idempotencyKey,
    });
  }

  get(id: string): Promise<WebhookEndpoint> {
    return this.request<WebhookEndpoint>('GET', `/v1/webhooks/${id}`);
  }

  update(id: string, body: UpdateWebhookRequest): Promise<WebhookEndpoint> {
    return this.request<WebhookEndpoint>('PATCH', `/v1/webhooks/${id}`, { body });
  }

  delete(id: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/webhooks/${id}`);
  }

  list(): Promise<Page<WebhookEndpoint>> {
    return this.request<Page<WebhookEndpoint>>('GET', '/v1/webhooks');
  }

  listDeliveries(
    id: string,
    params: ListDeliveriesParams = {},
  ): Promise<Page<WebhookDelivery>> {
    return this.request<Page<WebhookDelivery>>('GET', `/v1/webhooks/${id}/deliveries`, {
      query: params,
    });
  }

  replayDelivery(id: string, deliveryId: string): Promise<WebhookDelivery> {
    return this.request<WebhookDelivery>(
      'POST',
      `/v1/webhooks/${id}/deliveries/${deliveryId}/replay`,
    );
  }
}

class APIKeysAPI {
  constructor(private readonly request: RequestFn) {}

  /**
   * Mint an API key. Requires a Keycloak JWT (`bearerToken`).
   * The full `secret` is present on the returned object exactly once.
   */
  create(body: CreateAPIKeyRequest, opts: { idempotencyKey?: string } = {}): Promise<APIKey> {
    return this.request<APIKey>('POST', '/v1/api-keys', {
      body,
      idempotencyKey: opts.idempotencyKey,
    });
  }

  list(): Promise<Page<APIKey>> {
    return this.request<Page<APIKey>>('GET', '/v1/api-keys');
  }

  delete(id: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/api-keys/${id}`);
  }
}

class ProcessorsAPI {
  constructor(private readonly request: RequestFn) {}

  list(): Promise<Page<ProcessorSummary>> {
    return this.request<Page<ProcessorSummary>>('GET', '/v1/processors');
  }

  get(name: string): Promise<Processor> {
    return this.request<Processor>('GET', `/v1/processors/${name}`);
  }
}
