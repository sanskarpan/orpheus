/**
 * Webhook event enum — client-safe (no server-only imports), so it can be used
 * in client components like the webhook create form.
 * Mirrors `allowedEvents` in apps/api/internal/handlers/webhooks.go.
 */
export const ALLOWED_EVENTS = [
  "job.queued",
  "job.started",
  "job.completed",
  "job.failed",
  "job.retry",
  "job.dead_letter",
  "job.canceled",
  "bundle.ready",
  "bundle.failed",
  "batch.completed",
  "usage.budget_threshold",
  "data.erased",
  "upload.completed",
  "upload.failed",
  "api_key.created",
  "api_key.revoked",
  "billing.period_closed",
] as const;
