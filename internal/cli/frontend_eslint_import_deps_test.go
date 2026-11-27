package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// eslintImportXManifest is a frontend scaffolded before the import-x switch.
const eslintImportXManifest = `{
  "name": "web",
  "devDependencies": {
    "eslint-plugin-import": "^2.31.0"
  }
}
`

// eslintImportXConfigHeader is what `forge project upgrade` rewrote the
// frontend's eslint.config.mjs to import.
const eslintImportXConfigHeader = `import { createTypeScriptImportResolver } from "eslint-import-resolver-typescript";
import { importX } from "eslint-plugin-import-x";
export default [];
`

// `forge generate` declares the packages the upgraded eslint config imports in
// the frontend's package.json, so the next install brings them.
func TestReconcileFrontendDependenciesDeclaresESLintImportPackages(t *testing.T) {
	checksums.SetTier1TargetSet(map[string]bool{})
	t.Setenv("FORGE_SKIP_NPM_INSTALL", "1")
	dir := t.TempDir()
	writeAdoptionFile(t, dir, "frontends/web/package.json", eslintImportXManifest)
	writeAdoptionFile(t, dir, "frontends/web/eslint.config.mjs", eslintImportXConfigHeader)
	fe := config.FrontendConfig{Name: "web", Type: "nextjs"}.WithDir("frontends/web")

	reconcileFrontendDependencies(dir, "frontends/web", fe, nil)

	got := readAdoptionFile(t, dir, "frontends/web/package.json")
	for _, p := range templates.ESLintImportPackages {
		if want := `"` + p.Name + `": "` + p.Version + `"`; !strings.Contains(got, want) {
			t.Errorf("package.json does not declare %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"eslint-plugin-import": "^2.31.0"`) {
		t.Errorf("an unrelated devDependency was removed or changed:\n%s", got)
	}
}

// A disowned manifest gets the notice, never the edit.
func TestReconcileFrontendDependenciesLeavesDisownedManifestForESLintImports(t *testing.T) {
	checksums.SetTier1TargetSet(map[string]bool{})
	dir := t.TempDir()
	writeAdoptionFile(t, dir, "frontends/web/package.json", eslintImportXManifest)
	writeAdoptionFile(t, dir, "frontends/web/eslint.config.mjs", eslintImportXConfigHeader)
	fe := config.FrontendConfig{Name: "web", Type: "nextjs"}.WithDir("frontends/web")
	cs := &checksums.FileChecksums{Disowned: map[string]checksums.DisownedEntry{
		"frontends/web/package.json": {Reason: "the app pins its own lint toolchain"},
	}}

	reconcileFrontendDependencies(dir, "frontends/web", fe, cs)

	if got := readAdoptionFile(t, dir, "frontends/web/package.json"); got != eslintImportXManifest {
		t.Fatalf("a disowned package.json was edited:\n%s", got)
	}
}
