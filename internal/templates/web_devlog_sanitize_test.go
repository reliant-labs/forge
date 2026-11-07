package templates

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The web dev-log receivers (Vite plugin, Next.js route) print client-supplied
// text to stdout, which forge tees into .forge/logs/<env>/frontend_<name>.log.
// These tests EXECUTE the shipped handleDevLogBody under Node's native
// TypeScript support, so the sanitising is proven by behaviour: an embedded
// newline cannot forge a line, ANSI/CR/C0/C1 never reach the log, and ordinary
// single-line output is unchanged. The expectations are the same ones the
// native (Metro) receiver must meet.

const webDevlogSanitizeHarness = `
import assert from "node:assert/strict";

const { handleDevLogBody } = await import(process.argv[2]);

const one = (level, msg) => handleDevLogBody(JSON.stringify({ entries: [{ level, msg }] })).lines;

// Normal single-line output is unchanged, tabs included.
assert.deepEqual(one("info", "hello world"), ["[browser:info] hello world"]);
assert.deepEqual(one("error", "a\tb"), ["[browser:error] a\tb"]);
assert.deepEqual(one("info", ""), ["[browser:info] "]);

// A newline cannot forge a line: every continuation line carries the prefix.
assert.deepEqual(one("error", "boom\n[up] pretend backend line"), [
  "[browser:error] boom\n[browser:error] [up] pretend backend line",
]);
assert.deepEqual(one("error", "x\n[browser:error] forged"), [
  "[browser:error] x\n[browser:error] [browser:error] forged",
]);
// Stack traces become grep-able by level.
assert.deepEqual(one("error", "Error: x\n    at a (f.js:1:1)\n    at b (f.js:2:2)"), [
  "[browser:error] Error: x\n[browser:error]     at a (f.js:1:1)\n[browser:error]     at b (f.js:2:2)",
]);
// A trailing newline yields a trailing prefixed empty line, never a bare one.
assert.deepEqual(one("warn", "a\n"), ["[browser:warn] a\n[browser:warn] "]);

// ANSI escape sequences are removed whole, not just their ESC byte.
assert.deepEqual(one("info", "\x1b[31mred\x1b[0m \x1b[2K\x1b[?25lok"), ["[browser:info] red ok"]);

// CR cannot overwrite the prefix; \r\n collapses to \n.
assert.deepEqual(one("info", "visible\r[up] overwrite"), ["[browser:info] visible[up] overwrite"]);
assert.deepEqual(one("info", "a\r\nb"), ["[browser:info] a\n[browser:info] b"]);

// Other C0 / DEL / C1 controls are dropped.
assert.deepEqual(one("info", "a\x00b\x07c\x08d\x0be\x0cf\x1bg\x7fh\x80i\x9fj"), ["[browser:info] abcdefghij"]);

// A bare ESC not forming a CSI sequence is still dropped.
assert.deepEqual(one("info", "a\x1bb"), ["[browser:info] ab"]);

// Sanitising applies to v1 single entries too, and to every entry in a batch.
let r = handleDevLogBody(JSON.stringify({ level: "warn", msg: "l\nx" }));
assert.equal(r.status, 204);
assert.deepEqual(r.lines, ["[browser:warn] l\n[browser:warn] x"]);
r = handleDevLogBody(JSON.stringify({ entries: [{ level: "info", msg: "a\nb" }, { level: "error", msg: "\x1b[1mc" }] }));
assert.deepEqual(r.lines, ["[browser:info] a\n[browser:info] b", "[browser:error] c"]);

// Existing limits and protocol are untouched.
assert.deepEqual(one("fatal", "x"), ["[browser:log] x"]);
assert.deepEqual(one("info", "a".repeat(9000)), ["[browser:info] " + "a".repeat(8000) + "… (truncated)"]);
for (const body of ["{nope", "", "null", "42"]) {
  r = handleDevLogBody(body);
  assert.equal(r.status, 400);
  assert.deepEqual(r.lines, ["[browser:warn] [forge-devlog] dropped malformed post"]);
}
r = handleDevLogBody("z".repeat(1024 * 1024 + 10));
assert.equal(r.status, 413);
assert.deepEqual(r.lines, ["[browser:warn] [forge-devlog] dropped oversized post (1048586 bytes)"]);

console.log("ok");
`

// nodeWithTypeStripping returns node, skipping when it cannot import .ts files
// (Node < 22.18 has no default type stripping).
func nodeWithTypeStripping(t *testing.T) string {
	t.Helper()
	node := requireNode(t)
	if err := exec.Command(node, "-e", "process.exit(process.features.typescript ? 0 : 1)").Run(); err != nil {
		t.Skip("node lacks native TypeScript support")
	}
	return node
}

func TestWebDevLogReceiversSanitizeClientText(t *testing.T) {
	node := nodeWithTypeStripping(t)
	for name, tmpl := range map[string]string{
		"vite":   "vite-spa/src/lib/devlog-plugin.ts",
		"nextjs": "nextjs/src/app/%5F_forge/log/route.ts",
	} {
		t.Run(name, func(t *testing.T) {
			src, err := FrontendTemplates().Get(tmpl)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeHarnessFile(t, root, "receiver.ts", string(src))
			writeHarnessFile(t, root, "check.mjs", webDevlogSanitizeHarness)
			out, err := exec.Command(node, filepath.Join(root, "check.mjs"), filepath.Join(root, "receiver.ts")).CombinedOutput()
			if err != nil {
				t.Fatalf("receiver behaviour: %v\n%s", err, out)
			}
		})
	}
}
