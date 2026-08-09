import "server-only";
import { getApiKey } from "@/lib/session";

/* ================================================================== *
 * Server-only typed client for the Orpheus Go API (the BFF core).
 *
 * The browser never talks to /v1 directly (the API has no CORS and API
 * keys are machine credentials). Every call here runs on the Next server,
 * reads the key from the encrypted session, and forwards it.
 *
 * Field shapes are taken verbatim from the Go handler json tags.
 * ================================================================== */

export const API_URL = process.env.ORPHEUS_API_URL ?? "http://127.0.0.1:8090";

/* ---- error type (RFC 7807 problem+json) ---- */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail: string;
}

export class OrpheusError extends Error {
  status: number;
  kind: string;
  detail: string;
  constructor(p: Partial<Problem> & { status: number }) {
    super(p.detail || p.title || `HTTP ${p.status}`);
    this.name = "OrpheusError";
    this.status = p.status;
    this.detail = p.detail ?? "";
    // The machine-readable kind is the trailing path of `type`.
    this.kind = (p.type ?? "").split("/").pop() || "error";
  }
}

/* ---- core request ---- */

interface RequestOpts {
  method?: string;
  query?: Record<string, string | number | undefined | null>;
  body?: unknown;
  /** Override the key (used by /login to validate a candidate key). */
  apiKey?: string;
  /** Cache policy for RSC fetches; defaults to no-store (live data). */
  cache?: RequestCache;
  headers?: Record<string, string>;
}

function buildUrl(path: string, query?: RequestOpts["query"]): string {
  const url = new URL(path.startsWith("/") ? path : `/${path}`, API_URL);
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v !== undefined && v !== null && v !== "") url.searchParams.set(k, String(v));
    }
  }
  return url.toString();
}

export async function apiRequest<T>(path: string, opts: RequestOpts = {}): Promise<T> {
  const key = opts.apiKey ?? (await getApiKey());
  if (!key) throw new OrpheusError({ status: 401, type: "unauthenticated", detail: "No session key" });

  // Orpheus API keys authenticate via the X-API-Key header. The Authorization
  // Bearer path is reserved for Keycloak JWTs, which this dev console doesn't use.
  const headers: Record<string, string> = {
    "X-API-Key": key,
    Accept: "application/json",
    ...opts.headers,
  };
  let payload: BodyInit | undefined;
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(opts.body);
  }

  const res = await fetch(buildUrl(path, opts.query), {
    method: opts.method ?? "GET",
    headers,
    body: payload,
    cache: opts.cache ?? "no-store",
  });

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  if (!res.ok) {
    let problem: Partial<Problem> = { status: res.status };
    try {
      problem = { ...problem, ...(JSON.parse(text) as Problem) };
    } catch {
      problem.detail = text.slice(0, 300);
    }
    throw new OrpheusError({ ...problem, status: res.status });
  }

  const parsed = text ? JSON.parse(text) : undefined;
  // The list API returns {"data": null} (not []) for empty result sets across
  // every envelope shape. Normalize centrally so callers can always treat
  // `data` as an array.
  if (parsed && typeof parsed === "object" && "data" in parsed && parsed.data === null) {
    (parsed as { data: unknown[] }).data = [];
  }
  return parsed as T;
}

/* ================================================================== *
 * Domain types
 * ================================================================== */

export interface ListEnvelope<T> {
  data: T[];
  has_more: boolean;
  next_cursor?: string;
}

export type JobStatus = "queued" | "running" | "completed" | "failed" | "canceled" | "dead_letter";

export interface ProcessorRef {
  name: string;
  version: string;
}

export interface Job {
  id: string;
  artifact_id?: string;
  processor: ProcessorRef;
  status: JobStatus;
  params?: unknown;
  result?: unknown;
  attempts: number;
  max_retries: number;
  cost_usd?: number;
  cache?: "hit" | "miss";
  cached_from_job_id?: string;
  created_at: string;
  updated_at: string;
  started_at?: string;
  completed_at?: string;
}

export interface Artifact {
  id: string;
  sha256: string;
  size_bytes: number;
  content_type: string;
  codec?: string;
  duration_seconds?: number;
  sample_rate?: number;
  channels?: number;
  created_at: string;
}

export interface UploadPart {
  part_number: number;
  url: string;
  expires_at: string;
}

export interface UploadSession {
  id: string;
  status: string;
  part_size: number;
  parts: UploadPart[];
  expires_at: string;
  created_at: string;
}

export interface ProcessorSummary {
  name: string;
  display_name: string;
  description: string;
}

export interface ProcessorVersion {
  version: string;
  model_id?: string;
  model_version_id?: string;
  slo_p95_seconds: number;
  slo_p99_seconds: number;
  cost_usd: number;
  created_at: string;
  deprecated_at?: string;
}

export interface Processor {
  name: string;
  display_name: string;
  description: string;
  versions: ProcessorVersion[];
}

export interface APIKey {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  created_at: string;
  secret?: string;
}

export interface WebhookEndpoint {
  id: string;
  url: string;
  subscribed_events: string[];
  description: string;
  active: boolean;
  secret?: string;
  created_at: string;
  updated_at: string;
}

export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  event_id: string;
  event_type: string;
  status: "pending" | "delivering" | "delivered" | "failed" | "exhausted";
  attempt_count: number;
  last_attempt_at?: string;
  last_status_code?: number;
  last_error?: string;
  created_at: string;
}

export interface Usage {
  jobs_count: number;
  gpu_seconds: number;
  storage_gb_days: number;
  egress_gb: number;
  total_usd: number;
}

export interface TimeseriesPoint {
  bucket: string;
  group?: string;
  jobs: number;
  compute_seconds: number;
  cost_usd: number;
}

export interface Timeseries {
  granularity: string;
  group_by: string;
  from: string;
  to: string;
  series: TimeseriesPoint[];
}

export interface AuditEntry {
  id: string;
  actor_id: string;
  actor_type: string;
  action: string;
  resource_type: string;
  resource_id: string;
  ip: string;
  user_agent: string;
  request_id: string;
  created_at: string;
}

export interface MarketplaceSubmission {
  id: string;
  name: string;
  display_name: string;
  description: string;
  publisher: string;
  status: string;
  review_notes?: string;
  submitted_at: string;
  reviewed_at?: string;
}

export interface MarketplaceProcessor {
  name: string;
  display_name: string;
  description: string;
  trust_class: string;
  publisher: string;
  tier: string;
}

export interface StreamingSession {
  id: string;
  status: string;
  model_version_id?: string;
  started_at: string;
  ended_at?: string;
  audio_seconds?: number;
  transcript?: string;
  cost_usd: number;
  error?: string;
  ws_url?: string;
}

export interface ProvisionResponse {
  org_id: string;
  user_id: string;
  api_key: string;
  key_prefix: string;
  scopes: string[];
}

export interface SignedURL {
  url: string;
  expires_at: string;
}

/* ================================================================== *
 * Endpoint helpers — the surface the dashboard actually uses.
 * ================================================================== */

export const orpheus = {
  // Jobs
  listJobs: (q: { limit?: number; cursor?: string; status?: string; processor?: string } = {}) =>
    apiRequest<ListEnvelope<Job>>("/v1/jobs", { query: q }),
  getJob: (id: string) => apiRequest<Job>(`/v1/jobs/${id}`),
  createJob: (body: {
    artifact_id: string;
    processor: ProcessorRef;
    params?: unknown;
    cache?: "auto" | "bypass" | "only";
    priority?: number;
  }) => apiRequest<Job>("/v1/jobs", { method: "POST", body }),
  requeueJob: (id: string) => apiRequest<Job>(`/v1/jobs/${id}/requeue`, { method: "POST" }),
  cancelJob: (id: string) => apiRequest<Job>(`/v1/jobs/${id}`, { method: "DELETE" }),

  // Uploads
  createUpload: (body: { filename: string; content_type: string; size_bytes: number; sha256?: string }) =>
    apiRequest<UploadSession>("/v1/uploads", { method: "POST", body }),
  completeUpload: (id: string, parts: { part_number: number; etag: string }[]) =>
    apiRequest<Artifact>(`/v1/uploads/${id}/complete`, { method: "POST", body: { parts } }),

  // Artifacts
  listArtifacts: (q: { limit?: number; cursor?: string; content_type?: string } = {}) =>
    apiRequest<ListEnvelope<Artifact>>("/v1/artifacts", { query: q }),
  getArtifact: (id: string) => apiRequest<Artifact>(`/v1/artifacts/${id}`),
  signedUrl: (id: string, expires_in = 900) =>
    apiRequest<SignedURL>(`/v1/artifacts/${id}/signed-url`, { query: { expires_in } }),

  // Processors
  listProcessors: (limit = 100) =>
    apiRequest<ListEnvelope<ProcessorSummary>>("/v1/processors", { query: { limit } }),
  getProcessor: (name: string) => apiRequest<Processor>(`/v1/processors/${name}`),

  // Usage
  usage: () => apiRequest<Usage>("/v1/usage"),
  timeseries: (q: { granularity?: string; group_by?: string; from?: string; to?: string } = {}) =>
    apiRequest<Timeseries>("/v1/usage/timeseries", { query: q }),

  // API keys
  listKeys: (limit = 100) => apiRequest<{ data: APIKey[]; has_more: boolean }>("/v1/api-keys", { query: { limit } }),
  createKey: (body: { name: string; scopes: string[] }) =>
    apiRequest<APIKey>("/v1/api-keys", { method: "POST", body }),
  revokeKey: (id: string) => apiRequest<void>(`/v1/api-keys/${id}`, { method: "DELETE" }),

  // Webhooks
  listWebhooks: (q: { limit?: number; cursor?: string } = {}) =>
    apiRequest<ListEnvelope<WebhookEndpoint>>("/v1/webhooks", { query: q }),
  getWebhook: (id: string) => apiRequest<WebhookEndpoint>(`/v1/webhooks/${id}`),
  createWebhook: (body: { url: string; subscribed_events: string[]; description?: string; secret?: string }) =>
    apiRequest<WebhookEndpoint>("/v1/webhooks", { method: "POST", body }),
  listDeliveries: (id: string, q: { limit?: number; cursor?: string; status?: string; event_type?: string } = {}) =>
    apiRequest<ListEnvelope<WebhookDelivery>>(`/v1/webhooks/${id}/deliveries`, { query: q }),
  replayDelivery: (id: string, deliveryId: string) =>
    apiRequest<WebhookDelivery>(`/v1/webhooks/${id}/deliveries/${deliveryId}/replay`, { method: "POST" }),
  testWebhook: (id: string) => apiRequest<unknown>(`/v1/webhooks/${id}/test`, { method: "POST" }),

  // Audit
  listAudit: (q: { limit?: number; cursor?: string; action?: string; resource_type?: string } = {}) =>
    apiRequest<ListEnvelope<AuditEntry>>("/v1/audit-log", { query: q }),

  // Onboarding (admin)
  provision: (body: { org_name: string; user_email: string; key_name?: string; scopes?: string[] }) =>
    apiRequest<ProvisionResponse>("/v1/onboarding/provision", { method: "POST", body }),

  // Marketplace
  listMarketplace: (q: { trust_class?: string } = {}) =>
    apiRequest<{ data: MarketplaceProcessor[] }>("/v1/marketplace/processors", { query: q }),
  listSubmissions: (q: { status?: string } = {}) =>
    apiRequest<{ data: MarketplaceSubmission[] }>("/v1/marketplace/submissions", { query: q }),
  reviewSubmission: (id: string, decision: "approve" | "reject", notes?: string) =>
    apiRequest<{ id: string; status: string }>(`/v1/marketplace/submissions/${id}/review`, {
      method: "POST",
      body: { decision, notes },
    }),

  // Streaming
  listStreaming: () => apiRequest<{ data: StreamingSession[] }>("/v1/streaming/sessions"),
  createStreaming: (body: { model_version_id?: string } = {}) =>
    apiRequest<StreamingSession>("/v1/streaming/sessions", { method: "POST", body }),
  finalizeStreaming: (id: string, body: { transcript: string; audio_seconds: number }) =>
    apiRequest<StreamingSession>(`/v1/streaming/sessions/${id}/finalize`, { method: "POST", body }),
};

export { ALLOWED_EVENTS } from "@/lib/events";
