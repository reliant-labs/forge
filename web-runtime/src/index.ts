// @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// Public barrel. Import batteries from "@reliantlabs/forge-web-runtime".
//
// Everything here is MECHANISM: the transport interceptor stack, the session,
// the error boundary, the toast queue, the <Resource> tristate ladder, W3C
// trace propagation, client RUM. Nothing here reaches into an application —
// app-shaped values (auth state, event subscriptions, toast markup) arrive as
// props, so an app can restyle or replace its own files freely.
//
// Presentation that a project is meant to own — the component library under
// src/components/ui, globals.css, nav, layout — deliberately stays in the
// scaffold and is NOT part of this package.

// The per-group docs below are JSDoc, not `//`, on purpose: TypeScript's
// declaration emit preserves JSDoc and DROPS line comments, and dist/ is now
// the whole of what this package publishes. `forge project libraries` reads
// this inventory out of the published barrel, and a consumer's editor shows
// it on hover — both would get symbols with no explanation otherwise.

/** Transport interceptor stack. */
export {
  authInterceptor,
  buildRuntimeInterceptors,
  errorNormalizeInterceptor,
  headerInterceptor,
  retryInterceptor,
  traceInterceptor,
  type RuntimeInterceptorConfig,
} from "./interceptors.js";

/**
 * Typed client errors, and the backend-framing strippers every surface that
 * SHOWS an error goes through.
 */
export {
  ConnectClientError,
  FORGE_ERROR_REASON_HEADER,
  isRetryableCode,
  normalizeError,
  stripServerFraming,
  userMessage,
} from "./errors.js";

/**
 * Mount-prefix helpers, for the URLs Next.js's router never sees — absolute
 * URLs handed to external systems that round-trip back into the app.
 *
 * Barrel-exported rather than behind a subpath: three string functions with
 * no imports, so there is no dependency footprint for a subpath to fence off.
 */
export {
  createBasePath,
  joinBasePath,
  normalizeBasePath,
  type BasePathHelpers,
} from "./basepath.js";

/** W3C trace-context propagation. */
export {
  freshTraceparent,
  injectTraceContext,
  setTraceSampled,
  TRACEPARENT_RE,
} from "./trace.js";

/** Session: the signed-in user, derived from the app's auth state and JWT claims. */
export {
  decodeJwt,
  SessionProvider,
  useSession,
  type JwtClaims,
  type Session,
  type SessionAuth,
} from "./session.js";

/** The app-shell provider set: session + error boundary + toast host in one. */
export { RuntimeShell, type RuntimeShellProps } from "./providers.js";

/** Error boundary with a designed crash fallback, for a route or a subtree. */
export {
  DefaultErrorFallback,
  RuntimeErrorBoundary,
} from "./error-boundary.js";

/** The toast queue. The app supplies the event source and the markup. */
export {
  RuntimeToastHost,
  type RuntimeToast,
  type RuntimeToastHostProps,
  type RuntimeToastPayload,
  type RuntimeToastSink,
  type RuntimeToastVariant,
} from "./toast-host.js";

/** Route guarding: render a page only to a signed-in caller. */
export { RouteGuard, type RouteGuardProps } from "./route-guard.js";

/** Generic data-table container: the tristate ladder, filter and cursor pagination. */
export {
  Resource,
  type ResourceColumn,
  type ResourceProps,
  type ResourceStatus,
} from "./resource.js";

/** Client RUM (opt-in export). */
export {
  initClientTelemetry,
  type TelemetryConfig,
  type TelemetryEvent,
} from "./telemetry.js";
