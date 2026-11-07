package templates

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The owned metro.config.js maps a failed `.js` import specifier to its
// TypeScript source (generated protobuf-es files import "./x_pb.js"; Metro does
// not). These tests EXECUTE the rendered config's resolver under Node with a
// stubbed Metro context, so the contract — only on failure, never rewriting a
// success, never touching node_modules importers, chaining an app resolver — is
// proven by behaviour. A real Metro bundling a generated _pb file is proven by
// the mobile consumer conformance e2e.

const metroResolveHarness = `
const assert = require("node:assert/strict");
const path = require("node:path");

class FailedToResolvePathError extends Error { constructor() { super("nope"); this.candidates = {}; } }
class FailedToResolveNameError extends Error { constructor() { super("nope"); this.extraPaths = []; } }

// files is the set of specifiers the "default resolver" can resolve.
function makeContext(files, origin, log) {
  return {
    originModulePath: origin,
    resolveRequest(ctx, moduleName, platform) {
      log.push(moduleName);
      if (files.has(moduleName)) return { type: "sourceFile", filePath: "/r/" + moduleName };
      if (moduleName === "boom") throw new TypeError("not a resolution error");
      throw moduleName.startsWith(".") ? new FailedToResolvePathError() : new FailedToResolveNameError();
    },
  };
}

const app = path.join("/proj", "src", "gen", "services", "widget_pb.ts");
const inPkg = path.join("/proj", "node_modules", "dep", "index.js");

function run(config, files, specifier, origin) {
  const log = [];
  const ctx = makeContext(new Set(files), origin ?? app, log);
  let result, error;
  try { result = config.resolver.resolveRequest(ctx, specifier, "ios"); } catch (e) { error = e; }
  return { result, error, log };
}

const config = require("./metro.config.js");
assert.equal(typeof config.resolver.resolveRequest, "function");

// The generated-code case: a relative .js import whose only file is .ts.
let r = run(config, ["../../../forge/v1/forge_pb.ts"], "../../../forge/v1/forge_pb.js");
assert.equal(r.result.filePath, "/r/../../../forge/v1/forge_pb.ts");
assert.deepEqual(r.log, ["../../../forge/v1/forge_pb.js", "../../../forge/v1/forge_pb.ts"]);

// .tsx is tried after .ts.
r = run(config, ["./Screen.tsx"], "./Screen.js");
assert.equal(r.result.filePath, "/r/./Screen.tsx");
assert.deepEqual(r.log, ["./Screen.js", "./Screen.ts", "./Screen.tsx"]);

// .mjs -> .mts.
r = run(config, ["./x.mts"], "./x.mjs");
assert.equal(r.result.filePath, "/r/./x.mts");

// A workspace-package subpath import (a bare specifier) gets the same treatment.
r = run(config, ["@consumer/api/services/widget/v1/widget_pb.ts"], "@consumer/api/services/widget/v1/widget_pb.js");
assert.equal(r.result.filePath, "/r/@consumer/api/services/widget/v1/widget_pb.ts");

// A resolution that succeeds is returned as is: no retry, no rewrite.
r = run(config, ["./real.js", "./real.ts"], "./real.js");
assert.equal(r.result.filePath, "/r/./real.js");
assert.deepEqual(r.log, ["./real.js"]);

// Specifiers that do not end in .js/.mjs/.cjs are never retried.
r = run(config, [], "./missing");
assert.ok(r.error instanceof FailedToResolvePathError);
assert.deepEqual(r.log, ["./missing"]);
r = run(config, [], "./data.json");
assert.ok(r.error instanceof FailedToResolvePathError);
assert.deepEqual(r.log, ["./data.json"]);

// Nothing to fall back to: the ORIGINAL error is rethrown, after every retry.
r = run(config, [], "./gone.js");
assert.ok(r.error instanceof FailedToResolvePathError);
assert.deepEqual(r.log, ["./gone.js", "./gone.ts", "./gone.tsx"]);
r = run(config, [], "left-pad/missing.js");
assert.ok(r.error instanceof FailedToResolveNameError);

// A module imported from inside node_modules is left to Metro: a package that
// ships real .js must not be probed for a sibling .ts.
r = run(config, ["./index.ts"], "./index.js", inPkg);
assert.ok(r.error instanceof FailedToResolvePathError);
assert.deepEqual(r.log, ["./index.js"]);

// An error that is not a resolution failure is never swallowed, first try or retry.
r = run(config, [], "boom");
assert.ok(r.error instanceof TypeError);
{
  const log = [];
  const ctx = makeContext(new Set(), app, log);
  ctx.resolveRequest = (c, m) => { log.push(m); if (m.endsWith(".js")) throw new FailedToResolvePathError(); throw new RangeError("retry blew up"); };
  assert.throws(() => config.resolver.resolveRequest(ctx, "./a.js", "ios"), RangeError);
}

console.log("ok");
`

const metroResolveChainHarness = `
const assert = require("node:assert/strict");
class FailedToResolvePathError extends Error { constructor() { super("nope"); this.candidates = {}; } }

const config = require("./metro.config.js");
const log = [];
const ctx = {
  originModulePath: "/proj/src/a.ts",
  resolveRequest() { throw new Error("the app's resolver must be used instead of context.resolveRequest"); },
};

// The app's resolver runs first (and is what the retry goes through).
const out = config.resolver.resolveRequest(ctx, "./x.js", "ios");
assert.deepEqual(log, ["app:./x.js", "app:./x.ts"]);
assert.equal(out.filePath, "/app/./x.ts");
console.log("ok");
`

func TestNativeMetroResolvesTSExtensionForJSSpecifiers(t *testing.T) {
	node := requireNode(t)
	root := t.TempDir()
	stubExpoMetroConfig(t, root)
	writeHarnessFile(t, root, "metro.config.js", renderMetroConfig(t))
	writeHarnessFile(t, root, "check.cjs", metroResolveHarness)
	if out, err := exec.Command(node, filepath.Join(root, "check.cjs")).CombinedOutput(); err != nil {
		t.Fatalf("resolver behaviour: %v\n%s", err, out)
	}
}

// An app (or Expo CLI) resolver already on config.resolver.resolveRequest must
// keep running: the wrapper chains it rather than replacing it.
func TestNativeMetroResolveChainsPreviousResolveRequest(t *testing.T) {
	node := requireNode(t)
	root := t.TempDir()
	stubExpoMetroConfig(t, root)

	cfg := renderMetroConfig(t)
	const anchor = "const config = getDefaultConfig(__dirname);"
	if !strings.Contains(cfg, anchor) {
		t.Fatalf("metro.config.js no longer declares %q — update this test's injection point", anchor)
	}
	cfg = strings.Replace(cfg, anchor, anchor+`
globalThis.__log = [];
config.resolver.resolveRequest = (ctx, name) => {
  globalThis.__log.push("app:" + name);
  if (name.endsWith(".js")) { const e = new Error("x"); e.candidates = {}; Object.defineProperty(e, "constructor", { value: { name: "FailedToResolvePathError" } }); throw e; }
  return { type: "sourceFile", filePath: "/app/" + name };
};`, 1)
	harness := strings.Replace(metroResolveChainHarness, "const log = [];", "const log = globalThis.__log;", 1)
	writeHarnessFile(t, root, "metro.config.js", cfg)
	writeHarnessFile(t, root, "check.cjs", harness)
	if out, err := exec.Command(node, filepath.Join(root, "check.cjs")).CombinedOutput(); err != nil {
		t.Fatalf("chaining: %v\n%s", err, out)
	}
}

// The template must still carry the other two owned blocks this one sits beside.
func TestNativeMetroConfigKeepsDevlinkAndDevlogBlocks(t *testing.T) {
	t.Parallel()
	cfg := renderMetroConfig(t)
	for _, want := range []string{"runtimeDir", "enhanceMiddleware", "DEV_LOG_ENDPOINT", "resolveRequest"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("metro.config.js lost %q", want)
		}
	}
}
