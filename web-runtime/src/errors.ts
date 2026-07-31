// Part of @reliant-labs/web-runtime — the web twin of forge/pkg.
//
// Typed client errors. Connect surfaces failures as ConnectError with a
// numeric Code; the rest of the app should reason about a stable, typed
// shape instead of sniffing status codes at every call site. normalizeError
// maps any thrown value into a ConnectClientError carrying the canonical
// code name, an HTTP-ish status, a retryability verdict, and a
// human-presentable message.
import { Code, ConnectError } from "@connectrpc/connect";

/**
 * FORGE_ERROR_REASON_HEADER is the Connect trailer/metadata key carrying a
 * stable, machine-readable reason code (snake_case) the backend sets for
 * every error. UI/routing keys off `error.reason` instead of matching
 * human-readable message strings — the message text is display-only and
 * may change. The backend owns the setter; the frontend only reads it.
 */
export const FORGE_ERROR_REASON_HEADER = "x-forge-error-reason";

/**
 * stripServerFraming removes the transport framing that must never surface
 * to a user: the leading "[code] " gRPC-code prefix connect-es puts on
 * `ConnectError.message`. What survives is the message the server wrote,
 * which is exactly what a user should read.
 *
 * This is the SINGLE implementation — a project never wants a different
 * answer here, and a copy frozen into a scaffold could never be corrected
 * once shipped. Idempotent: framing that has already been stripped strips
 * to itself.
 */
export function stripServerFraming(message: string): string {
  return message.replace(/^\[[a-z_]+\]\s*/i, "").trim();
}

/** The copy shown when an error carries no presentable message of its own. */
const DEFAULT_USER_MESSAGE = "Something went wrong. Please try again.";

/**
 * userMessage — turn any thrown value into copy fit for end users.
 *
 * `ConnectError.message` prefixes the gRPC code ("[not_found] no such task");
 * `rawMessage` is the server's human-readable text without that framing, and
 * stripServerFraming drops the prefix from whichever of the two is available.
 * Use this everywhere an error is SHOWN (banners, toasts) instead of
 * `err.message`.
 *
 * Pass `fallback` to override the default copy for a surface that wants its
 * own wording.
 */
export function userMessage(err: unknown, fallback: string = DEFAULT_USER_MESSAGE): string {
  if (err instanceof ConnectError) {
    return stripServerFraming(err.rawMessage || err.message) || fallback;
  }
  if (err instanceof Error) {
    return stripServerFraming(err.message) || fallback;
  }
  return err == null ? fallback : stripServerFraming(String(err)) || fallback;
}

/** Connect codes worth a transparent transport-level retry. */
const RETRYABLE = new Set<Code>([
  Code.Unavailable,
  Code.DeadlineExceeded,
  Code.Aborted,
  Code.ResourceExhausted,
]);

/** Rough Connect-code → HTTP-status mapping for display/telemetry only. */
const HTTP_STATUS: Partial<Record<Code, number>> = {
  [Code.InvalidArgument]: 400,
  [Code.Unauthenticated]: 401,
  [Code.PermissionDenied]: 403,
  [Code.NotFound]: 404,
  [Code.AlreadyExists]: 409,
  [Code.ResourceExhausted]: 429,
  [Code.Unimplemented]: 501,
  [Code.Unavailable]: 503,
  [Code.DeadlineExceeded]: 504,
  [Code.Internal]: 500,
};

/**
 * isConnectClientErrorShape — the STRUCTURAL test behind
 * {@link ConnectClientError}'s `instanceof`.
 *
 * A class object is per module instance, not per package. npm nests a
 * transitive dependency on any version conflict, and a bundler can pull one
 * package in as both ESM and CJS, so a tree holding two copies of this
 * package holds two distinct ConnectClientError classes. Matching on shape
 * instead of identity is what makes the two agree: the shape is the
 * contract, the constructor is not.
 */
function isConnectClientErrorShape(v: unknown): v is ConnectClientError {
  if (!(v instanceof Error)) return false;
  if (Object.getPrototypeOf(v) === ConnectClientError.prototype) return true;
  const e = v as Partial<ConnectClientError>;
  return (
    v.name === "ConnectClientError" &&
    typeof e.code === "string" &&
    typeof e.status === "number" &&
    typeof e.retryable === "boolean" &&
    (typeof e.reason === "string" || e.reason === null)
  );
}

/**
 * ConnectClientError is the single typed error shape the app catches. It is
 * a plain Error subclass so it works with React Query, error boundaries, and
 * `instanceof` alike.
 */
export class ConnectClientError extends Error {
  /** Canonical Connect code name, e.g. "unavailable", "not_found". */
  readonly code: string;
  /** Numeric Connect code (Code enum), or undefined for non-Connect errors. */
  readonly connectCode?: Code;
  /** Approximate HTTP status, for telemetry and generic UI. */
  readonly status: number;
  /** True when a transparent retry is reasonable (transient failures). */
  readonly retryable: boolean;
  /**
   * Stable machine-readable reason code from the backend
   * (x-forge-error-reason metadata), or null. Key UI/routing off THIS,
   * never off `message` — message text is display-only.
   */
  readonly reason: string | null;

  constructor(args: {
    message: string;
    code: string;
    connectCode?: Code;
    status: number;
    retryable: boolean;
    reason?: string | null;
    cause?: unknown;
  }) {
    super(args.message, { cause: args.cause });
    this.name = "ConnectClientError";
    this.code = args.code;
    this.connectCode = args.connectCode;
    this.status = args.status;
    this.retryable = args.retryable;
    this.reason = args.reason ?? null;
  }

  /**
   * `instanceof` matches on SHAPE, not on class identity — so every check
   * spelled `err instanceof ConnectClientError`, here and in app code, keeps
   * working when two copies of this package end up in one bundle (see
   * {@link isConnectClientErrorShape}). Without it, normalizeError's
   * pass-through guard misses an already-normalized error from the other
   * copy; that error's `name` is "ConnectClientError" rather than
   * "ConnectError", so it misses the ConnectError branch too and is re-wrapped
   * through the generic Error branch — silently downgrading every error in the
   * app to code "unknown", status 500, no connectCode and no reason.
   * @connectrpc/connect does the same on ConnectError, for the same reason.
   */
  static [Symbol.hasInstance](v: unknown): boolean {
    return isConnectClientErrorShape(v);
  }
}

/** isRetryable reports whether a Connect code merits a transport retry. */
export function isRetryableCode(code: Code): boolean {
  return RETRYABLE.has(code);
}

/**
 * normalizeError coerces any thrown value into a ConnectClientError. Already-
 * normalized errors pass through unchanged so the interceptor chain never
 * double-wraps.
 *
 * Both `instanceof` checks below match on SHAPE rather than class identity —
 * ours via ConnectClientError's Symbol.hasInstance, ConnectError's via the one
 * @connectrpc/connect ships — so neither branch is missed when a second copy
 * of either package lands in the bundle.
 */
export function normalizeError(err: unknown): ConnectClientError {
  if (err instanceof ConnectClientError) {
    return err;
  }

  if (err instanceof ConnectError) {
    return new ConnectClientError({
      // Strip the "[code] " prefix so the message is user-presentable at
      // every layer.
      message: stripServerFraming(err.rawMessage || err.message),
      code: Code[err.code]?.toLowerCase() ?? "unknown",
      connectCode: err.code,
      status: HTTP_STATUS[err.code] ?? 500,
      retryable: isRetryableCode(err.code),
      // Stable machine reason from backend metadata (backend owns the set).
      reason: err.metadata.get(FORGE_ERROR_REASON_HEADER) || null,
      cause: err,
    });
  }

  if (err instanceof Error) {
    // A network fault (fetch rejected: offline, DNS, CORS preflight, TLS)
    // surfaces as a TypeError — that, and only that, is a safe transparent
    // retry. A generic Error is NOT blanket-retried (blanket-retrying every
    // bare Error widened the blast radius to non-idempotent failures).
    const networkFault = err instanceof TypeError;
    return new ConnectClientError({
      message: stripServerFraming(err.message),
      code: networkFault ? "unavailable" : "unknown",
      status: networkFault ? 503 : 500,
      retryable: networkFault,
      reason: null,
      cause: err,
    });
  }

  return new ConnectClientError({
    message: typeof err === "string" ? stripServerFraming(err) : "Unknown error",
    code: "unknown",
    status: 500,
    retryable: false,
    reason: null,
    cause: err,
  });
}
