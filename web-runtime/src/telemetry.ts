// Part of @reliant-labs/web-runtime — the web twin of forge/pkg.
//
// Lightweight client RUM: unhandled errors + core Web Vitals, captured with
// native browser APIs (no extra dependency). Export is OPT-IN: by default
// events go to an in-process sink (console.debug in dev). Set `endpoint` to
// beacon them to a browser-reachable OTLP/HTTP collector (needs CORS on that
// collector — stand it up before relying on export). This is the config seam;
// distributed tracing itself (the traceparent on every RPC) is always on and
// lives in trace.ts — it does NOT depend on this module.
export interface TelemetryEvent {
  type: "error" | "unhandled-rejection" | "web-vital";
  name: string;
  value?: number;
  message?: string;
  detail?: Record<string, unknown>;
  timestamp: number;
}

export interface TelemetryConfig {
  serviceName?: string;
  /** OTLP/HTTP-ish collector URL. Unset = no network export (default). */
  endpoint?: string;
  /** In-process sink for every event. Default: console.debug in dev. */
  onEvent?: (event: TelemetryEvent) => void;
  /** Capture window errors + unhandled rejections. Default true. */
  captureErrors?: boolean;
  /** Capture LCP / CLS / FID via PerformanceObserver. Default true. */
  captureWebVitals?: boolean;
}

interface LayoutShiftEntry extends PerformanceEntry {
  value: number;
  hadRecentInput: boolean;
}

interface FirstInputEntry extends PerformanceEntry {
  processingStart: number;
}

// `process` is a bundler-injected global (Next replaces the literal
// `process.env.NODE_ENV` at build time), not a browser one. Declaring it
// locally keeps this package free of a @types/node dependency while leaving
// the expression textually intact so that replacement still happens.
declare const process:
  | { env?: Record<string, string | undefined> }
  | undefined;

let started = false;

/**
 * initClientTelemetry wires the capture listeners and returns a teardown
 * function. Idempotent and SSR-safe (no-op without a DOM). Call once from the
 * app shell (e.g. inside Providers' mount effect).
 */
export function initClientTelemetry(config: TelemetryConfig = {}): () => void {
  if (started || typeof window === "undefined") {
    return () => {};
  }
  started = true;

  const serviceName = config.serviceName ?? "frontend";
  const captureErrors = config.captureErrors ?? true;
  const captureWebVitals = config.captureWebVitals ?? true;

  const dispatch = (event: TelemetryEvent) => {
    if (config.onEvent) {
      config.onEvent(event);
    } else if (
      typeof process !== "undefined" &&
      process.env?.NODE_ENV === "development"
    ) {
      // The `typeof` guard keeps this framework-agnostic — Vite SPAs and
      // plain browsers have no `process` at all.
      console.debug("[telemetry]", event.type, event.name, event.value ?? "");
    }
    if (config.endpoint) {
      exportEvent(config.endpoint, serviceName, event);
    }
  };

  const teardowns: Array<() => void> = [];

  if (captureErrors) {
    const onError = (e: ErrorEvent) =>
      dispatch({
        type: "error",
        name: e.error?.name ?? "Error",
        message: e.message,
        detail: { filename: e.filename, line: e.lineno, col: e.colno },
        timestamp: Date.now(),
      });
    const onRejection = (e: PromiseRejectionEvent) =>
      dispatch({
        type: "unhandled-rejection",
        name: "UnhandledRejection",
        message: String(e.reason),
        timestamp: Date.now(),
      });
    window.addEventListener("error", onError);
    window.addEventListener("unhandledrejection", onRejection);
    teardowns.push(() => window.removeEventListener("error", onError));
    teardowns.push(() =>
      window.removeEventListener("unhandledrejection", onRejection),
    );
  }

  if (captureWebVitals && typeof PerformanceObserver === "function") {
    let clsValue = 0;
    const observe = (
      type: string,
      cb: (entries: PerformanceEntryList) => void,
    ) => {
      try {
        const obs = new PerformanceObserver((list) => cb(list.getEntries()));
        obs.observe({ type, buffered: true });
        teardowns.push(() => obs.disconnect());
      } catch {
        // Unsupported entry type in this browser — skip silently.
      }
    };

    observe("largest-contentful-paint", (entries) => {
      const last = entries.at(-1);
      if (last) {
        dispatch({
          type: "web-vital",
          name: "LCP",
          value: Math.round(last.startTime),
          timestamp: Date.now(),
        });
      }
    });

    observe("layout-shift", (entries) => {
      for (const entry of entries as LayoutShiftEntry[]) {
        if (!entry.hadRecentInput) {
          clsValue += entry.value;
        }
      }
      dispatch({
        type: "web-vital",
        name: "CLS",
        value: Number(clsValue.toFixed(4)),
        timestamp: Date.now(),
      });
    });

    // FID (First Input Delay): processingStart − startTime of the first
    // input. This is FID, NOT INP — INP needs the full interaction-latency
    // model from the web-vitals library, which this dependency-free module
    // deliberately doesn't pull in. Reach for `web-vitals` if you need INP.
    observe("first-input", (entries) => {
      const first = entries[0] as FirstInputEntry | undefined;
      if (first) {
        dispatch({
          type: "web-vital",
          name: "FID",
          value: Math.round(first.processingStart - first.startTime),
          timestamp: Date.now(),
        });
      }
    });
  }

  return () => {
    for (const t of teardowns) {
      t();
    }
    started = false;
  };
}

/** Best-effort beacon of one event. Never throws into the caller. */
function exportEvent(
  endpoint: string,
  serviceName: string,
  event: TelemetryEvent,
): void {
  try {
    const body = JSON.stringify({ service: serviceName, ...event });
    if (typeof navigator !== "undefined" && navigator.sendBeacon) {
      navigator.sendBeacon(endpoint, body);
    } else {
      void fetch(endpoint, {
        method: "POST",
        body,
        keepalive: true,
        headers: { "Content-Type": "application/json" },
      }).catch(() => {});
    }
  } catch {
    // Telemetry must never break the app.
  }
}
