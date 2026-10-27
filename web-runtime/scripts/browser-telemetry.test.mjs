// Slow, opt-in real Chromium/HyperDX contract proof: npm run test:browser.
// No live collector, credentials or deployed service; both HTTP origins bind
// ephemeral loopback ports. The normal vitest tier remains browser-free.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { chromium } from "playwright";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const require = createRequire(import.meta.url);
const apiDir = dirname(require.resolve("@opentelemetry/api/package.json"));
const protoDir = dirname(require.resolve("@bufbuild/protobuf/package.json"));
const sdk = require.resolve("@hyperdx/browser");
const listen = async (server) => {
  await new Promise((done) => server.listen(0, "127.0.0.1", done));
  return `http://127.0.0.1:${server.address().port}`;
};
const close = (server) => new Promise((done) => {
  server.closeAllConnections();
  server.close(done);
});

// URLs select only files in these package roots, never the host filesystem.
async function moduleFile(url) {
  const routes = {
    "/runtime/": join(root, "dist"),
    "/otel/": join(apiDir, "build/esm"),
    "/proto/": join(protoDir, "dist/esm"),
  };
  for (const [prefix, base] of Object.entries(routes)) {
    if (!url.startsWith(prefix)) continue;
    const path = resolve(base, decodeURIComponent(url.slice(prefix.length)));
    if (!path.startsWith(`${base}/`)) throw new Error("invalid module path");
    return readFile(path.endsWith(".js") ? path : `${path}.js`);
  }
  throw new Error("unknown module");
}

test("raw interactions stay in masked replay; explicit action and RPC share real span parents", { timeout: 30000 }, async () => {
  const traces = [];
  const logs = [];
  const requests = [];
  const backend = createServer((req, res) => {
    res.setHeader("Access-Control-Allow-Origin", "*");
    res.setHeader("Access-Control-Allow-Headers", "traceparent,tracestate,content-type");
    if (req.method !== "OPTIONS") requests.push(req.headers.traceparent);
    res.end("{}");
  });
  const backendOrigin = await listen(backend);
  const server = createServer(async (req, res) => {
    try {
      const url = new URL(req.url, "http://localhost").pathname;
      if (url.startsWith("/_otel/")) {
        const chunks = [];
        for await (const chunk of req) chunks.push(chunk);
        const payload = JSON.parse(Buffer.concat(chunks).toString());
        (url.endsWith("traces") ? traces : logs).push(payload);
        res.setHeader("content-type", "application/json");
        res.end("{}");
      } else if (url === "/sdk.js") {
        res.setHeader("content-type", "text/javascript");
        res.end(await readFile(sdk));
      } else if (url === "/hyperdx.js") {
        res.setHeader("content-type", "text/javascript");
        res.end("export default globalThis.HyperDX;");
      } else if (url === "/") {
        res.setHeader("content-type", "text/html");
        res.end(`<!doctype html><p>PRIVATE_TEXT_SENTINEL</p><input value="PRIVATE_INPUT_SENTINEL"><button id="idle">Idle</button><button id="action">Run</button>
<script src="/sdk.js"></script>
<script type="importmap">{"imports":{"@hyperdx/browser":"/hyperdx.js","@opentelemetry/api":"/otel/index.js","@bufbuild/protobuf/wkt":"/proto/wkt/index.js"}}</script>
<script type="module">
import { initBrowserTelemetry, context, trace } from '/runtime/telemetry.js';
import { traceInterceptor } from '/runtime/interceptors.js';
window.ready = await initBrowserTelemetry({ serviceName: 'browser-proof', endpoint: '/_otel', replay: true, apiOrigins: [${JSON.stringify(backendOrigin)}], consoleCapture: false });
window.tracing = trace;
for (const type of ['mousedown','mouseup','click']) document.querySelector('#idle').addEventListener(type, () => {});
document.querySelector('#action').addEventListener('click', () => {
  const action = trace.getTracer('meaningful-action').startSpan('save-record');
  context.with(trace.setSpan(context.active(), action), () => {
    window.finished = traceInterceptor()(async req => {
      await fetch(${JSON.stringify(`${backendOrigin}/svc/Save`)}, { method: 'POST', headers: req.header });
    })({ header: new Headers(), stream: false }).finally(() => action.end());
  });
});
</script>`);
      } else {
        res.setHeader("content-type", "text/javascript");
        res.end(await moduleFile(url));
      }
    } catch (error) {
      res.statusCode = 500;
      res.end(String(error));
    }
  });
  const origin = await listen(server);
  let browser;
  try {
    browser = await chromium.launch({ headless: true });
    const page = await browser.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.goto(origin);
    await page.waitForFunction(() => window.ready === true);
    await page.locator("#idle").click();
    await page.locator("input").fill("PRIVATE_EDIT_SENTINEL");
    await page.locator("#action").click();
    await page.evaluate(() => window.finished);
    await page.evaluate(async () => {
      // Both are the REAL SDK's providers, not a test substitute.
      await window.tracing.getTracerProvider().getDelegate().forceFlush();
      HyperDX.stopSessionRecorder();
    });
    await page.waitForFunction(() => true);
    // The recorder flushes on visibility/page exit, just like a real tab.
    await page.goto("about:blank");
    const spans = traces.flatMap((batch) => batch.resourceSpans ?? [])
      .flatMap((resource) => resource.scopeSpans ?? [])
      .flatMap((scope) => scope.spans ?? []);
    assert.deepEqual(errors, []);
    assert.equal(spans.some((span) => ["mousedown", "mouseup", "click"].includes(span.name)), false);
    const action = spans.find((span) => span.name === "save-record");
    const fetch = spans.find((span) => span.name === "HTTP POST");
    assert.ok(action, "explicitly instrumented meaningful action is exported");
    assert.ok(fetch, "network span is still exported");
    assert.equal(fetch.traceId, action.traceId);
    assert.equal(fetch.parentSpanId, action.spanId, "fetch is a child, not just the same trace");
    assert.deepEqual(requests, [`00-${fetch.traceId}-${fetch.spanId}-01`], "wire parent is the exported fetch span, not the interceptor's synthetic fallback");
    assert.ok(spans.some((span) => span.name === "documentLoad"), "document-load tracing remains enabled");
    const replay = logs.flatMap((batch) => batch.resourceLogs ?? [])
      .flatMap((resource) => resource.scopeLogs ?? [])
      .filter((scope) => scope.scope?.name === "rum.rr-web")
      .flatMap((scope) => scope.logRecords ?? []);
    assert.ok(replay.length > 0, "session replay still exports records");
    const exported = JSON.stringify([...traces, ...logs]);
    for (const secret of ["PRIVATE_TEXT_SENTINEL", "PRIVATE_INPUT_SENTINEL", "PRIVATE_EDIT_SENTINEL"]) {
      assert.equal(exported.includes(secret), false, "private text never leaves the browser");
    }
  } finally {
    await browser?.close();
    await close(server);
    await close(backend);
  }
});
