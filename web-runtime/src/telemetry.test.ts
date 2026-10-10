// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// The contract of initBrowserTelemetry, pinned against a fake of the HyperDX
// SDK: SSR is a no-op, init is idempotent, the ingest URL is same-origin,
// replay is off by default and masked when on, and no real secret is ever in
// the options. The SDK itself is mocked — its behaviour is HyperDX's to test;
// what is ours is exactly what we hand it.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const init = vi.fn();
const recordException = vi.fn();

vi.mock("@hyperdx/browser", () => ({
  default: { init, recordException },
}));

const ORIGIN = "http://localhost:3000";

type Telemetry = typeof import("./telemetry.js");
type Reporter = typeof import("./error-reporter.js");

async function load(): Promise<{ t: Telemetry; r: Reporter }> {
  vi.resetModules();
  return {
    t: await import("./telemetry.js"),
    r: await import("./error-reporter.js"),
  };
}

type Listener = (e: unknown) => void;
const listeners = new Map<string, Listener[]>();

function stubDom() {
  listeners.clear();
  vi.stubGlobal("window", {
    location: { origin: ORIGIN },
    addEventListener: (type: string, fn: Listener) =>
      listeners.set(type, [...(listeners.get(type) ?? []), fn]),
  });
}

function dispatch(type: string, event: unknown) {
  for (const fn of listeners.get(type) ?? []) fn(event);
}

beforeEach(() => {
  init.mockReset();
  recordException.mockReset();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("initBrowserTelemetry", () => {
  it("is a no-op without a DOM and never loads the SDK", async () => {
    const { t } = await load();

    await expect(
      t.initBrowserTelemetry({ serviceName: "web", endpoint: "/_otel" }),
    ).resolves.toBe(false);
    expect(init).not.toHaveBeenCalled();
  });

  it("is a no-op without an endpoint, or with it set to off", async () => {
    stubDom();
    const { t } = await load();

    await expect(t.initBrowserTelemetry({ serviceName: "web" })).resolves.toBe(
      false,
    );
    await expect(
      t.initBrowserTelemetry({ serviceName: "web", endpoint: "off" }),
    ).resolves.toBe(false);
    expect(init).not.toHaveBeenCalled();
  });

  it("initialises the SDK exactly once however often it is called", async () => {
    stubDom();
    const { t } = await load();
    const cfg = { serviceName: "web", endpoint: "/_otel" };

    const results = await Promise.all([
      t.initBrowserTelemetry(cfg),
      t.initBrowserTelemetry(cfg),
    ]);
    await t.initBrowserTelemetry(cfg);

    expect(results).toEqual([true, true]);
    expect(init).toHaveBeenCalledTimes(1);
  });

  it("never throws when the SDK fails to load or init", async () => {
    stubDom();
    init.mockImplementation(() => {
      throw new Error("boom");
    });
    vi.spyOn(console, "debug").mockImplementation(() => undefined);
    const { t } = await load();

    await expect(
      t.initBrowserTelemetry({ serviceName: "web", endpoint: "/_otel" }),
    ).resolves.toBe(false);
  });

  it("sends to this origin's /_otel, with a placeholder key and no real secret", async () => {
    stubDom();
    const { t } = await load();

    await t.initBrowserTelemetry({
      serviceName: "web",
      endpoint: t.DEFAULT_INGEST_PATH,
    });

    const opts = init.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(opts.url).toBe(`${ORIGIN}/_otel`);
    expect(new URL(opts.url as string).origin).toBe(ORIGIN);
    expect(opts.apiKey).toBe(t.PLACEHOLDER_API_KEY);
    expect(opts.service).toBe("web");
  });

  it("records exceptions through the SDK once running, and not before", async () => {
    stubDom();
    const { t, r } = await load();
    const err = new Error("render crash");

    // before init: nothing to hand it to, and the caller is told so
    expect(r.reportException(err)).toBe(false);
    expect(recordException).not.toHaveBeenCalled();

    await t.initBrowserTelemetry({ serviceName: "web", endpoint: "/_otel" });
    expect(r.reportException(err, { "react.component_stack": "at X" })).toBe(
      true,
    );

    expect(recordException).toHaveBeenCalledWith(err, {
      "react.component_stack": "at X",
    });
  });
});

describe("reportException de-duplication", () => {
  it("skips an error the SDK already recorded through console.error, and records any other", async () => {
    stubDom();
    const original = console.error;
    const sink = vi.fn();
    console.error = sink;
    try {
      const { t, r } = await load();
      await t.initBrowserTelemetry({ serviceName: "web", endpoint: "/_otel" });

      // React logs a boundary-caught error to console.error first.
      const logged = new Error("caught by boundary");
      console.error(logged);
      r.reportException(logged);
      expect(sink).toHaveBeenCalledWith(logged); // still forwarded untouched
      expect(recordException).not.toHaveBeenCalled();

      const silent = new Error("never logged");
      r.reportException(silent);
      expect(recordException).toHaveBeenCalledTimes(1);
      expect(recordException).toHaveBeenCalledWith(silent, undefined);
    } finally {
      console.error = original;
    }
  });

  it("skips an error the SDK saw as an uncaught window error or unhandled rejection", async () => {
    stubDom();
    const { t, r } = await load();
    await t.initBrowserTelemetry({ serviceName: "web", endpoint: "/_otel" });

    const uncaught = new Error("uncaught render error");
    dispatch("error", { error: uncaught });
    const rejected = new Error("rejected");
    dispatch("unhandledrejection", { reason: rejected });

    // Still reports "the SDK has it" so a caller does not console.error it.
    expect(r.reportException(uncaught)).toBe(true);
    expect(r.reportException(rejected)).toBe(true);
    expect(recordException).not.toHaveBeenCalled();
  });
});

describe("buildSdkOptions", () => {
  const base = { serviceName: "web" };

  it("keeps replay OFF by default", async () => {
    const { t } = await load();
    const opts = t.buildSdkOptions(base, `${ORIGIN}/_otel`, ORIGIN);

    expect(opts.disableReplay).toBe(true);
    expect(opts).not.toHaveProperty("maskAllInputs");
  });

  it("forces all text and all inputs masked whenever replay is enabled", async () => {
    const { t } = await load();
    const opts = t.buildSdkOptions(
      { ...base, replay: true },
      `${ORIGIN}/_otel`,
      ORIGIN,
    );

    expect(opts.disableReplay).toBe(false);
    expect(opts.maskAllInputs).toBe(true);
    expect(opts.maskAllText).toBe(true);
    expect(opts.recordCanvas).toBe(false);
  });

  it("captures console, but not network bodies, by default", async () => {
    const { t } = await load();
    const opts = t.buildSdkOptions(base, `${ORIGIN}/_otel`, ORIGIN);

    expect(opts.consoleCapture).toBe(true);
    expect(opts.advancedNetworkCapture).toBe(false);
    expect(opts.disableIntercom).toBe(true);
  });

  it("propagates traceparent to this origin and to the API origin, and nowhere else", async () => {
    const { t } = await load();
    const opts = t.buildSdkOptions(
      { ...base, apiOrigins: ["http://localhost:8080/", undefined] },
      `${ORIGIN}/_otel`,
      ORIGIN,
    );
    const targets = opts.tracePropagationTargets as RegExp[];
    const matches = (url: string) => targets.some((re) => re.test(url));

    expect(matches(`${ORIGIN}/anything`)).toBe(true);
    expect(matches("http://localhost:8080/svc.v1.Svc/Method")).toBe(true);
    expect(matches("http://localhost:8080")).toBe(true);
    expect(matches("https://third-party.example.com/x")).toBe(false);
    // Prefix lookalikes must not match: localhost:80801 is another origin.
    expect(matches("http://localhost:80801/x")).toBe(false);
  });

  it("does not trace its own exports", async () => {
    const { t } = await load();
    const opts = t.buildSdkOptions(base, `${ORIGIN}/_otel`, ORIGIN);
    const ignore = opts.ignoreUrls as RegExp[];

    expect(ignore.some((re) => re.test(`${ORIGIN}/_otel/v1/traces`))).toBe(
      true,
    );
    expect(ignore.some((re) => re.test(`${ORIGIN}/api/things`))).toBe(false);
  });

  it("emits both the current and the legacy deployment-environment key", async () => {
    const { t } = await load();
    const opts = t.buildSdkOptions(
      {
        ...base,
        environment: "staging",
        serviceVersion: "1.2.3",
        serviceNamespace: "shop",
      },
      `${ORIGIN}/_otel`,
      ORIGIN,
    );

    expect(opts.otelResourceAttributes).toEqual({
      "service.version": "1.2.3",
      "service.namespace": "shop",
      "deployment.environment.name": "staging",
      "deployment.environment": "staging",
    });
  });
});

describe("resolveIngestUrl", () => {
  it("resolves a path against the page origin", async () => {
    const { t } = await load();

    expect(t.resolveIngestUrl("/_otel", ORIGIN)).toBe(`${ORIGIN}/_otel`);
    expect(t.resolveIngestUrl("/_otel/", ORIGIN)).toBe(`${ORIGIN}/_otel`);
  });

  it("returns null when telemetry is off", async () => {
    const { t } = await load();

    expect(t.resolveIngestUrl(undefined, ORIGIN)).toBeNull();
    expect(t.resolveIngestUrl("  ", ORIGIN)).toBeNull();
    expect(t.resolveIngestUrl("OFF", ORIGIN)).toBeNull();
  });
});
