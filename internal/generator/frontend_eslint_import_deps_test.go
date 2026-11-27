package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

const (
	pluginImportX    = "eslint-plugin-import-x"
	resolverTSImport = "eslint-import-resolver-typescript"
)

// eslintImportXConfig is the import header of the Next.js scaffold's
// eslint.config.mjs since it moved to import-x.
const eslintImportXConfig = `import js from "@eslint/js";
import { createTypeScriptImportResolver } from "eslint-import-resolver-typescript";
import { importX } from "eslint-plugin-import-x";

export default [js.configs.recommended];
`

// preImportXManifest is a Next.js frontend's package.json from before the
// switch: it declares eslint-plugin-import and neither new package.
const preImportXManifest = `{
  "name": "web",
  "private": true,
  "dependencies": {
    "next": "^16.3.5"
  },
  "devDependencies": {
    "eslint": "^9.25.0",
    "eslint-plugin-import": "^2.31.0",
    "typescript": "^5.8.0"
  }
}
`

func eslintPin(t *testing.T, name string) string {
	t.Helper()
	for _, p := range templates.ESLintImportPackages {
		if p.Name == name {
			return p.Version
		}
	}
	t.Fatalf("templates.ESLintImportPackages has no %s", name)
	return ""
}

// eslintImportFixture writes frontends/web with the given manifest and, when
// config is not empty, an eslint.config.mjs holding it.
func eslintImportFixture(t *testing.T, manifest, config string) (projectDir, feRel, pkgPath string) {
	t.Helper()
	projectDir = t.TempDir()
	pkgPath = writeManifest(t, projectDir, manifest)
	if config != "" {
		writeFrontendSource(t, projectDir, "web", "eslint.config.mjs", config)
	}
	return projectDir, filepath.Join("frontends", "web"), pkgPath
}

func readManifestText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An upgraded project: `forge project upgrade` rewrote eslint.config.mjs to
// import the two packages, and package.json still declares neither. Both are
// added to devDependencies at forge's exact pins, every other entry and the
// file's layout are kept, and a second run changes nothing.
func TestEnsureESLintImportDependencies_AddsImportedMissingPackages(t *testing.T) {
	projectDir, feRel, pkgPath := eslintImportFixture(t, preImportXManifest, eslintImportXConfig)

	if !EnsureESLintImportDependencies(projectDir, feRel, "web") {
		t.Fatal("EnsureESLintImportDependencies reported no change for a manifest missing both packages")
	}
	got := readManifestText(t, pkgPath)
	doc := readScopes(t, pkgPath)
	for _, name := range []string{pluginImportX, resolverTSImport} {
		if v := doc.DevDependencies[name]; v != eslintPin(t, name) {
			t.Errorf("devDependencies[%s] = %q, want %q", name, v, eslintPin(t, name))
		}
		if _, ok := doc.Dependencies[name]; ok {
			t.Errorf("%s was declared in dependencies; eslint plugins are devDependencies", name)
		}
	}
	// Nothing else moved: the old plugin, the other entries, the layout.
	want := strings.Replace(preImportXManifest, `"devDependencies": {`,
		`"devDependencies": {`+"\n"+
			`    "`+resolverTSImport+`": "`+eslintPin(t, resolverTSImport)+`",`+"\n"+
			`    "`+pluginImportX+`": "`+eslintPin(t, pluginImportX)+`",`, 1)
	if got != want {
		t.Errorf("manifest was not edited surgically; got:\n%s\nwant:\n%s", got, want)
	}

	if EnsureESLintImportDependencies(projectDir, feRel, "web") {
		t.Error("a second run rewrote an already-converged manifest")
	}
	if again := readManifestText(t, pkgPath); again != got {
		t.Errorf("a second run changed the manifest:\n%s", again)
	}
}

// A package the project already declares, at any version and in either
// scope, is the project's: forge neither re-pins nor moves it.
func TestEnsureESLintImportDependencies_LeavesDeclaredPackagesAlone(t *testing.T) {
	for name, manifest := range map[string]string{
		"devDependencies, other versions": `{
  "devDependencies": {
    "` + pluginImportX + `": "^4.0.0",
    "` + resolverTSImport + `": "3.10.1"
  }
}
`,
		"dependencies": `{
  "dependencies": {
    "` + pluginImportX + `": "latest",
    "` + resolverTSImport + `": "*"
  }
}
`,
	} {
		t.Run(name, func(t *testing.T) {
			projectDir, feRel, pkgPath := eslintImportFixture(t, manifest, eslintImportXConfig)
			if EnsureESLintImportDependencies(projectDir, feRel, "web") {
				t.Error("reported a change for a manifest that declares both packages")
			}
			if got := readManifestText(t, pkgPath); got != manifest {
				t.Errorf("manifest changed:\n%s", got)
			}
			if n := eslintImportDependencyNotices(filepath.Join(projectDir, feRel), manifest); len(n) != 0 {
				t.Errorf("notices for declared packages: %v", n)
			}
		})
	}
}

// A frontend whose eslint config does not import the packages (a Vite SPA, an
// adopted app with its own config, a frontend with no eslint config at all)
// is left alone.
func TestEnsureESLintImportDependencies_NotImportedIsNoOp(t *testing.T) {
	for name, config := range map[string]string{
		"no eslint config": "",
		"old plugin only": `import importPlugin from "eslint-plugin-import";
export default [{ plugins: { import: importPlugin }, rules: { "import/no-cycle": "error" } }];
`,
		"mentions import-x only in rule keys": `export default [{ rules: { "import-x/no-cycle": "off" } }];
`,
	} {
		t.Run(name, func(t *testing.T) {
			projectDir, feRel, pkgPath := eslintImportFixture(t, preImportXManifest, config)
			if EnsureESLintImportDependencies(projectDir, feRel, "web") {
				t.Error("reported a change for a frontend whose eslint config imports neither package")
			}
			if got := readManifestText(t, pkgPath); got != preImportXManifest {
				t.Errorf("manifest changed:\n%s", got)
			}
		})
	}
}

// Only what the config imports is declared: a config that brings its own
// resolver setup but imports the plugin gets the plugin alone. Every config
// file name ESLint 9 reads counts, and so do require() and subpath imports.
func TestEnsureESLintImportDependencies_OnlyTheImportedOnes(t *testing.T) {
	for name, tc := range map[string]struct {
		file, config string
		want         []string
	}{
		"plugin only (mjs)": {
			file:   "eslint.config.mjs",
			config: `import { importX } from "eslint-plugin-import-x";` + "\n",
			want:   []string{pluginImportX},
		},
		"resolver only, require (cjs)": {
			file:   "eslint.config.cjs",
			config: `const { createTypeScriptImportResolver } = require('eslint-import-resolver-typescript');` + "\n",
			want:   []string{resolverTSImport},
		},
		"plugin subpath (ts)": {
			file:   "eslint.config.ts",
			config: `import importX from "eslint-plugin-import-x/flat";` + "\n",
			want:   []string{pluginImportX},
		},
	} {
		t.Run(name, func(t *testing.T) {
			projectDir, feRel, pkgPath := eslintImportFixture(t, preImportXManifest, "")
			writeFrontendSource(t, projectDir, "web", tc.file, tc.config)
			if !EnsureESLintImportDependencies(projectDir, feRel, "web") {
				t.Fatal("reported no change")
			}
			doc := readScopes(t, pkgPath)
			for _, name := range []string{pluginImportX, resolverTSImport} {
				_, declared := doc.DevDependencies[name]
				wanted := false
				for _, w := range tc.want {
					wanted = wanted || w == name
				}
				if declared != wanted {
					t.Errorf("%s declared = %v, want %v", name, declared, wanted)
				}
			}
			if doc.DevDependencies["eslint-plugin-import"] != "^2.31.0" {
				t.Error("an unrelated devDependency was touched")
			}
		})
	}
}

// A manifest with no devDependencies object gets one.
func TestEnsureESLintImportDependencies_CreatesDevDependencies(t *testing.T) {
	manifest := `{
  "name": "web",
  "dependencies": {
    "next": "^16.3.5"
  }
}
`
	projectDir, feRel, pkgPath := eslintImportFixture(t, manifest, eslintImportXConfig)
	if !EnsureESLintImportDependencies(projectDir, feRel, "web") {
		t.Fatal("reported no change")
	}
	doc := readScopes(t, pkgPath)
	if doc.DevDependencies[pluginImportX] == "" || doc.DevDependencies[resolverTSImport] == "" {
		t.Fatalf("devDependencies not created:\n%s", readManifestText(t, pkgPath))
	}
	if doc.Dependencies["next"] != "^16.3.5" {
		t.Error("dependencies changed")
	}
}

// A disowned manifest is reported, not edited: FrontendDependencyNotices (what
// `forge generate` prints for one) names each package it would add, with the
// command that adds it.
func TestFrontendDependencyNotices_NameMissingESLintImportPackages(t *testing.T) {
	projectDir, feRel, pkgPath := eslintImportFixture(t, preImportXManifest, eslintImportXConfig)
	notices := strings.Join(FrontendDependencyNotices(projectDir, feRel), "\n")
	for _, name := range []string{pluginImportX, resolverTSImport} {
		if want := "npm install --save-dev " + name + "@" + eslintPin(t, name); !strings.Contains(notices, want) {
			t.Errorf("no notice asks for %q:\n%s", want, notices)
		}
	}
	if got := readManifestText(t, pkgPath); got != preImportXManifest {
		t.Errorf("computing notices edited the manifest:\n%s", got)
	}
}
