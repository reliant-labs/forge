package templates

import (
	"regexp"
	"strings"
	"testing"
)

// TestNextJSESLintImportX_ConfigIsWired pins the part of the import-x switch
// that fails SILENTLY when it is wrong. eslint-config-next supplies parser and
// resolver settings only under the old `import/*` keys, and import-x's default
// extensions are .js/.mjs/.cjs, so a config that renames the rules 1:1 lints
// green while import-x/no-cycle parses no .ts/.tsx file and reports nothing.
// The slow end-to-end proof is TestNextJSScaffoldLintReportsImportCycle (in
// internal/cli); this catches the config regression in milliseconds.
func TestNextJSESLintImportX_ConfigIsWired(t *testing.T) {
	t.Parallel()
	raw, err := FrontendTemplates().Get("nextjs/eslint.config.mjs")
	if err != nil {
		t.Fatalf("read nextjs/eslint.config.mjs: %v", err)
	}
	cfg := string(raw)

	m := regexp.MustCompile(`"import-x/extensions":\s*\[([^\]]*)\]`).FindStringSubmatch(cfg)
	if m == nil {
		t.Fatalf("nextjs eslint config sets no import-x/extensions; import-x then reads only .js/.mjs/.cjs and no-cycle silently reports nothing")
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx"} {
		if !strings.Contains(m[1], `"`+ext+`"`) {
			t.Errorf("import-x/extensions %s omits %q", m[1], ext)
		}
	}
	if !strings.Contains(cfg, `"import-x/resolver-next": [createTypeScriptImportResolver()]`) {
		t.Errorf("nextjs eslint config does not give import-x the TypeScript resolver; @/ aliases and .ts imports then do not resolve")
	}

	// Every rule and setting this config owns is an import-x one. An
	// `import/*` key would configure eslint-config-next's eslint-plugin-import
	// instead (the expensive no-cycle forge moved off), or nothing at all.
	if old := regexp.MustCompile(`"import/[a-z-]+"\s*:`).FindAllString(cfg, -1); len(old) > 0 {
		t.Errorf("nextjs eslint config still configures eslint-plugin-import keys %v; use import-x/*", old)
	}
	if !strings.Contains(cfg, `"import-x": importX`) {
		t.Errorf("nextjs eslint config does not register the plugin as \"import-x\"")
	}
	if strings.Contains(cfg, `from "eslint-plugin-import"`) {
		t.Errorf("nextjs eslint config still imports eslint-plugin-import; registering it as `import` beside eslint-config-next's copy fails with `Cannot redefine plugin \"import\"` once the two resolve apart")
	}
	for _, rule := range []string{"import-x/no-cycle", "import-x/order", "import-x/no-default-export"} {
		if !strings.Contains(cfg, `"`+rule+`"`) {
			t.Errorf("nextjs eslint config no longer configures %s", rule)
		}
	}
}

// TestNextJSPackageJSON_DeclaresESLintImportPins holds the scaffold's
// package.json to ESLintImportPackages, the pins `forge generate` also
// declares in an upgraded project, and to what the config imports.
func TestNextJSPackageJSON_DeclaresESLintImportPins(t *testing.T) {
	t.Parallel()
	pkg := renderedPackageJSON(t, "nextjs")
	dev, _ := pkg["devDependencies"].(map[string]any)
	raw, err := FrontendTemplates().Get("nextjs/eslint.config.mjs")
	if err != nil {
		t.Fatalf("read nextjs/eslint.config.mjs: %v", err)
	}
	for _, p := range ESLintImportPackages {
		if got, _ := dev[p.Name].(string); got != p.Version {
			t.Errorf("nextjs package.json devDependencies[%q] = %q, want the exact pin %q", p.Name, got, p.Version)
		}
		if !strings.Contains(string(raw), `from "`+p.Name+`"`) {
			t.Errorf("ESLintImportPackages names %s, which nextjs/eslint.config.mjs does not import", p.Name)
		}
	}
	if _, ok := dev["eslint-plugin-import"]; ok {
		t.Errorf("nextjs package.json still declares eslint-plugin-import, which the config no longer imports")
	}
}
