// Part of @reliant-labs/web-runtime — the web twin of forge/pkg.
//
// OpenTelemetry JS SDK 2.x wiring for the browser: a WebTracerProvider with
// a batched OTLP/HTTP exporter, W3C trace-context propagation, and the fetch
// / XHR / document-load auto-instrumentations.
//
// This is the EXPORT path. Distributed tracing itself — the traceparent
// attached to every RPC, which is what makes browser spans join the backend's
// trace — is always on and lives in trace.ts; it needs no collector and does
// NOT depend on this module. Reach for this when you additionally want the
// browser's own spans shipped somewhere.
//
// Imported through the "@reliant-labs/web-runtime/otel" subpath, never the
// barrel. The eight @opentelemetry/* SDK packages below are heavy, and only
// the Next.js scaffold installs them: a Vite SPA or React Native frontend
// declares just @opentelemetry/api and imports the barrel, so re-exporting
// this from index.ts would leave those frontends resolving packages they
// never installed.
//
// Notable migration points from SDK 1.x -> 2.x:
//   - `new Resource({ ... })` is replaced by `resourceFromAttributes({ ... })`.
//     The `Resource` class is no longer exported from `@opentelemetry/resources`.
//   - `provider.addSpanProcessor(...)` is removed; span processors must be
//     passed via the `spanProcessors` option on the `WebTracerProvider`
//     constructor (the provider becomes immutable after construction).
//   - Minimum TS target is ES2022.
//
// See https://github.com/open-telemetry/opentelemetry-js/blob/main/doc/upgrade-to-2.x.md
import { context, propagation, trace } from "@opentelemetry/api";
import { getWebAutoInstrumentations } from "@opentelemetry/auto-instrumentations-web";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { registerInstrumentations } from "@opentelemetry/instrumentation";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { BatchSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { WebTracerProvider } from "@opentelemetry/sdk-trace-web";
import {
  ATTR_SERVICE_NAME,
  ATTR_SERVICE_VERSION,
} from "@opentelemetry/semantic-conventions";

export interface BrowserTracingConfig {
  /**
   * Collector base URL. Spans are POSTed to `${endpoint}/v1/traces`. Unset or
   * empty is a no-op — the app runs untraced rather than throwing, which is
   * what makes this safe to call unconditionally from the app shell.
   */
  endpoint?: string;
  /** `service.name` on every span. */
  serviceName: string;
  /** `service.version` on every span. Defaults to "0.0.0". */
  serviceVersion?: string;
}

let provider: WebTracerProvider | null = null;

/**
 * initBrowserTracing registers the browser tracer provider and the fetch /
 * XHR / document-load auto-instrumentations.
 *
 * No-ops without an `endpoint` (nothing to export to) and without a DOM
 * (SSR / prerender). Idempotent: a second call while a provider is already
 * registered returns without double-registering, which is what survives
 * React StrictMode's double-mount and Fast Refresh.
 *
 * The env vars that carry the endpoint stay in APPLICATION code
 * (`NEXT_PUBLIC_OTEL_ENDPOINT` and friends) — a bundler replaces those
 * literals at build time, and it can only do that where they are written
 * out verbatim.
 */
export function initBrowserTracing(config: BrowserTracingConfig): void {
  if (!config.endpoint || typeof window === "undefined") {
    return; // No-op when endpoint not configured or SSR
  }
  if (provider) {
    return; // Idempotent — avoid double-registering on HMR/StrictMode remounts
  }

  const resource = resourceFromAttributes({
    [ATTR_SERVICE_NAME]: config.serviceName,
    [ATTR_SERVICE_VERSION]: config.serviceVersion || "0.0.0",
  });

  provider = new WebTracerProvider({
    resource,
    spanProcessors: [
      new BatchSpanProcessor(
        new OTLPTraceExporter({
          url: `${config.endpoint}/v1/traces`,
        }),
      ),
    ],
  });

  // Enable W3C trace context propagation (sends traceparent header)
  propagation.setGlobalPropagator(new W3CTraceContextPropagator());
  provider.register();

  // Auto-instrument fetch, XMLHttpRequest, document load
  registerInstrumentations({
    instrumentations: [
      getWebAutoInstrumentations({
        "@opentelemetry/instrumentation-fetch": {
          propagateTraceHeaderCorsUrls: [/.*/], // Propagate to all URLs
        },
        "@opentelemetry/instrumentation-xml-http-request": {
          propagateTraceHeaderCorsUrls: [/.*/],
        },
      }),
    ],
  });
}

/** The tracer to open manual spans on. */
export function getTracer(name = "default") {
  return trace.getTracer(name);
}

export { context, propagation, trace };
