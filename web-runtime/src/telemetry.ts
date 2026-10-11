// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// Browser telemetry: the HyperDX browser SDK, initialised the forge way.
//
// Captures errors (uncaught, unhandled rejections, boundary-caught), console
// output, fetch/XHR spans, document load, long tasks and web vitals, and
// ships them to ClickStack through a SAME-ORIGIN `/_otel` route.
//
// ── Why HyperDX REPLACES the old OTel web wiring, not joins it ───────────
//
// @hyperdx/browser wraps @hyperdx/otel-web, which builds its own
// WebTracerProvider and calls provider.register() — the global tracer
// provider, context manager and propagator. A second registration (the old
// `./otel` subpath did exactly that) is rejected by @opentelemetry/api with a
// "duplicate registration" diagnostic and silently loses: whichever ran
// second would simply never see a span. Two tracer providers cannot coexist
// in one page, so there is one, and it is HyperDX's. `./otel` and its eight
// @opentelemetry/* SDK peer dependencies are gone.
//
// traceparent on RPCs is unaffected. traceInterceptor (trace.ts) injects the
// active span's context into every Connect call, or synthesises a valid
// traceparent when none is active — with no SDK at all. With the SDK running,
// the fetch span HyperDX opens for the call carries the same trace id, and
// `apiOrigins` below makes that hold for a cross-origin API too (which is
// every dev loop: the API binds its own port).
//
// ── The browser holds no secret ──────────────────────────────────────────
//
// `url` is `${location.origin}/_otel`. The browser talks only to its own
// origin; a dev-server proxy (next.config.ts rewrites / Vite server.proxy) or
// the deployment's ingress forwards `/_otel/v1/{traces,logs}` to the OTLP/HTTP
// port of a collector, and that hop — never the bundle — owns credentials.
//
// The SDK nevertheless wants an `apiKey` and sends it as an `authorization`
// header. We pass PLACEHOLDER_API_KEY, a constant that grants nothing and is
// public by construction. A collector that enforces ingestion auth (ClickStack
// `collectorAuthenticationEnforced`) rejects it with 401, so the proxy in
// front of such a collector must strip the header or replace it with the real
// key. The local ClickStack collector does not enforce, and ignores it.
//
// ── Replay: shipped, off, masked ─────────────────────────────────────────
//
// The SDK bundles an rrweb recorder. It is DISABLED unless `replay: true`.
// When enabled, masking is forced on — every input and all text — because a
// replay records the DOM, which for most apps is the most sensitive data they
// have. There is deliberately no option to turn masking off while replay is
// on; an app that needs unmasked replay passes its own SDK options.
//
// ── Loading ──────────────────────────────────────────────────────────────
//
// The SDK is ~600 KB minified (rrweb and the OTel web stack are bundled in
// it), so it is imported lazily: bundlers split it into its own chunk that
// loads after the app is interactive and costs the first paint nothing.
// initBrowserTelemetry is therefore async and idempotent — every caller gets
// the same promise, which is what survives React StrictMode's double mount and
// Fast Refresh. It is a no-op without a DOM (SSR, prerender) and without an
// endpoint, and it never throws.
import { trace } from "@opentelemetry/api";

import { setExceptionReporter } from "./error-reporter.js";

/** Where the browser ships telemetry: this origin's `/_otel`. */
export const DEFAULT_INGEST_PATH = "/_otel";

/**
 * The apiKey handed to the SDK. Public by construction: it identifies
 * nothing and authorises nothing. See the header — the proxy or collector
 * owns the real credential.
 */
export const PLACEHOLDER_API_KEY = "forge-browser-public";

export interface BrowserTelemetryConfig {
  /** `service.name` on everything this page emits. */
  serviceName: string;
  /** `service.version`. Omit for none. */
  serviceVersion?: string;
  /**
   * The forge environment name. Emitted as BOTH `deployment.environment.name`
   * (current semconv) and `deployment.environment` (the legacy key HyperDX's
   * default sources still key on).
   */
  environment?: string;
  /** `service.namespace`, conventionally the project name. */
  serviceNamespace?: string;
  /**
   * Ingest base: a path on this origin (`/_otel`) or, as an escape hatch, an
   * absolute URL. Unset, empty, or `off` disables telemetry — the app runs
   * untraced rather than throwing, which is what makes it safe to call
   * unconditionally from the app shell.
   */
  endpoint?: string;
  /**
   * Origins (or URLs — only the origin is used) of the app's own backend.
   * Requests to them always get a `traceparent` from the SDK's fetch/XHR
   * spans. This page's own origin is always included.
   */
  apiOrigins?: Array<string | undefined>;
  /** Mirror console.* into telemetry. Default true. */
  consoleCapture?: boolean;
  /**
   * Capture full request/response headers AND BODIES on fetch/XHR spans.
   * Default false, and it should stay false: bodies are user data.
   */
  advancedNetworkCapture?: boolean;
  /** Session replay (rrweb). Default false. When true, all text and inputs are masked. */
  replay?: boolean;
  /** Verbose SDK diagnostics to the console. Default false. */
  debug?: boolean;
}

/** The slice of the SDK this module calls. Typed here, not imported, so the module loads without it. */
interface HyperDXClient {
  init(config: Record<string, unknown>): void;
  recordException(error: unknown, attributes?: Record<string, unknown>): void;
}

let started: Promise<boolean> | null = null;

/**
 * initBrowserTelemetry starts the HyperDX browser SDK once per page load and
 * resolves to whether it is running. See the module header for the contract.
 */
export function initBrowserTelemetry(
  config: BrowserTelemetryConfig,
): Promise<boolean> {
  if (typeof window === "undefined") {
    return Promise.resolve(false);
  }
  if (started) {
    return started;
  }
  const ingest = resolveIngestUrl(config.endpoint, window.location.origin);
  if (!ingest) {
    return Promise.resolve(false);
  }
  started = start(config, ingest);
  return started;
}

async function start(
  config: BrowserTelemetryConfig,
  ingest: string,
): Promise<boolean> {
  try {
    const hdx = resolveClient(await import("@hyperdx/browser"));
    if (!hdx) {
      return false;
    }
    hdx.init(buildSdkOptions(config, ingest, window.location.origin));
    tapSdkInputs();
    setExceptionReporter((error, attributes) => {
      if (alreadyRecorded(error)) {
        return;
      }
      hdx.recordException(error, attributes);
    });
    return true;
  } catch (err) {
    // Blocked chunk, ad-blocker, SDK throw: telemetry must never break the app.
    console.debug("[telemetry] disabled:", err);
    return false;
  }
}

// ── One failure, one record ──────────────────────────────────────────────
//
// The SDK records an Error that reaches window "error", window
// "unhandledrejection", or console.error. React (18 and 19) console.errors an
// error a boundary catches BEFORE it calls componentDidCatch, and Next
// dispatches an uncaught render error to window "error" — so by the time
// reportException runs, the SDK has usually recorded that very error already.
// Recording it again would count one failure twice, and the issue layer
// (which groups by fingerprint) would report two.
//
// So the taps below remember which Error OBJECTS the SDK was shown, and the
// installed recorder skips those. Whatever the SDK did not see — a custom
// onCaughtError that silences React's logging, an error rethrown as a new
// object — is still recorded. The console tap is installed after the SDK's own
// wrapper, so it sits outermost and only observes; every call is forwarded
// untouched.
//
// The cost is the React component stack: the SDK's record of a boundary-caught
// error comes from console.error(error), which carries the error and its
// stack but not React's componentStack, and attributes passed to a skipped
// reportException are dropped with it.
const seenBySdk = new WeakSet<object>();
let tapped = false;

function noteSeen(value: unknown): void {
  if (typeof value === "object" && value !== null) {
    seenBySdk.add(value);
  }
}

function tapSdkInputs(): void {
  if (tapped) {
    return;
  }
  tapped = true;
  const wrapped = console.error;
  console.error = function (this: unknown, ...args: unknown[]) {
    for (const arg of args) {
      if (arg instanceof Error) {
        noteSeen(arg);
      }
    }
    return wrapped.apply(this, args);
  };
  window.addEventListener("error", (e) => noteSeen(e.error), true);
  window.addEventListener(
    "unhandledrejection",
    (e) => noteSeen(e.reason),
    true,
  );
}

function alreadyRecorded(error: unknown): boolean {
  return typeof error === "object" && error !== null && seenBySdk.has(error);
}

/**
 * The options passed to HyperDX.init. Exported for tests and for apps that
 * want to see exactly what is configured — it is pure.
 */
export function buildSdkOptions(
  config: BrowserTelemetryConfig,
  ingestUrl: string,
  pageOrigin: string,
): Record<string, unknown> {
  const replay = config.replay === true;
  const resourceAttributes: Record<string, string> = {};
  if (config.serviceVersion) {
    resourceAttributes["service.version"] = config.serviceVersion;
  }
  if (config.serviceNamespace) {
    resourceAttributes["service.namespace"] = config.serviceNamespace;
  }
  if (config.environment) {
    resourceAttributes["deployment.environment.name"] = config.environment;
    resourceAttributes["deployment.environment"] = config.environment;
  }

  const options: Record<string, unknown> = {
    apiKey: PLACEHOLDER_API_KEY,
    service: config.serviceName,
    url: ingestUrl,
    tracePropagationTargets: propagationTargets(pageOrigin, config.apiOrigins),
    // The SDK would otherwise trace its own exports, and it must never
    // observe the route it ships through.
    ignoreUrls: [new RegExp(`^${escapeRegExp(ingestUrl)}`)],
    consoleCapture: config.consoleCapture ?? true,
    advancedNetworkCapture: config.advancedNetworkCapture ?? false,
    // Polls for window.Intercom on a timer; nothing here uses Intercom.
    disableIntercom: true,
    disableReplay: !replay,
    otelResourceAttributes: resourceAttributes,
    debug: config.debug ?? false,
  };
  if (replay) {
    // The SDK's own default masks inputs but NOT text; text is where chat
    // content and records live. Force both.
    options.maskAllInputs = true;
    options.maskAllText = true;
    options.recordCanvas = false;
  }
  return options;
}

/**
 * resolveIngestUrl turns the configured endpoint into the absolute URL the SDK
 * is given, or null when telemetry is off. A path resolves against the page
 * origin, so the default is same-origin by construction.
 */
export function resolveIngestUrl(
  endpoint: string | undefined,
  pageOrigin: string,
): string | null {
  const value = (endpoint ?? "").trim();
  if (value === "" || value.toLowerCase() === "off") {
    return null;
  }
  try {
    return new URL(value, pageOrigin).toString().replace(/\/+$/, "");
  } catch {
    return null;
  }
}

function propagationTargets(
  pageOrigin: string,
  apiOrigins: Array<string | undefined> = [],
): RegExp[] {
  const origins = new Set<string>([pageOrigin]);
  for (const raw of apiOrigins) {
    if (!raw) {
      continue;
    }
    try {
      origins.add(new URL(raw, pageOrigin).origin);
    } catch {
      // An unparsable origin is not a target.
    }
  }
  return [...origins].map((o) => new RegExp(`^${escapeRegExp(o)}(?:[/?#]|$)`));
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// @hyperdx/browser is a UMD/CJS bundle whose module.exports IS the client, and
// its methods use `this`. A bundler hands back a module NAMESPACE that exposes
// both the client's members at the top level (as read-only getters) and the
// client itself as `default`. Calling init on the namespace binds `this` to that
// frozen object and the SDK throws on its first property write, which disabled
// telemetry silently. So descend to the deepest `default` that is still a
// client, the real instance, and never settle for the namespace in front of it.
function resolveClient(mod: unknown): HyperDXClient | null {
  let cur: unknown = mod;
  for (let depth = 0; depth < 4; depth++) {
    const next = peek(cur, "default");
    if (!isClient(next)) {
      break;
    }
    cur = next;
  }
  return isClient(cur) ? cur : null;
}

function isClient(v: unknown): v is HyperDXClient {
  return (
    typeof peek(v, "init") === "function" &&
    typeof peek(v, "recordException") === "function"
  );
}

// A module namespace object may throw on an absent key (strict ESM
// interop shims and test doubles do), and "absent" is an answer here.
function peek(obj: unknown, key: string): unknown {
  try {
    return (obj as Record<string, unknown>)[key];
  } catch {
    return undefined;
  }
}

/** The tracer to open manual spans on. Resolves HyperDX's provider once it is running. */
export function getTracer(name = "default") {
  return trace.getTracer(name);
}

export { context, propagation, trace } from "@opentelemetry/api";
