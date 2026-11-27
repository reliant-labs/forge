package lint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheFrontend writes a minimal installed frontend: a lint script, an
// eslint config, a source file, a node_modules with npm's install stamp, and
// the build output and tool state the key must ignore.
func cacheFrontend(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"package.json":                    `{"scripts":{"lint":"eslint .","lint:styles":"eslint ."}}`,
		"eslint.config.mjs":               "export default [];\n",
		"tsconfig.json":                   "{}\n",
		"src/app.ts":                      "export const a = 1;\n",
		"src/gen/client_pb.ts":            "export const g = 1;\n",
		"node_modules/.package-lock.json": `{"lockfileVersion":3}`,
		"node_modules/left-pad/index.js":  "module.exports = {};\n",
		".next/cache/x":                   "build\n",
		".next-prod/x":                    "build\n",
		"out/index.html":                  "<html></html>\n",
		"dist/a.js":                       "x\n",
		"coverage/lcov.info":              "x\n",
		"tsconfig.tsbuildinfo":            "{}\n",
		".eslintcache":                    "[]\n",
	} {
		writeCacheFile(t, filepath.Join(dir, rel), body)
	}
	return dir
}

func writeCacheFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubCacheSys pins the forge executable, the tools and the environment the
// key reads, so a test controls every input.
func stubCacheSys(t *testing.T, env ...string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "forge")
	writeCacheFile(t, exe, "forge")
	oldExe, oldLook, oldEnv := lintCacheExecutable, lintCacheLookPath, lintCacheEnviron
	lintCacheExecutable = func() (string, error) { return exe, nil }
	lintCacheLookPath = func(string) (string, error) { return "", errors.New("absent") }
	lintCacheEnviron = func() []string { return env }
	t.Cleanup(func() { lintCacheExecutable, lintCacheLookPath, lintCacheEnviron = oldExe, oldLook, oldEnv })
}

// stubCacheEnv replaces only the environment the key reads.
func stubCacheEnv(t *testing.T, env ...string) {
	t.Helper()
	old := lintCacheEnviron
	lintCacheEnviron = func() []string { return env }
	t.Cleanup(func() { lintCacheEnviron = old })
}

func mustKey(t *testing.T, c *frontendLintCache, dir string, run frontendLintRun) string {
	t.Helper()
	key, _, err := c.fingerprint(dir, run)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return key
}

// The key moves with every lint input and with nothing else.
func TestFrontendLintFingerprintCoversInputsOnly(t *testing.T) {
	stubCacheSys(t)
	dir := cacheFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	run := frontendLintRun{scripts: []string{"lint"}, nmDir: dir}
	base := mustKey(t, c, dir, run)
	if again := mustKey(t, c, dir, run); again != base {
		t.Fatal("the key is not deterministic")
	}

	ignored := []string{
		"node_modules/left-pad/index.js", ".next/cache/x", ".next-prod/x", "out/index.html",
		"dist/a.js", "coverage/lcov.info", "tsconfig.tsbuildinfo", ".eslintcache",
		"src/node_modules/nested/x.js",
	}
	for _, rel := range ignored {
		writeCacheFile(t, filepath.Join(dir, rel), "changed "+rel+"\n")
		if got := mustKey(t, c, dir, run); got != base {
			t.Errorf("editing %s (not a lint input) changed the key", rel)
		}
	}

	inputs := []string{
		"src/app.ts", "src/gen/client_pb.ts", "eslint.config.mjs", "package.json", "tsconfig.json",
		"src/new.ts", "node_modules/.package-lock.json", "package-lock.json",
	}
	prev := base
	for _, rel := range inputs {
		body := "changed " + rel + "\n"
		if rel == "eslint.config.mjs" {
			body = "export default [{}];\n"
		}
		if rel == "package.json" {
			body = `{"private":true,"scripts":{"lint":"eslint .","lint:styles":"eslint ."}}`
		}
		writeCacheFile(t, filepath.Join(dir, rel), body)
		got := mustKey(t, c, dir, run)
		if got == prev {
			t.Errorf("editing %s (a lint input) did not change the key", rel)
		}
		prev = got
	}

	if got := mustKey(t, c, dir, frontendLintRun{scripts: []string{"lint", "lint:styles"}, nmDir: dir}); got == prev {
		t.Error("adding lint:styles to the run did not change the key")
	}
	stubCacheEnv(t, "ESLINT_USE_FLAT_CONFIG=false")
	if got := mustKey(t, c, dir, run); got == prev {
		t.Error("an ESLINT_ variable did not change the key")
	}
	for _, name := range []string{"HOME", "LINT_STRICT", "PROJECT_LINT_CONFIG"} {
		stubCacheEnv(t, name+"=first")
		first := mustKey(t, c, dir, run)
		if first == prev {
			t.Errorf("adding %s did not change the key", name)
		}
		stubCacheEnv(t, name+"=second")
		if got := mustKey(t, c, dir, run); got == first {
			t.Errorf("changing %s did not change the key", name)
		}
		stubCacheEnv(t, name+"=")
		if got := mustKey(t, c, dir, run); got == prev {
			t.Errorf("empty %s was indistinguishable from unset", name)
		}
		stubCacheEnv(t)
		if got := mustKey(t, c, dir, run); got != prev {
			t.Errorf("removing %s did not restore the key", name)
		}
	}
}

// A rebuilt forge (same version string, new binary) is a new judge.
func TestFrontendLintFingerprintKeysOnTheForgeBinary(t *testing.T) {
	stubCacheSys(t)
	dir := cacheFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	run := frontendLintRun{scripts: []string{"lint"}, nmDir: dir}
	base := mustKey(t, c, dir, run)
	exe, _ := lintCacheExecutable()
	writeCacheFile(t, exe, "a rebuilt forge")
	if got := mustKey(t, c, dir, run); got == base {
		t.Error("a rebuilt forge binary did not change the key")
	}
}

// A member whose dependencies are hoisted to a workspace root also keys on
// that root's own files and its other members, but not on the rest of the
// root's tree (a Go module beside the frontends).
func TestFrontendLintFingerprintHoistedWorkspace(t *testing.T) {
	stubCacheSys(t)
	root := t.TempDir()
	for rel, body := range map[string]string{
		"pnpm-workspace.yaml":             "packages:\n  - frontends/*\n  - packages/*\n",
		"package.json":                    `{"private":true}`,
		"pnpm-lock.yaml":                  "lockfileVersion: '9.0'\n",
		"node_modules/.modules.yaml":      "layoutVersion: 5\n",
		"frontends/web/package.json":      `{"scripts":{"lint":"eslint ."}}`,
		"frontends/web/src/app.ts":        "export const a = 1;\n",
		"packages/ui/src/button.ts":       "export const b = 1;\n",
		"internal/handlers/tasks/rpc.go":  "package tasks\n",
		"frontends/web/eslint.config.mjs": "export default [];\n",
	} {
		writeCacheFile(t, filepath.Join(root, rel), body)
	}
	dir := filepath.Join(root, "frontends", "web")
	c := newFrontendLintCache(root, false, func(string) string { return "" })
	run := frontendLintRun{scripts: []string{"lint"}, nmDir: root}
	base := mustKey(t, c, dir, run)

	writeCacheFile(t, filepath.Join(root, "internal/handlers/tasks/rpc.go"), "package tasks // edited\n")
	if got := mustKey(t, c, dir, run); got != base {
		t.Error("a Go edit outside the workspace members changed the frontend's key")
	}
	prev := base
	for _, rel := range []string{"packages/ui/src/button.ts", "pnpm-lock.yaml", "package.json", "node_modules/.modules.yaml"} {
		writeCacheFile(t, filepath.Join(root, rel), "changed\n")
		got := mustKey(t, c, dir, run)
		if got == prev {
			t.Errorf("editing the workspace's %s did not change the key", rel)
		}
		prev = got
	}
}

func TestWorkspaceMembersReadsBothDeclarations(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"apps/web", "apps/admin", "libs/a", "libs/b/c"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCacheFile(t, filepath.Join(root, "package.json"), "{\n\t\"workspaces\": {\"packages\": [\"apps/*\", \"!apps/admin\"]}\n}\n")
	writeCacheFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - libs/**\n")
	var got []string
	for _, m := range workspaceMembers(root) {
		rel, _ := filepath.Rel(root, m)
		got = append(got, filepath.ToSlash(rel))
	}
	want := "apps/admin apps/web libs"
	if strings.Join(got, " ") != want {
		t.Fatalf("members = %v, want %s", got, want)
	}
}

// fakeLintRunner replaces the lane's npm with one that counts its runs and
// returns err (and runs during, when set, inside the run).
type fakeLintRunner struct {
	calls  int
	err    error
	during func()
}

func stubLintRunner(t *testing.T) *fakeLintRunner {
	t.Helper()
	f := &fakeLintRunner{}
	old := runFrontendScript
	runFrontendScript = func(_ context.Context, _ string, _ []string, out io.Writer) error {
		f.calls++
		if f.during != nil {
			f.during()
		}
		if f.err != nil {
			_, _ = io.WriteString(out, "src/app.ts\n  1:1  error  Dependency cycle detected  import/no-cycle\n")
		}
		return f.err
	}
	t.Cleanup(func() { runFrontendScript = old })
	return f
}

func lintDir(c *frontendLintCache, dir string, fix bool) ([]lintJSONFinding, bool) {
	return collectFrontendDirJSON(context.Background(), frontendDepsPolicy{}, "web", dir, false, fix, c)
}

func cachedNote(fs []lintJSONFinding) *lintJSONFinding {
	for i := range fs {
		if fs[i].Cached {
			return &fs[i]
		}
	}
	return nil
}

// Lifecycle hooks may read policy outside the project. Neither a passing
// run nor a subsequent external edit may leave a reusable cached verdict.
func TestFrontendLintCacheLifecycleHooksReadExternalPolicy(t *testing.T) {
	for _, hook := range []string{"prepare", "prelint", "postlint", "prelint:styles", "postlint:styles"} {
		t.Run(hook, func(t *testing.T) {
			stubCacheSys(t)
			dir := cacheFrontend(t)
			c := newFrontendLintCache(dir, false, func(string) string { return "" })
			run := frontendLintRun{scripts: []string{"lint", "lint:styles"}, nmDir: dir}
			external := filepath.Join(t.TempDir(), "lint-policy.json")
			writeCacheFile(t, external, `{"strict":false}`)
			manifest := map[string]any{"scripts": map[string]string{
				"lint":        "eslint .",
				"lint:styles": "eslint .",
				hook:          "cat " + external,
			}}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			writeCacheFile(t, filepath.Join(dir, "package.json"), string(data))
			for _, policy := range []string{`{"strict":false}`, `{"strict":true}`} {
				writeCacheFile(t, external, policy)
				key, _, err := c.fingerprint(dir, run)
				if err == nil || key != "" {
					t.Fatalf("hook with external policy yielded a reusable key: key=%q err=%v", key, err)
				}
				c.record(dir, "previous-pass", 1)
				if c.recordPass(dir, run, "previous-pass") || len(c.load().Frontends) != 0 {
					t.Fatal("hook with external policy retained a cached pass")
				}
			}
		})
	}
}

// A pass is recorded; the same inputs then skip the run with a note that
// says so; an edit runs it again.
func TestFrontendLintCacheSkipsOnMatchAndRunsOnMiss(t *testing.T) {
	stubCacheSys(t)
	r := stubLintRunner(t)
	dir := cacheFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })

	fs, gated := lintDir(c, dir, false)
	if gated || r.calls != 1 || cachedNote(fs) != nil {
		t.Fatalf("first run: want one clean, uncached run; calls=%d gated=%v %+v", r.calls, gated, fs)
	}

	fs, gated = lintDir(c, dir, false)
	if gated || r.calls != 1 {
		t.Fatalf("unchanged inputs ran npm again: calls=%d gated=%v", r.calls, gated)
	}
	n := cachedNote(fs)
	if n == nil || n.Rule != ruleRanNote || n.Severity != lintSevInfo {
		t.Fatalf("a cached pass must say so, as an info ran note: %+v", fs)
	}
	for _, want := range []string{"web: unchanged since its last pass", "cached pass", "--no-cache"} {
		if !strings.Contains(n.Message, want) {
			t.Errorf("cached note %q lacks %q", n.Message, want)
		}
	}

	writeCacheFile(t, filepath.Join(dir, "src/app.ts"), "export const a = 2;\n")
	if fs, _ := lintDir(c, dir, false); r.calls != 2 || cachedNote(fs) != nil {
		t.Fatalf("an edited source did not re-run the lane: calls=%d", r.calls)
	}
}

// Local config imports may escape the hashed frontend, including through
// an in-tree helper. Until their closure can be resolved, always run lint.
func TestFrontendLintCacheLocalConfigImportsRunAfterExternalEdit(t *testing.T) {
	for _, indirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "indirect"}[indirect], func(t *testing.T) {
			stubCacheSys(t)
			r := stubLintRunner(t)
			root := t.TempDir()
			dir := filepath.Join(root, "frontends", "web")
			writeCacheFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"lint":"eslint ."}}`)
			writeCacheFile(t, filepath.Join(dir, "node_modules", ".package-lock.json"), "{}")
			external := filepath.Join(root, "eslint.shared.mjs")
			writeCacheFile(t, external, "export default [];\n")
			config := "export { default } from '../../eslint.shared.mjs';\n"
			if indirect {
				writeCacheFile(t, filepath.Join(dir, "helper.mjs"), config)
				config = "export { default } from './helper.mjs';\n"
			}
			writeCacheFile(t, filepath.Join(dir, "eslint.config.mjs"), config)
			c := newFrontendLintCache(root, false, func(string) string { return "" })
			for i := 1; i <= 2; i++ {
				fs, gated := lintDir(c, dir, false)
				if gated || r.calls != i || cachedNote(fs) != nil {
					t.Fatalf("run %d: want uncached pass, calls=%d gated=%v findings=%+v", i, r.calls, gated, fs)
				}
			}
			writeCacheFile(t, external, "export default [{ rules: { semi: 'error' } }];\n")
			r.err = errors.New("external config now rejects the source")
			fs, gated := lintDir(c, dir, false)
			if !gated || r.calls != 3 || cachedNote(fs) != nil {
				t.Fatalf("external edit: want fresh failure, calls=%d gated=%v findings=%+v", r.calls, gated, fs)
			}
		})
	}
}

// Project scripts and npm's implicit lifecycle hooks can read inputs outside
// the frontend without importing them through an ESLint config.
func TestFrontendLintCacheExternalScriptInputsAlwaysRun(t *testing.T) {
	for _, script := range []string{"lint", "prelint", "postlint"} {
		t.Run(script, func(t *testing.T) {
			stubCacheSys(t)
			r := stubLintRunner(t)
			root := t.TempDir()
			dir := filepath.Join(root, "frontends", "web")
			scripts := map[string]string{"lint": "eslint ."}
			scripts[script] = "node check.mjs"
			manifest, err := json.Marshal(map[string]any{"scripts": scripts})
			if err != nil {
				t.Fatal(err)
			}
			writeCacheFile(t, filepath.Join(dir, "package.json"), string(manifest))
			writeCacheFile(t, filepath.Join(dir, "eslint.config.mjs"), "export default [];\n")
			writeCacheFile(t, filepath.Join(dir, "node_modules", ".package-lock.json"), "{}")
			writeCacheFile(t, filepath.Join(dir, "check.mjs"), "import { readFileSync } from 'node:fs';\nif (readFileSync('../../shared/lint-policy', 'utf8') !== 'pass') process.exit(1);\n")
			external := filepath.Join(root, "shared", "lint-policy")
			writeCacheFile(t, external, "pass")
			// Model the script's file read without requiring node/npm in this test.
			r.during = func() {
				body, err := os.ReadFile(external)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != "pass" {
					r.err = errors.New("external lint policy rejected the source")
				}
			}
			c := newFrontendLintCache(root, false, func(string) string { return "" })
			for i := 1; i <= 2; i++ {
				fs, gated := lintDir(c, dir, false)
				if gated || r.calls != i || cachedNote(fs) != nil {
					t.Fatalf("run %d: want uncached pass, calls=%d gated=%v findings=%+v", i, r.calls, gated, fs)
				}
			}
			writeCacheFile(t, external, "fail")
			fs, gated := lintDir(c, dir, false)
			if !gated || r.calls != 3 || cachedNote(fs) != nil {
				t.Fatalf("external edit: want fresh failure, calls=%d gated=%v findings=%+v", r.calls, gated, fs)
			}
		})
	}
}

// Only passes are cached: a failing run runs again next time, and a failure
// on inputs a pass was recorded for drops that record.
func TestFrontendLintCacheNeverCachesFailure(t *testing.T) {
	stubCacheSys(t)
	r := stubLintRunner(t)
	dir := cacheFrontend(t)
	root := t.TempDir()
	c := newFrontendLintCache(root, false, func(string) string { return "" })

	r.err = errors.New("exit status 1")
	for i := 1; i <= 2; i++ {
		if _, gated := lintDir(c, dir, false); !gated || r.calls != i {
			t.Fatalf("failing run %d: want it run and gated, calls=%d gated=%v", i, r.calls, gated)
		}
	}

	// A pass on these inputs, then a failure on the SAME inputs (forced
	// with --no-cache): the next consulting run must lint, not replay the
	// older pass.
	r.err = nil
	lintDir(c, dir, false)
	r.err = errors.New("exit status 1")
	if _, gated := lintDir(newFrontendLintCache(root, true, func(string) string { return "" }), dir, false); !gated {
		t.Fatal("the forced run should have failed")
	}
	r.err = nil
	calls := r.calls
	if fs, _ := lintDir(c, dir, false); r.calls != calls+1 || cachedNote(fs) != nil {
		t.Fatalf("a failure did not drop the recorded pass: calls=%d→%d", calls, r.calls)
	}
}

// --no-cache, FORGE_LINT_NO_CACHE and CI run the lane every time, and still
// record the pass for a later run that consults.
func TestFrontendLintCacheForceRuns(t *testing.T) {
	for name, mk := range map[string]func(root string) *frontendLintCache{
		"--no-cache": func(root string) *frontendLintCache {
			return newFrontendLintCache(root, true, func(string) string { return "" })
		},
		lintNoCacheEnv: func(root string) *frontendLintCache {
			return newFrontendLintCache(root, false, func(k string) string { return map[string]string{lintNoCacheEnv: "1"}[k] })
		},
		"CI": func(root string) *frontendLintCache {
			return newFrontendLintCache(root, false, func(k string) string { return map[string]string{"CI": "true"}[k] })
		},
	} {
		t.Run(name, func(t *testing.T) {
			stubCacheSys(t)
			r := stubLintRunner(t)
			dir := cacheFrontend(t)
			root := t.TempDir()
			forced := mk(root)
			if forced.consult || forced.why == "" {
				t.Fatalf("%s: want a cache that does not consult, with its reason; got %+v", name, forced)
			}
			lintDir(forced, dir, false)
			lintDir(forced, dir, false)
			if r.calls != 2 {
				t.Fatalf("%s: want every run to lint, calls=%d", name, r.calls)
			}
			fs, _ := lintDir(newFrontendLintCache(root, false, func(string) string { return "" }), dir, false)
			if r.calls != 2 || cachedNote(fs) == nil {
				t.Fatalf("%s: the forced pass was not recorded for a consulting run: calls=%d", name, r.calls)
			}
		})
	}
	if c := newFrontendLintCache(t.TempDir(), false, func(k string) string { return map[string]string{"CI": "false"}[k] }); !c.consult {
		t.Error("CI=false must not turn the cache off")
	}
}

// --fix exists to run eslint --fix, so it never skips; and a run that
// rewrote its inputs (an edit during the run, or the fix itself) records
// nothing.
func TestFrontendLintCacheFixAndEditsDuringTheRun(t *testing.T) {
	stubCacheSys(t)
	r := stubLintRunner(t)
	dir := cacheFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	lintDir(c, dir, false)
	if fs, _ := lintDir(c, dir, true); r.calls != 2 || cachedNote(fs) != nil {
		t.Fatalf("--fix skipped eslint on a cached pass: calls=%d", r.calls)
	}

	writeCacheFile(t, filepath.Join(dir, "src/app.ts"), "export const a = 3;\n")
	r.during = func() { writeCacheFile(t, filepath.Join(dir, "src/app.ts"), "export const fixed = 1;\n") }
	lintDir(c, dir, false)
	r.during = nil
	calls := r.calls
	if lintDir(c, dir, false); r.calls != calls+1 {
		t.Fatal("a run whose inputs changed while it ran recorded a pass for them")
	}
}

// A run with no cache (every hand-built run, every non-sweep driver) lints
// every time and writes no record.
func TestFrontendLintNilCacheAlwaysRuns(t *testing.T) {
	r := stubLintRunner(t)
	dir := cacheFrontend(t)
	lintDir(nil, dir, false)
	lintDir(nil, dir, false)
	if r.calls != 2 {
		t.Fatalf("calls=%d, want 2", r.calls)
	}
}

// The cached pass is named in the verdict, --quiet included, and is
// additive in --json.
func TestFrontendLintCachedPassIsNeverSilent(t *testing.T) {
	note := cachedPassNote("web", frontendLintPass{Files: 12}, []string{"lint"})
	steps := []linterStep{{
		name: "frontend lint", gates: true, scope: scopeWholeProject,
		shouldRun: func(*lintRunCtx) (bool, string) { return true, "" },
		collect: func(*lintRunCtx) ([]lintJSONFinding, bool, error) {
			return []lintJSONFinding{note}, false, nil
		},
	}}
	for _, quiet := range []bool{true, false} {
		var out bytes.Buffer
		if err := renderLintOutcomes(&out, collectLintOutcomesFrom(&lintRunCtx{ctx: context.Background()}, steps), quiet, nil); err != nil {
			t.Fatalf("quiet=%v: %v", quiet, err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		last := lines[len(lines)-1]
		if !strings.Contains(last, "frontend lint (web) unchanged since its last pass, cached pass") {
			t.Errorf("quiet=%v: the verdict does not name the cached pass: %q", quiet, out.String())
		}
	}

	data, err := json.Marshal(note)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"cached":true`) || !strings.Contains(string(data), `"rule":"ran"`) {
		t.Errorf("cached note JSON = %s", data)
	}
	if data, _ := json.Marshal(ranNote("web: npm run lint passed")); strings.Contains(string(data), "cached") {
		t.Errorf("an ordinary ran note grew a cached field: %s", data)
	}
}

// --no-cache reaches the run: the flag is declared and plumbed into --json's
// options, and the record lives under .forge/.
func TestLintNoCacheFlagIsPlumbed(t *testing.T) {
	cmd := newCmd(nil)
	if cmd.Flags().Lookup("no-cache") == nil {
		t.Fatal("forge lint has no --no-cache flag")
	}
	if opts := jsonRunOptions(lintFlags{noCache: true}, nil, nil, nil, nil, nil); !opts.noCache {
		t.Error("--no-cache is not plumbed into --json's run")
	}
	c := newFrontendLintCache("/p", false, func(string) string { return "" })
	if got := filepath.ToSlash(c.path()); got != "/p/.forge/lint-cache.json" {
		t.Errorf("record path = %s", got)
	}
}

// [B1] An edit landing exactly between the post-run hash and the record
// write leaves no record: the re-hash after the write deletes it, so the
// next run lints instead of replaying a pass of inputs eslint never saw.
func TestFrontendLintCacheEditAtTheRecordBoundaryLeavesNoPass(t *testing.T) {
	stubCacheSys(t)
	r := stubLintRunner(t)
	dir := cacheFrontend(t)
	root := t.TempDir()
	c := newFrontendLintCache(root, false, func(string) string { return "" })

	// An earlier pass of the original tree is on record.
	lintDir(c, dir, false)
	writeCacheFile(t, filepath.Join(dir, "src/app.ts"), "export const a = 2;\n")

	old := lintCacheBeforeRecord
	hooked := 0
	lintCacheBeforeRecord = func() {
		hooked++
		writeCacheFile(t, filepath.Join(dir, "src/app.ts"), "export const unjudged = 3;\n")
	}
	lintDir(c, dir, false)
	lintCacheBeforeRecord = old
	if hooked != 1 || r.calls != 2 {
		t.Fatalf("want the second run to lint and reach the record boundary once; calls=%d hooked=%d", r.calls, hooked)
	}
	if rec := c.load(); len(rec.Frontends) != 0 {
		t.Fatalf("a record survived an edit at the record boundary: %+v", rec.Frontends)
	}
	if fs, _ := lintDir(c, dir, false); r.calls != 3 || cachedNote(fs) != nil {
		t.Fatalf("the unjudged tree was replayed as a cached pass: calls=%d", r.calls)
	}
	if fs, _ := lintDir(c, dir, false); r.calls != 3 || cachedNote(fs) == nil {
		t.Fatalf("a stable tree's pass was not recorded once it was judged: calls=%d", r.calls)
	}
}

// recordPass alone: a mismatch at either check leaves nothing; a stable
// tree keeps its record.
func TestFrontendLintRecordPassRevalidatesAfterTheWrite(t *testing.T) {
	stubCacheSys(t)
	dir := cacheFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	run := frontendLintRun{scripts: []string{"lint"}, nmDir: dir}
	key := mustKey(t, c, dir, run)

	if !c.recordPass(dir, run, key) {
		t.Fatal("a stable tree's pass was not recorded")
	}
	if _, ok := c.hit(dir, key); !ok {
		t.Fatal("the recorded pass does not hit")
	}

	old := lintCacheBeforeRecord
	t.Cleanup(func() { lintCacheBeforeRecord = old })
	lintCacheBeforeRecord = func() { writeCacheFile(t, filepath.Join(dir, "eslint.config.mjs"), "export default [{}];\n") }
	if c.recordPass(dir, run, key) {
		t.Fatal("recordPass kept a record for inputs edited before its write")
	}
	if _, ok := c.hit(dir, key); ok {
		t.Fatal("the stale key still hits")
	}

	lintCacheBeforeRecord = func() {}
	if c.recordPass(dir, run, key) {
		t.Fatal("recordPass recorded a key the tree no longer hashes to")
	}
}

// hoistedWorkspace is a workspace root that hoists frontends/web's
// dependencies, with the given extra files.
func hoistedWorkspace(t *testing.T, files map[string]string) (root, dir string, c *frontendLintCache, run frontendLintRun) {
	t.Helper()
	root = t.TempDir()
	base := map[string]string{
		"package.json":               `{"private":true}`,
		"pnpm-lock.yaml":             "lockfileVersion: '9.0'\n",
		"node_modules/.modules.yaml": "layoutVersion: 5\n",
		"frontends/web/package.json": `{"scripts":{"lint":"eslint ."}}`,
		"frontends/web/src/app.ts":   "export const a = 1;\n",
		"packages/ui/src/button.ts":  "export const b = 1;\n",
	}
	for k, v := range files {
		base[k] = v
	}
	for rel, body := range base {
		writeCacheFile(t, filepath.Join(root, rel), body)
	}
	dir = filepath.Join(root, "frontends", "web")
	return root, dir, newFrontendLintCache(root, false, func(string) string { return "" }), frontendLintRun{scripts: []string{"lint"}, nmDir: root}
}

// [B2] Creating or removing a workspace member — even an empty directory,
// under a pattern that already matched — changes the key, and so does each
// way of declaring membership.
func TestFrontendLintFingerprintWorkspaceMembership(t *testing.T) {
	stubCacheSys(t)
	for name, pattern := range map[string]string{
		"single star": "packages/*",
	} {
		t.Run(name, func(t *testing.T) {
			root, dir, c, run := hoistedWorkspace(t, map[string]string{
				"pnpm-workspace.yaml": "packages:\n  - frontends/*\n  - " + pattern + "\n",
			})
			base := mustKey(t, c, dir, run)

			member := filepath.Join(root, "packages", "new")
			if err := os.Mkdir(member, 0o755); err != nil {
				t.Fatal(err)
			}
			created := mustKey(t, c, dir, run)
			if created == base {
				t.Fatal("creating an empty member directory did not change the key")
			}
			writeCacheFile(t, filepath.Join(member, "src", "x.ts"), "export const x = 1;\n")
			filled := mustKey(t, c, dir, run)
			if filled == created {
				t.Fatal("adding a file to the new member did not change the key")
			}
			if err := os.RemoveAll(member); err != nil {
				t.Fatal(err)
			}
			if got := mustKey(t, c, dir, run); got == filled || got != base {
				t.Fatalf("removing the member: want the original key back (changed=%v)", got != filled)
			}
			if err := os.RemoveAll(filepath.Join(root, "packages", "ui")); err != nil {
				t.Fatal(err)
			}
			if got := mustKey(t, c, dir, run); got == base {
				t.Fatal("removing an existing member did not change the key")
			}
		})
	}

	t.Run("nested pattern", func(t *testing.T) {
		root, dir, c, run := hoistedWorkspace(t, map[string]string{
			"pnpm-workspace.yaml": "packages:\n  - frontends/*\n  - apps/*/web\n",
			"apps/admin/README":   "an app without a web member yet\n",
		})
		base := mustKey(t, c, dir, run)
		if err := os.Mkdir(filepath.Join(root, "apps", "admin", "web"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := mustKey(t, c, dir, run); got == base {
			t.Fatal("a member appearing under apps/*/web did not change the key")
		}
	})

	t.Run("declarations", func(t *testing.T) {
		root, dir, c, run := hoistedWorkspace(t, map[string]string{
			"pnpm-workspace.yaml": "packages:\n  - frontends/*\n  - packages/*\n",
		})
		prev := mustKey(t, c, dir, run)
		for _, step := range []struct{ file, body string }{
			{"pnpm-workspace.yaml", "packages:\n  - frontends/*\n"},
			{"pnpm-workspace.yaml", "packages:\n  - frontends/*\n  - packages/*\n  - tools/*\n"},
			{"package.json", `{"private":true,"workspaces":["tools/*"]}`},
			{"package.json", `{"private":true,"workspaces":{"packages":["tools/*","libs/*"]}}`},
		} {
			writeCacheFile(t, filepath.Join(root, step.file), step.body)
			got := mustKey(t, c, dir, run)
			if got == prev {
				t.Errorf("declaring %q in %s did not change the key", step.body, step.file)
			}
			prev = got
		}
	})
}

// Unsupported workspace syntax must never yield a reusable key, including
// before and after nested workspace members are created or removed.
func TestFrontendLintCacheUnsupportedWorkspacePatterns(t *testing.T) {
	stubCacheSys(t)
	for _, pattern := range []string{"packages/**", "apps/*/web/**", "!packages/old", "packages/{ui,core}", "packages/+(ui|core)"} {
		t.Run(pattern, func(t *testing.T) {
			root, dir, c, run := hoistedWorkspace(t, map[string]string{
				"pnpm-workspace.yaml": "packages:\n  - frontends/*\n  - '" + pattern + "'\n",
			})
			assertUncached := func() {
				t.Helper()
				key, _, err := c.fingerprint(dir, run)
				if err == nil || key != "" {
					t.Fatalf("unsupported pattern yielded a reusable key: key=%q err=%v", key, err)
				}
				if c.recordPass(dir, run, "previous-pass") {
					t.Fatal("unsupported workspace recorded a pass")
				}
				if len(c.load().Frontends) != 0 {
					t.Fatal("a stale pass survived unsupported workspace syntax")
				}
			}
			c.record(dir, "previous-pass", 1)
			assertUncached()
			for _, rel := range []string{"packages/nested/member", "apps/admin/web/nested/member"} {
				member := filepath.Join(root, filepath.FromSlash(rel))
				writeCacheFile(t, filepath.Join(member, "package.json"), `{"name":"nested"}`)
				assertUncached()
				if err := os.RemoveAll(member); err != nil {
					t.Fatal(err)
				}
				assertUncached()
			}
		})
	}
}

func TestResolveWorkspaceNormalizesDeclarations(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"packages/ui", "apps/x/web"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCacheFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - ' packages/* '\n  - apps/*/web\n  - '!packages/old'\n")
	writeCacheFile(t, filepath.Join(root, "package.json"), `{"workspaces":["packages/*"]}`)
	ws := resolveWorkspace(root)
	rel := func(ps []string) string {
		var out []string
		for _, p := range ps {
			r, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return strings.Join(out, " ")
	}
	if got := strings.Join(ws.patterns, " "); got != "!packages/old apps/*/web packages/*" {
		t.Errorf("patterns = %q", got)
	}
	if got := rel(ws.members); got != "apps/x/web packages/ui" {
		t.Errorf("members = %q", got)
	}
	if got := rel(ws.parents); got != "apps packages" {
		t.Errorf("glob parents = %q", got)
	}
}
