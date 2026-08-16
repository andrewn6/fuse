// Error types for the Fuse SDK, mirroring sdks/go/errors.go: a structured
// FuseApiError for non-2xx responses plus code-checking helpers, and a
// FuseError for transport/configuration failures.

const REQUEST_ID_HEADER = "X-Request-ID";

/** Stable, machine-readable error codes returned by the server. */
export const ErrorCode = {
  NotFound: "not_found",
  Conflict: "conflict",
  InvalidArgument: "invalid_argument",
  Unavailable: "unavailable",
  Internal: "internal",
  Unauthorized: "unauthorized",
} as const;

export type ErrorCode = (typeof ErrorCode)[keyof typeof ErrorCode];

const STATUS_TEXT: Record<number, string> = {
  400: "bad request",
  401: "unauthorized",
  403: "forbidden",
  404: "not found",
  409: "conflict",
  500: "internal server error",
  502: "bad gateway",
  503: "service unavailable",
};

function statusText(status: number): string {
  return STATUS_TEXT[status] ?? `status ${status}`;
}

export interface FuseApiErrorInit {
  status: number;
  code?: string;
  message?: string;
  details?: Record<string, string>;
  requestId?: string;
  body?: string;
}

function formatMessage(init: FuseApiErrorInit): string {
  const parts: string[] = [`status=${init.status}`];
  if (init.code) parts.push(`code=${init.code}`);
  if (init.message) parts.push(init.message);
  else parts.push(statusText(init.status));
  if (init.requestId) parts.push(`request_id=${init.requestId}`);
  return "fuse api error: " + parts.join(", ");
}

/** FuseApiError is a non-2xx response from the server. */
export class FuseApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: Record<string, string>;
  readonly requestId?: string;
  /** Raw response body, when available. */
  readonly body?: string;

  constructor(init: FuseApiErrorInit) {
    super(formatMessage(init));
    this.name = "FuseApiError";
    this.status = init.status;
    this.code = init.code ?? "";
    this.details = init.details;
    this.requestId = init.requestId;
    this.body = init.body;
    // Restore the prototype chain so `instanceof` works after transpilation.
    Object.setPrototypeOf(this, FuseApiError.prototype);
  }
}

/** FuseError is a transport, configuration, or decoding failure (not an API error). */
export class FuseError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "FuseError";
    Object.setPrototypeOf(this, FuseError.prototype);
  }
}

/** isFuseApiError is a type guard for FuseApiError. */
export function isFuseApiError(err: unknown): err is FuseApiError {
  return err instanceof FuseApiError;
}

function hasCode(err: unknown, code: string): boolean {
  return isFuseApiError(err) && err.code === code;
}

/** isNotFound reports whether err is a not_found api error. */
export function isNotFound(err: unknown): boolean {
  return hasCode(err, ErrorCode.NotFound);
}

/** isConflict reports whether err is a conflict api error. */
export function isConflict(err: unknown): boolean {
  return hasCode(err, ErrorCode.Conflict);
}

/** isUnauthorized reports whether err is an unauthorized api error. */
export function isUnauthorized(err: unknown): boolean {
  return hasCode(err, ErrorCode.Unauthorized);
}

/** isInvalidArgument reports whether err is an invalid_argument api error. */
export function isInvalidArgument(err: unknown): boolean {
  return hasCode(err, ErrorCode.InvalidArgument);
}

/** isUnavailable reports whether err is an unavailable api error. */
export function isUnavailable(err: unknown): boolean {
  return hasCode(err, ErrorCode.Unavailable);
}

interface ErrorEnvelope {
  error?: {
    code?: string;
    message?: string;
    details?: Record<string, string>;
  };
}

/**
 * parseApiError builds a FuseApiError from a status, optional request id, and
 * raw body, parsing the `{"error":{code,message,details}}` envelope when
 * present. Used directly by paths that do not have a fetch Response (the
 * upgrade transport reads node http responses).
 */
export function parseApiError(
  status: number,
  requestId: string | undefined,
  body: string,
): FuseApiError {
  if (body) {
    try {
      const env = JSON.parse(body) as ErrorEnvelope;
      const e = env?.error;
      if (e && (e.code || e.message)) {
        return new FuseApiError({
          status,
          code: e.code,
          message: e.message,
          details: e.details,
          requestId,
          body,
        });
      }
    } catch {
      // fall through to the status-text error below
    }
  }
  return new FuseApiError({ status, message: statusText(status), requestId, body });
}

/**
 * errorFromResponse builds a FuseApiError from a non-2xx Response, parsing the
 * `{"error":{code,message,details}}` envelope and reading the X-Request-ID
 * response header for correlation.
 */
export async function errorFromResponse(res: Response): Promise<FuseApiError> {
  const requestId = res.headers.get(REQUEST_ID_HEADER) ?? undefined;
  let body = "";
  try {
    body = await res.text();
  } catch (err) {
    return new FuseApiError({
      status: res.status,
      message: `read error body: ${String(err)}`,
      requestId,
    });
  }
  return parseApiError(res.status, requestId, body);
}
