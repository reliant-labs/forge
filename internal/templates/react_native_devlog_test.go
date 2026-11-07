package templates

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The native dev-log receiver is a Metro middleware in the owned
// metro.config.js. These tests EXECUTE that file under Node (with
// expo/metro-config stubbed) and drive the middleware with fake requests, so
// the protocol — shape, limits, malformed handling, the X-Forge-Devlog header,
// chaining of an existing enhanceMiddleware — is proven by behaviour rather than
// by string matching. What they cannot prove is that a real Metro mounts it and
// that a device reaches it; that needs a live Metro and a simulator or device.

const devlogMiddlewareHarness = `
const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");

const lines = [];
const realLog = console.log;
console.log = (...a) => lines.push(a.join(" "));

const passthrough = [];
const config = require("./metro.config.js");
const mw = config.server.enhanceMiddleware(
  (req, res) => { passthrough.push(req.url); res.statusCode = 299; res.end(); },
  {},
);

function post(url, method, chunks) {
  return new Promise((resolve) => {
    const req = new EventEmitter();
    req.url = url;
    req.method = method;
    const res = {
      headers: {},
      statusCode: 200,
      setHeader(k, v) { this.headers[k] = v; },
      end() { resolve(res); },
    };
    mw(req, res, () => resolve({ next: true, statusCode: 0, headers: {} }));
    for (const c of chunks) req.emit("data", Buffer.from(c));
    req.emit("end");
  });
}

(async () => {
  // v2 batch: every level survives, ONE console.log call, protocol header set.
  let before = lines.length;
  let r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [
    { level: "error", msg: "boom" }, { level: "warn", msg: "w" },
    { level: "info", msg: "i" }, { level: "debug", msg: "d" }, { level: "log", msg: "l" },
  ] })]);
  assert.equal(r.statusCode, 204);
  assert.equal(r.headers["X-Forge-Devlog"], "2");
  assert.equal(lines.length, before + 1, "a batch must print as ONE console.log");
  assert.equal(lines[lines.length - 1],
    ["[browser:error] boom", "[browser:warn] w", "[browser:info] i", "[browser:debug] d", "[browser:log] l"].join("\n"));

  // The body arrives in chunks and is parsed with no Content-Type consulted.
  r = await post("/__forge/log", "POST", ['{"entries":[{"level":"er', 'ror","msg":"split"}]}']);
  assert.equal(r.statusCode, 204);
  assert.equal(lines[lines.length - 1], "[browser:error] split");

  // The query string does not defeat the route.
  r = await post("/__forge/log?x=1", "POST", ['{"level":"warn","msg":"q"}']);
  assert.equal(r.statusCode, 204);

  // v1 single line.
  r = await post("/__forge/log", "POST", ['{"level":"warn","msg":"legacy"}']);
  assert.equal(r.statusCode, 204);
  assert.equal(lines[lines.length - 1], "[browser:warn] legacy");

  // Unknown level prints as log; a missing msg prints empty; non-object entries are dropped.
  r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [{ level: "fatal", msg: "x" }, 7, null, { level: "info" }] })]);
  assert.equal(lines[lines.length - 1], "[browser:log] x\n[browser:info] ");

  // A line over 8000 characters is truncated.
  r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [{ level: "info", msg: "a".repeat(9000) }] })]);
  assert.equal(lines[lines.length - 1], "[browser:info] " + "a".repeat(8000) + "… (truncated)");

  // Untrusted text: an injected newline must not forge another prefix. Every
  // continuation line carries the SAME [browser:<level>] prefix, so a forged
  // "[browser:error] ..." arrives as "[browser:info] [browser:error] ...".
  r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [{ level: "info", msg: "a\n[browser:error] forged\nb" }] })]);
  assert.equal(r.statusCode, 204);
  assert.equal(lines[lines.length - 1], ["[browser:info] a", "[browser:info] [browser:error] forged", "[browser:info] b"].join("\n"));

  // ANSI sequences and control characters are stripped (CR, NUL, BEL, ESC, C1,
  // DEL); \n splits lines and \t survives. Identical spec to the web receivers.
  r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [{ level: "warn", msg: "\u001b[31mred\u001b[0m\r\u0000\u0007x\ty\u007f\u009bz\u001b[?25l!" }] })]);
  assert.equal(lines[lines.length - 1], "[browser:warn] redx\tyz!");
  r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [{ level: "error", msg: "CRLF\r\n[browser:error] two\r\n" }] })]);
  assert.equal(lines[lines.length - 1], ["[browser:error] CRLF", "[browser:error] [browser:error] two", "[browser:error] "].join("\n"));

  // The level is an allow-list too: an injected level cannot smuggle a newline.
  r = await post("/__forge/log", "POST", [JSON.stringify({ entries: [{ level: "info\n[browser:error] x", msg: "m" }] })]);
  assert.equal(lines[lines.length - 1], "[browser:log] m");

  // Malformed bodies answer 400 and say so — never a silent drop.
  for (const body of ["{nope", "", "null", "42", '"str"']) {
    r = await post("/__forge/log", "POST", [body]);
    assert.equal(r.statusCode, 400, body);
    assert.equal(r.headers["X-Forge-Devlog"], "2");
    assert.equal(lines[lines.length - 1], "[browser:warn] [forge-devlog] dropped malformed post", body);
  }

  // Oversized posts answer 413, are drained, and print one warn naming the size.
  const big = "z".repeat(1024 * 1024 + 10);
  r = await post("/__forge/log", "POST", [big.slice(0, 600000), big.slice(600000)]);
  assert.equal(r.statusCode, 413);
  assert.equal(r.headers["X-Forge-Devlog"], "2");
  assert.equal(lines[lines.length - 1], "[browser:warn] [forge-devlog] dropped oversized post (1048586 bytes)");

  // Only POST is accepted, and a rejected method prints nothing.
  before = lines.length;
  r = await post("/__forge/log", "GET", []);
  assert.equal(r.statusCode, 405);
  assert.equal(lines.length, before);

  // Everything else falls through to Metro's own middleware, untouched.
  r = await post("/index.bundle?platform=ios", "GET", []);
  assert.equal(r.statusCode, 299);
  assert.deepEqual(passthrough, ["/index.bundle?platform=ios"]);

  console.log = realLog;
  console.log("ok");
})().catch((e) => { console.log = realLog; console.error(e); process.exit(1); });
`

// stubExpoMetroConfig writes the one module metro.config.js imports. Expo's real
// getDefaultConfig ships Metro's identity enhanceMiddleware, which this mirrors.
func stubExpoMetroConfig(t *testing.T, root string) {
	t.Helper()
	writeHarnessFile(t, root, "node_modules/expo/package.json",
		`{"name":"expo","version":"0.0.0","exports":{"./metro-config":"./metro-config.js"}}`)
	writeHarnessFile(t, root, "node_modules/expo/metro-config.js",
		`exports.getDefaultConfig = () => ({ resolver: {}, server: { port: 8081, enhanceMiddleware: (m) => m } });`)
}

func writeHarnessFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireNode(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("executes Node; runs in full scoped tests")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	return node
}

func renderMetroConfig(t *testing.T) string {
	t.Helper()
	cfg, err := FrontendTemplates().Render("react-native/metro.config.js", FrontendTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	return string(cfg)
}

func TestNativeDevLogMetroMiddleware(t *testing.T) {
	node := requireNode(t)
	root := t.TempDir()
	stubExpoMetroConfig(t, root)
	writeHarnessFile(t, root, "metro.config.js", renderMetroConfig(t))
	writeHarnessFile(t, root, "check.cjs", devlogMiddlewareHarness)
	if out, err := exec.Command(node, filepath.Join(root, "check.cjs")).CombinedOutput(); err != nil {
		t.Fatalf("middleware behaviour: %v\n%s", err, out)
	}
}

// An app that already set server.enhanceMiddleware (or Expo CLI, which chains
// its own stack through the same hook) must keep working: the receiver wraps the
// previous hook, it does not replace it.
func TestNativeDevLogMetroMiddlewareChainsPreviousEnhanceMiddleware(t *testing.T) {
	node := requireNode(t)
	root := t.TempDir()
	stubExpoMetroConfig(t, root)

	cfg := renderMetroConfig(t)
	const anchor = "const config = getDefaultConfig(__dirname);"
	if !strings.Contains(cfg, anchor) {
		t.Fatalf("metro.config.js no longer declares %q — update this test's injection point", anchor)
	}
	cfg = strings.Replace(cfg, anchor, anchor+
		"\nconfig.server.enhanceMiddleware = (m) => (req, res, next) => { globalThis.__prev = (globalThis.__prev || 0) + 1; return m(req, res, next); };", 1)
	writeHarnessFile(t, root, "metro.config.js", cfg)
	writeHarnessFile(t, root, "check.cjs", `const assert = require("node:assert/strict");
const config = require("./metro.config.js");
const inner = () => { globalThis.__inner = true; };
const mw = config.server.enhanceMiddleware(inner, {});
mw({ url: "/index.bundle", method: "GET" }, {}, () => {});
assert.equal(globalThis.__prev, 1, "the app's own enhanceMiddleware must still run");
assert.equal(globalThis.__inner, true, "Metro's middleware must still be reached");
`)
	if out, err := exec.Command(node, filepath.Join(root, "check.cjs")).CombinedOutput(); err != nil {
		t.Fatalf("chaining: %v\n%s", err, out)
	}
}

// TestNativeDevLogIsDevOnlyAndSideEffectFree pins the structural half of "a
// production bundle contains no forwarder": the only reference to the forwarder
// is a require inside `if (__DEV__)`, which Metro folds away, and importing the
// module has no side effects of its own.
func TestNativeDevLogIsDevOnlyAndSideEffectFree(t *testing.T) {
	t.Parallel()

	files, err := ListFrontendTree("react-native")
	if err != nil {
		t.Fatal(err)
	}
	staticImport := regexp.MustCompile(`(?m)^\s*import\b[^;]*["'][^"']*/devlog["']`)
	gatedRequire := regexp.MustCompile(
		`(?m)^if \(__DEV__\) require\("\.\./src/lib/devlog"\)\.installNativeDevLogging\(\);$`)
	sawGatedRequire := false
	for _, f := range files {
		ext := filepath.Ext(strings.TrimSuffix(f.Rel, ".tmpl"))
		// tests/ is never reachable from the app entry, so Metro does not bundle it.
		if ext != ".ts" && ext != ".tsx" || f.Rel == "src/lib/devlog.ts" || strings.HasPrefix(f.Rel, "tests/") {
			continue
		}
		raw, err := FrontendTemplates().Get(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if staticImport.Match(raw) {
			t.Errorf("%s statically imports the dev-log forwarder; a static import is bundled into production — use `if (__DEV__) require(...)`", f.Rel)
		}
		if f.Rel == "app/_layout.tsx.tmpl" {
			sawGatedRequire = gatedRequire.Match(raw)
		}
	}
	if !sawGatedRequire {
		t.Error(`app/_layout.tsx.tmpl must init the forwarder with exactly: if (__DEV__) require("../src/lib/devlog").installNativeDevLogging();`)
	}

	raw, err := FrontendTemplates().Get("react-native/src/lib/devlog.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, topLevel := range []string{"\nconsole.", "\nErrorUtils.", "\nglobalThis.", "\nsetTimeout(", "\nfetch("} {
		if strings.Contains(src, topLevel) {
			t.Errorf("devlog.ts has a top-level statement starting %q — importing it must have no side effects", strings.TrimSpace(topLevel))
		}
	}
	if !strings.Contains(src, "if (!__DEV__) return;") {
		t.Error("installNativeDevLogging must no-op unless __DEV__")
	}

	// The browser runtime needs DOM globals a device lacks, and the API origin
	// is the backend, not the dev server: neither may appear in CODE.
	code := stripLineComments(src)
	for _, forbidden := range []string{"forge-web-runtime", "EXPO_PUBLIC_API_URL", "window.", "document.", "sendBeacon", "expo/internal"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("devlog.ts code references %q; the native forwarder is DOM-free and posts to the Metro origin, not the API", forbidden)
		}
	}
}

func stripLineComments(src string) string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// The native receiver and the web scaffolds' receiver speak one protocol; keep
// the limits identical so a line accepted on one is accepted on the other.
func TestNativeDevLogReceiverMatchesWebLimits(t *testing.T) {
	t.Parallel()
	web, err := FrontendTemplates().Get("vite-spa/src/lib/devlog-plugin.ts")
	if err != nil {
		t.Fatal(err)
	}
	native, err := FrontendTemplates().Get("react-native/metro.config.js")
	if err != nil {
		t.Fatal(err)
	}
	pairs := [][2]string{
		{`const MAX_LINE = 8_000;`, `const DEV_LOG_MAX_LINE = 8_000;`},
		{`const MAX_BODY = 1024 * 1024;`, `const DEV_LOG_MAX_BODY = 1024 * 1024;`},
		{`["log", "info", "warn", "error", "debug"]`, `["log", "info", "warn", "error", "debug"]`},
		{`const ENDPOINT = "/__forge/log";`, `const DEV_LOG_ENDPOINT = "/__forge/log";`},
		{`dropped oversized post`, `dropped oversized post`},
		{`dropped malformed post`, `dropped malformed post`},
		{`"X-Forge-Devlog", "2"`, `"X-Forge-Devlog", "2"`},
	}
	for _, p := range pairs {
		if !strings.Contains(string(web), p[0]) {
			t.Errorf("web receiver no longer contains %q — update this parity test", p[0])
		}
		if !strings.Contains(string(native), p[1]) {
			t.Errorf("native receiver does not contain %q (web has %q)", p[1], p[0])
		}
	}
}
