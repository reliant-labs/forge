// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// Which trace an outgoing request joins while an interaction is open.
//
// ── Why a registry and not context.with ──────────────────────────────────
//
// A browser has no async context. OpenTelemetry's web context managers (the
// stack manager, and the HyperDX SDK's, which is the same thing unless async
// patching is switched on) hold the active span only for the SYNCHRONOUS
// extent of a context.with() call; the first `await` drops it. The
// interceptor stack restores the CALLER's context across its own awaits
// (#1428), which parents an RPC on a span the caller had active when it made
// the call. But "every RPC made while the chat is opening" is a statement
// about wall-clock time, not about a call stack: those RPCs start in effects,
// query functions and stream hooks that never ran inside the click handler,
// with no span active at all. So interaction.ts registers each open
// interaction here, and the trace interceptor asks outgoingContext() — the
// caller's span if it has one, else the foreground interaction — at the
// moment the call enters it, and holds that context until fetch() is called.
//
// ── Carrying the context to the browser fetch span ───────────────────────
//
// With the SDK running, the span the server joins is not ours: the SDK's
// patched fetch opens a CLIENT span parented on context.active() at the
// instant fetch() is called, and overwrites the traceparent with that span's.
// The trace interceptor calls next() inside context.with(ctx), which covers a
// call whose fetch() is synchronous with next() (connect-web's unary path).
// A server stream is not: connect-web awaits the request body
// before it calls fetch(), and the context is gone by then. So the
// interceptor also records the context against the request's Headers object
// (rememberRequestContext), which connect-web hands to fetch() as-is, and
// the fetch shim telemetry.ts installs over the SDK's patched fetch
// (installFetchContext) re-enters it for the duration of that call.
//
// This module imports nothing but @opentelemetry/api: the interceptors entry
// point (React-free, DOM-free) reaches it through trace.ts.
import { context, trace, type Context } from "@opentelemetry/api";

// Oldest first. The last one is the foreground: the operation the user asked
// for most recently.
const open: Context[] = [];

/** Register an open interaction; returns the function that unregisters it. */
export function registerOpenInteraction(ctx: Context): () => void {
  open.push(ctx);
  return () => {
    const i = open.indexOf(ctx);
    if (i !== -1) {
      open.splice(i, 1);
    }
  };
}

/**
 * The context an outgoing request should be parented on, right now.
 *
 * A span the caller made active wins: an action the app traced explicitly
 * (context.with(actionSpan, () => client.call())), a span under op.run(). The
 * interceptor stack carries the caller's context across its own awaits
 * (#1428), so the caller's span is still active when this runs. With no span
 * active, a request joins the most recently started open interaction — the
 * caller's context with the interaction's span set on it, so any other value
 * the caller carries (baggage, its own keys) still reaches the call. With none
 * open it is context.active(), exactly as before.
 */
export function outgoingContext(): Context {
  const active = context.active();
  const foreground = open[open.length - 1];
  if (!foreground) {
    return active;
  }
  const sc = trace.getSpanContext(active);
  if (sc && trace.isSpanContextValid(sc)) {
    return active;
  }
  const span = trace.getSpan(foreground);
  return span ? trace.setSpan(active, span) : active;
}

// Keyed by the request's Headers object. Weak, so an abandoned request leaves
// nothing behind.
const requestContexts = new WeakMap<object, Context>();

/** Remember which context the request carrying `headers` was parented on. */
export function rememberRequestContext(headers: Headers, ctx: Context): void {
  if (trace.getSpan(ctx)) {
    requestContexts.set(headers, ctx);
  }
}

function contextOfHeaders(headers: unknown): Context | undefined {
  return typeof headers === "object" && headers !== null
    ? requestContexts.get(headers)
    : undefined;
}

const SHIM_MARK = Symbol.for("forge.web-runtime.fetch-context");

type FetchFn = (...args: Parameters<typeof fetch>) => ReturnType<typeof fetch>;

/**
 * Wrap globalThis.fetch so a call whose request headers were remembered runs
 * inside the context its interceptor chose. Install it AFTER the telemetry SDK
 * has patched fetch, so the SDK's span is what starts inside that context.
 * Idempotent; a no-op without fetch.
 */
export function installFetchContext(): void {
  const g = globalThis as { fetch?: FetchFn };
  const inner = g.fetch;
  if (
    typeof inner !== "function" ||
    (inner as unknown as Record<symbol, unknown>)[SHIM_MARK]
  ) {
    return;
  }
  const wrapped = function (this: unknown, ...args: Parameters<typeof fetch>) {
    let ctx: Context | undefined;
    try {
      const [input, init] = args;
      ctx =
        contextOfHeaders(init?.headers) ??
        (typeof Request !== "undefined" && input instanceof Request
          ? contextOfHeaders(input.headers)
          : undefined);
    } catch {
      ctx = undefined;
    }
    if (!ctx || ctx === context.active()) {
      return inner.apply(this, args);
    }
    return context.with(ctx, () => inner.apply(this, args));
  };
  Object.defineProperty(wrapped, SHIM_MARK, { value: true });
  g.fetch = wrapped;
}
