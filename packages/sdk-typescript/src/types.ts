/**
 * Type definitions for the Orpheus API, generated from the OpenAPI spec.
 * These mirror the response and request shapes at `apps/api/internal/handlers/openapi.json`.
 */

// ---------------------------------------------------------------------------
// Enums (as string unions)
// ---------------------------------------------------------------------------

export type UploadStatus = 'pending' | 'completed' | 'aborted' | 'expired';

export type JobStatus =
  | 'queued'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'canceled'
  | 'canceling';

export type WebhookEvent =
  | 'job.queued'
  | 'job.started'
  | 'job.succeeded'
  | 'job.failed'
  | 'job.canceled'
  | 'upload.completed'
  | 'upload.failed'
  | 'api_key.created'
  | 'api_key.revoked'
  | 'billing.period_closed'
  | '*';

export type WebhookDeliveryStatus = 'pending' | 'succeeded' | 'failed' | 'retrying';

export type APIKeyScope =
  | 'uploads:read'
  | 'uploads:write'
  | 'artifacts:read'
  | 'artifacts:write'
  | 'jobs:read'
  | 'jobs:write'
  | 'webhooks:read'
  | 'webhooks:write'
  | 'usage:read'
  | 'audit:read'
  | '*';

export type ProcessorVersionStatus = 'active' | 'beta' | 'deprecated' | 'sunset';

// ---------------------------------------------------------------------------
// Problem / errors (RFC 7807)
// ---------------------------------------------------------------------------

export interface ErrorField {
  field: string;
  code: string;
  message: string;
}

export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  errors?: ErrorField[];
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

export interface Page<T> {
  data: T[];
  has_more: boolean;
  next_cursor?: string | null;
}

export interface ListParams {
  limit?: number;
  cursor?: string;
}

// ---------------------------------------------------------------------------
// Uploads & artifacts
// ---------------------------------------------------------------------------

export interface CreateUploadRequest {
  filename: string;
  content_type: string;
  size_bytes: number;
  sha256?: string;
}

export interface Part {
  part_number: number;
  url: string;
  expires_at: string;
}

export interface UploadSession {
  id: string;
  status: UploadStatus;
  part_size: number;
  parts: Part[];
  expires_at: string;
  created_at: string;
  fields?: Record<string, string>;
}

export interface CompletedPart {
  part_number: number;
  etag: string;
}

export interface Artifact {
  id: string;
  sha256: string;
  size_bytes: number;
  content_type: string;
  codec: string;
  duration_seconds: number;
  sample_rate: number;
  channels: number;
  filename?: string;
  created_at: string;
}

export interface SignedURL {
  url: string;
  expires_at: string;
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

export interface ProcessorRef {
  name: string;
  version: string;
}

export interface CreateJobRequest {
  artifact_id: string;
  processor: ProcessorRef;
  params?: Record<string, unknown>;
  priority?: number;
  idempotency_key?: string;
}

export interface JobError {
  code?: string;
  message?: string;
}

export interface Job {
  id: string;
  artifact_id: string;
  processor: ProcessorRef;
  status: JobStatus;
  params?: Record<string, unknown>;
  result?: Record<string, unknown> | null;
  error?: JobError | null;
  attempts: number;
  max_retries: number;
  cost_usd?: number;
  started_at?: string | null;
  completed_at?: string | null;
  created_at: string;
  updated_at: string;
  poll_url?: string;
}

export interface BulkRejection {
  index: number;
  reason: string;
  code?: string;
}

export interface BulkJobsResponse {
  batch_id: string;
  accepted: string[];
  rejected: BulkRejection[];
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

export interface CreateWebhookRequest {
  url: string;
  subscribed_events: WebhookEvent[];
  description?: string;
  secret?: string;
}

export interface UpdateWebhookRequest {
  url?: string;
  subscribed_events?: WebhookEvent[];
  description?: string;
  active?: boolean;
}

export interface WebhookEndpoint {
  id: string;
  url: string;
  subscribed_events: WebhookEvent[];
  description?: string;
  active: boolean;
  secret?: string;
  created_at: string;
  updated_at?: string;
}

export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  event_id: string;
  event_type: WebhookEvent;
  status: WebhookDeliveryStatus;
  attempt_count: number;
  last_attempt_at?: string | null;
  last_status_code?: number | null;
  last_error?: string | null;
  payload_url?: string;
  created_at: string;
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

export interface CreateAPIKeyRequest {
  name: string;
  scopes: APIKeyScope[];
  expires_at?: string | null;
}

export interface APIKey {
  id: string;
  name: string;
  prefix: string;
  secret?: string | null;
  scopes: APIKeyScope[];
  expires_at?: string | null;
  last_used_at?: string | null;
  created_at: string;
}

// ---------------------------------------------------------------------------
// Processors ("workflows" catalog)
// ---------------------------------------------------------------------------

export interface ProcessorVersion {
  version: string;
  status: ProcessorVersionStatus;
  released_at?: string;
  deprecated_at?: string | null;
  sunset_at?: string | null;
  input_schema?: Record<string, unknown>;
  params_schema: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
  default_params?: Record<string, unknown>;
  gpu_memory_mb?: number;
  cost_per_second_usd?: number;
  max_audio_duration_seconds?: number | null;
}

export interface Processor {
  name: string;
  display_name: string;
  description?: string;
  category?: string;
  versions: ProcessorVersion[];
}

export interface ProcessorSummary {
  name: string;
  display_name: string;
  description?: string;
  category?: string;
  latest_version: string;
  active_versions: string[];
}

// ---------------------------------------------------------------------------
// Usage & audit log
// ---------------------------------------------------------------------------

export interface UsagePeriod {
  start: string;
  end: string;
}

export interface UsageBreakdown {
  category: 'compute' | 'storage' | 'egress' | 'ingest' | 'other';
  quantity?: number;
  unit?: 'seconds' | 'gb_days' | 'gb' | 'bytes' | 'count';
  amount_usd: number;
}

export interface Usage {
  org_id: string;
  period: UsagePeriod;
  jobs_count: number;
  gpu_seconds: number;
  storage_gb_days: number;
  egress_gb: number;
  total_usd: number;
  breakdown?: UsageBreakdown[];
}

export interface AuditLog {
  id: string;
  org_id: string;
  actor_id?: string | null;
  actor_type: 'user' | 'api_key' | 'system';
  action: string;
  resource_type: 'upload' | 'artifact' | 'job' | 'webhook' | 'api_key' | 'processor' | 'org';
  resource_id?: string | null;
  ip?: string | null;
  user_agent?: string | null;
  metadata?: Record<string, unknown> | null;
  created_at: string;
}

// ---------------------------------------------------------------------------
// Query-param shapes
// ---------------------------------------------------------------------------

export interface ListUploadsParams extends ListParams {
  status?: UploadStatus;
  created_after?: string;
  created_before?: string;
}

export interface ListArtifactsParams extends ListParams {
  content_type?: string;
  created_after?: string;
  created_before?: string;
}

export interface ListJobsParams extends ListParams {
  status?: JobStatus;
  processor?: string;
  artifact_id?: string;
  created_after?: string;
  created_before?: string;
}

export interface ListDeliveriesParams extends ListParams {
  event_type?: string;
  status?: WebhookDeliveryStatus;
}

export interface ListAuditLogParams extends ListParams {
  action?: string;
  actor_id?: string;
  resource_type?: string;
  created_after?: string;
  created_before?: string;
}
