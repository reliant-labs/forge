// Part of @reliant-labs/web-runtime — the web twin of forge/pkg.
//
// The headline guarantee, self-tested: every Connect call carries a valid
// W3C traceparent, WITHOUT any OTLP collector configured, so the backend
// (otelconnect + otelhttp) joins the same distributed trace. If this test
// fails, full-stack tracing is broken.
import { describe, expect, it } from "vitest";

import { buildRuntimeInterceptors, traceInterceptor } from "./interceptors.js";
import { freshTraceparent, injectTraceContext, TRACEPARENT_RE } from "./trace.js";

const ZERO_TRACE_ID = "0".repeat(32);

describe("traceparent propagation", () => {
  it("injectTraceContext writes a valid, non-zero traceparent", () => {
    const header = new Headers();
    injectTraceContext(header);

    const tp = header.get("traceparent");
    expect(tp).not.toBeNull();
    expect(tp).toMatch(TRACEPARENT_RE);
    // trace id (2nd field) must not be all zeros per the W3C spec.
    expect(tp?.split("-")[1]).not.toBe(ZERO_TRACE_ID);
  });

  it("freshTraceparent is well-formed and unique per call", () => {
    const a = freshTraceparent();
    const b = freshTraceparent();
    expect(a).toMatch(TRACEPARENT_RE);
    expect(a).not.toBe(b);
  });

  it("the trace interceptor attaches traceparent to the outgoing request", async () => {
    // The runtime trace interceptor only reads/writes req.header, so a
    // minimal request shape exercises it end to end.
    const req = { header: new Headers(), stream: false };
    let propagated: string | null = null;

    const next = async (r: { header: Headers }) => {
      propagated = r.header.get("traceparent");
      return { message: {} };
    };

    const run = traceInterceptor() as unknown as (
      n: typeof next,
    ) => (r: typeof req) => Promise<unknown>;
    await run(next)(req);

    expect(propagated).not.toBeNull();
    expect(propagated).toMatch(TRACEPARENT_RE);
  });

  it("buildRuntimeInterceptors composes the full stack", () => {
    // error-normalize → auth → brand headers → traceparent.
    // retryInterceptor is intentionally NOT in the default chain — React
    // Query is the single retry owner (see interceptors.ts).
    expect(buildRuntimeInterceptors()).toHaveLength(4);
  });
});
