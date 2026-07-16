/**
 * Typed errors mapped from Orpheus RFC 7807 `problem+json` responses.
 */

import type { Problem, ErrorField } from './types';

export class OrpheusError extends Error {
  constructor(message: string) {
    super(message);
    this.name = new.target.name;
  }
}

/** The request never reached the server (network failure, DNS, TLS, timeout). */
export class OrpheusConnectionError extends OrpheusError {
  readonly cause?: unknown;
  constructor(message: string, cause?: unknown) {
    super(message);
    this.cause = cause;
  }
}

/** Any non-2xx HTTP response. */
export class OrpheusAPIError extends OrpheusError {
  readonly statusCode: number;
  readonly problem?: Problem;
  readonly requestId?: string;
  readonly headers: Record<string, string>;

  constructor(
    statusCode: number,
    problem: Problem | undefined,
    headers: Record<string, string>,
  ) {
    const title = problem?.title ?? 'HTTP error';
    const detail = problem?.detail ? `: ${problem.detail}` : '';
    super(`[${statusCode}] ${title}${detail}`);
    this.statusCode = statusCode;
    this.problem = problem;
    this.headers = headers;
    this.requestId = headers['x-request-id'];
  }

  /** Field-level validation errors, primarily on 400 responses. */
  get errors(): ErrorField[] {
    return this.problem?.errors ?? [];
  }
}

/** 400 - request validation failed; inspect `errors`. */
export class BadRequestError extends OrpheusAPIError {}
/** 401 - missing or invalid credentials. */
export class AuthenticationError extends OrpheusAPIError {}
/** 403 - authenticated but not permitted (e.g. insufficient scopes). */
export class PermissionDeniedError extends OrpheusAPIError {}
/** 404 - resource not found or not visible to the org. */
export class NotFoundError extends OrpheusAPIError {}
/** 409 - state conflict, e.g. idempotency-key reuse with a different body. */
export class ConflictError extends OrpheusAPIError {}
/** 413 - the requested upload exceeds the org's maximum artifact size. */
export class PayloadTooLargeError extends OrpheusAPIError {}
/** 5xx - server-side failure. */
export class ServerError extends OrpheusAPIError {}

/** 429 - rate limit exceeded. */
export class RateLimitError extends OrpheusAPIError {
  /** Seconds to wait before retrying, from the `Retry-After` header. */
  get retryAfter(): number | undefined {
    const raw = this.headers['retry-after'];
    if (raw === undefined) return undefined;
    const n = Number.parseInt(raw, 10);
    return Number.isNaN(n) ? undefined : n;
  }
}

const STATUS_MAP: Record<number, new (...a: never[]) => OrpheusAPIError> = {
  400: BadRequestError,
  401: AuthenticationError,
  403: PermissionDeniedError,
  404: NotFoundError,
  409: ConflictError,
  413: PayloadTooLargeError,
  429: RateLimitError,
};

function headersToObject(headers: Headers): Record<string, string> {
  const out: Record<string, string> = {};
  headers.forEach((value, key) => {
    out[key.toLowerCase()] = value;
  });
  return out;
}

function isProblem(body: unknown): body is Problem {
  return (
    typeof body === 'object' &&
    body !== null &&
    'title' in body &&
    'status' in body
  );
}

export function errorFromResponse(
  statusCode: number,
  body: unknown,
  headers: Headers,
): OrpheusAPIError {
  const problem = isProblem(body) ? body : undefined;
  const headerObj = headersToObject(headers);
  const Ctor =
    STATUS_MAP[statusCode] ?? (statusCode >= 500 ? ServerError : OrpheusAPIError);
  return new (Ctor as new (
    s: number,
    p: Problem | undefined,
    h: Record<string, string>,
  ) => OrpheusAPIError)(statusCode, problem, headerObj);
}
