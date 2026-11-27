package lint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFrontendLintCacheUnprovenConfigsRunAfterExternalEdit(t *testing.T) {
	for name, config := range map[string]string{
		"computed require": "const p = '../../shared.cjs'; module.exports = require(p);\n",
		"environment import": "export default (await import(process.env.CONFIG_PATH)).default;\n",
		"transitive helper": "export { default } from './helper.mjs';\n",
		"legacy extends": "{\"extends\": \"../../shared.cjs\"}",
	} {
		t.Run(name, func(t *testing.T) {
			stubCacheSys(t)
			r := stubLintRunner(t)
			root := t.TempDir()
			dir := filepath.Join(root, "frontends", "web")
			writeCacheFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"lint":"eslint ."}}`)
			writeCacheFile(t, filepath.Join(dir, "node_modules", ".package-lock.json"), "{}")
			file := "eslint.config.mjs"
			if name == "legacy extends" {
				file = ".eslintrc.json"
			}
			writeCacheFile(t, filepath.Join(dir, file), config)
			writeCacheFile(t, filepath.Join(dir, "helper.mjs"), "export { default } from '../../shared.cjs';\n")
			external := filepath.Join(root, "shared.cjs")
			writeCacheFile(t, external, "module.exports = [];\n")
			c := newFrontendLintCache(root, false, func(string) string { return "" })
			lintDir(c, dir, false)
			writeCacheFile(t, external, "module.exports = [{ rules: { semi: 'error' } }];\n")
			r.err = errors.New("changed external config rejects source")
			fs, gated := lintDir(c, dir, false)
			if !gated || r.calls != 2 || cachedNote(fs) != nil {
				t.Fatalf("external edit replayed a pass: calls=%d gated=%v findings=%+v", r.calls, gated, fs)
			}
		})
	}
}

func TestFrontendLintFingerprintHashesToolContents(t *testing.T) {
	for _, tool := range []string{"node", "npm"} {
		t.Run(tool, func(t *testing.T) {
			stubCacheSys(t)
			p := filepath.Join(t.TempDir(), tool)
			writeCacheFile(t, p, "before")
			st, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			lintCacheLookPath = func(name string) (string, error) {
				if name == tool {
					return p, nil
				}
				return "", errors.New("absent")
			}
			dir := cacheFrontend(t)
			c := newFrontendLintCache(t.TempDir(), false, func(string) string { return "" })
			run := frontendLintRun{scripts: []string{"lint"}, nmDir: dir}
			before := mustKey(t, c, dir, run)
			writeCacheFile(t, p, "after!")
			if err := os.Chtimes(p, st.ModTime(), st.ModTime()); err != nil {
				t.Fatal(err)
			}
			if after := mustKey(t, c, dir, run); after == before {
				t.Fatal("same-size replacement with restored mtime did not change the key")
			}
		})
	}
}
