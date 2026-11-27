package lint

import (
	"os"
	"path/filepath"
	"testing"
)

// depsFrontend is an installed frontend in pnpm's layout: node_modules/<name>
// symlinks into a store directory whose own dependencies are its siblings.
// The frontend lists one eslint plugin and one unrelated library, and its
// config imports a shared config; the plugin depends on another plugin and
// on espree, and eslint on an espree-like package nothing else names.
func depsFrontend(t *testing.T) string {
	t.Helper()
	dir := cacheFrontend(t)
	writeCacheFile(t, filepath.Join(dir, "package.json"),
		`{"scripts":{"lint":"eslint ."},"devDependencies":{"eslint-plugin-foo":"1.0.0","left-pad":"1.0.0"}}`)
	writeCacheFile(t, filepath.Join(dir, "eslint.config.mjs"),
		"import next from \"eslint-config-bar/core\";\nimport local from \"./local.mjs\";\nexport default [...next, local];\n")
	store := filepath.Join(dir, "node_modules", ".pnpm", "eslint-plugin-foo@1.0.0", "node_modules")
	for rel, body := range map[string]string{
		"eslint-plugin-foo/package.json":            `{"name":"eslint-plugin-foo","dependencies":{"eslint-plugin-transitive":"1","espree":"1"}}`,
		"eslint-plugin-foo/lib/rules/no-thing.js":   "module.exports = {};\n",
		"eslint-plugin-foo/dist/index.js":           "module.exports = {};\n",
		"eslint-plugin-foo/node_modules/x/index.js": "nested\n",
		"eslint-plugin-transitive/package.json":     `{"name":"eslint-plugin-transitive"}`,
		"eslint-plugin-transitive/index.js":         "module.exports = {};\n",
		"espree/package.json":                       `{"name":"espree"}`,
		"espree/index.js":                           "module.exports = {};\n",
	} {
		writeCacheFile(t, filepath.Join(store, rel), body)
	}
	if err := os.Symlink(filepath.Join(store, "eslint-plugin-foo"), filepath.Join(dir, "node_modules", "eslint-plugin-foo")); err != nil {
		t.Skipf("symlinks unavailable here (%v): the test needs pnpm's layout", err)
	}
	// eslint itself, in the same layout, with a runtime dependency nothing
	// else names: reached only through eslint's own dependencies.
	for rel, body := range map[string]string{
		"eslint/package.json":         `{"name":"eslint","dependencies":{"espree-like":"1"},"peerDependencies":{"jiti":"*"}}`,
		"eslint/lib/linter.js":        "module.exports = {};\n",
		"espree-like/package.json":    `{"name":"espree-like"}`,
		"espree-like/dist/espree.cjs": "module.exports = {};\n",
	} {
		writeCacheFile(t, filepath.Join(eslintStore(dir), rel), body)
	}
	if err := os.Symlink(filepath.Join(eslintStore(dir), "eslint"), filepath.Join(dir, "node_modules", "eslint")); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"node_modules/eslint-config-bar/package.json": `{"name":"eslint-config-bar"}`,
		"node_modules/eslint-config-bar/core.js":      "module.exports = [];\n",
		"node_modules/typescript/lib/typescript.js":   "// tsc\n",
	} {
		writeCacheFile(t, filepath.Join(dir, rel), body)
	}
	return dir
}

// eslintStore is where depsFrontend installs eslint and its dependency.
func eslintStore(dir string) string {
	return filepath.Join(dir, "node_modules", ".pnpm", "eslint@9.0.0", "node_modules")
}

// Any package of the closure eslint loads, edited in place with no reinstall
// (the install stamp untouched), changes the key — eslint-shaped or not, a
// root or a transitive runtime dependency; a package outside the closure
// does not.
func TestFrontendLintFingerprintCoversTheDependenciesEslintLoads(t *testing.T) {
	stubCacheSys(t)
	dir := depsFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	run := frontendLintRun{scripts: []string{"lint"}, nmDir: dir}
	stamp, err := os.ReadFile(filepath.Join(dir, "node_modules", ".package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	prev := mustKey(t, c, dir, run)

	store := filepath.Join(dir, "node_modules", ".pnpm", "eslint-plugin-foo@1.0.0", "node_modules")
	for _, p := range []string{
		filepath.Join(store, "eslint-plugin-foo", "lib", "rules", "no-thing.js"), // listed, through the symlink
		filepath.Join(store, "eslint-plugin-foo", "dist", "index.js"),            // dist/ is code in a package
		filepath.Join(store, "eslint-plugin-transitive", "index.js"),             // the plugin's own plugin
		filepath.Join(dir, "node_modules", "eslint-config-bar", "core.js"),       // imported by the config
		filepath.Join(dir, "node_modules", "typescript", "lib", "typescript.js"),
		filepath.Join(store, "espree", "index.js"),                           // the plugin's runtime dependency
		filepath.Join(eslintStore(dir), "eslint", "lib", "linter.js"),        // eslint itself
		filepath.Join(eslintStore(dir), "espree-like", "dist", "espree.cjs"), // reached only via eslint
	} {
		writeCacheFile(t, p, "// patched in place\n")
		got := mustKey(t, c, dir, run)
		if got == prev {
			t.Errorf("patching %s in place did not change the key", p)
		}
		prev = got
	}
	for _, p := range []string{
		filepath.Join(dir, "node_modules", "left-pad", "index.js"),                 // outside the closure
		filepath.Join(store, "eslint-plugin-foo", "node_modules", "x", "index.js"), // nested, but nothing depends on it
	} {
		writeCacheFile(t, p, "// patched in place\n")
		if got := mustKey(t, c, dir, run); got != prev {
			t.Errorf("editing %s (outside the closure eslint loads) changed the key", p)
		}
	}
	// An uninstalled peer (eslint's jiti) is keyed as absent: installing it
	// is a change.
	writeCacheFile(t, filepath.Join(dir, "node_modules", "jiti", "package.json"), `{"name":"jiti"}`)
	if got := mustKey(t, c, dir, run); got == prev {
		t.Error("installing a peer that was absent did not change the key")
	} else {
		prev = got
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "node_modules", ".package-lock.json")); string(now) != string(stamp) {
		t.Fatal("the install stamp moved: the test no longer proves an in-place edit")
	}

	// Installing a package that was absent (typescript removed, then back)
	// is a change as well.
	if err := os.RemoveAll(filepath.Join(dir, "node_modules", "typescript")); err != nil {
		t.Fatal(err)
	}
	if got := mustKey(t, c, dir, run); got == prev {
		t.Error("removing typescript did not change the key")
	}
}

// The lane end to end: a pass, an in-place patch of the plugin with the
// install stamp untouched, and the lane runs again instead of replaying.
func TestFrontendLintCacheRerunsAfterAnInPlaceDependencyEdit(t *testing.T) {
	stubCacheSys(t)
	r := stubLintRunner(t)
	dir := depsFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	lintDir(c, dir, false)
	if fs, _ := lintDir(c, dir, false); r.calls != 1 || cachedNote(fs) == nil {
		t.Fatalf("setup: want a cached second run, calls=%d", r.calls)
	}
	writeCacheFile(t, filepath.Join(dir, "node_modules", "eslint-plugin-foo", "lib", "rules", "no-thing.js"),
		"module.exports = { patched: true };\n")
	if fs, _ := lintDir(c, dir, false); r.calls != 2 || cachedNote(fs) != nil {
		t.Fatalf("an in-place plugin patch replayed a cached pass: calls=%d", r.calls)
	}
}

// The lane end to end for a transitive, non-eslint-shaped dependency: an
// espree-like package reached only through eslint's own dependencies,
// patched in place, reruns the lane; an unrelated package does not.
func TestFrontendLintCacheRerunsAfterATransitiveRuntimeDependencyEdit(t *testing.T) {
	stubCacheSys(t)
	r := stubLintRunner(t)
	dir := depsFrontend(t)
	c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
	lintDir(c, dir, false)
	writeCacheFile(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), "// unrelated edit\n")
	if fs, _ := lintDir(c, dir, false); r.calls != 1 || cachedNote(fs) == nil {
		t.Fatalf("an edit outside the closure re-ran the lane: calls=%d", r.calls)
	}
	writeCacheFile(t, filepath.Join(eslintStore(dir), "espree-like", "dist", "espree.cjs"), "module.exports = { patched: true };\n")
	if fs, _ := lintDir(c, dir, false); r.calls != 2 || cachedNote(fs) != nil {
		t.Fatalf("an in-place patch of eslint's runtime dependency replayed a cached pass: calls=%d", r.calls)
	}
}

func TestEslintRelevantPackage(t *testing.T) {
	for name, want := range map[string]bool{
		"eslint": true, "eslint-plugin-import": true, "eslint-config-next": true,
		"eslint-import-resolver-typescript": true, "@eslint/js": true, "@eslint/eslintrc": true,
		"typescript": true, "typescript-eslint": true, "@typescript-eslint/parser": true,
		"@next/eslint-plugin-next": true, "@vendor/eslint-config": true, "@scope/eslint-config-x": true,
		"react": false, "espree": false, "left-pad": false, "@types/node": false, "prettier": false,
	} {
		if got := eslintRelevantPackage(name); got != want {
			t.Errorf("eslintRelevantPackage(%q) = %v, want %v", name, got, want)
		}
	}
	for spec, want := range map[string]string{
		"eslint-config-next/core-web-vitals": "eslint-config-next", "@eslint/js": "@eslint/js",
		"@eslint/js/x": "@eslint/js", "./local.mjs": "", "node:path": "", "/abs": "", "@bad": "",
	} {
		if got := packageNameOf(spec); got != want {
			t.Errorf("packageNameOf(%q) = %q, want %q", spec, got, want)
		}
	}
}
