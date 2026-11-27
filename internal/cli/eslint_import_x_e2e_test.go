//go:build e2e

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// assertImportCycleIsReported writes a two-file import cycle into a
// scaffolded Next.js frontend with an installed node_modules and requires the
// frontend's eslint to report it as an import-x/no-cycle error, then removes
// the files. The cycle crosses .ts and .tsx and a relative and an `@/` alias
// import, the three things import-x only follows when the config gives it the
// TypeScript resolver and the .ts/.tsx extensions: without them the rule
// lints green and reports nothing.
func assertImportCycleIsReported(t *testing.T, feDir string) {
	t.Helper()
	a := filepath.Join(feDir, "src", "lib", "forge-cycle-a.ts")
	b := filepath.Join(feDir, "src", "components", "forge-cycle-b.tsx")
	writeFileE2E(t, a, "import { b } from \"../components/forge-cycle-b\";\n\nexport const a = (): number => b() + 1;\n")
	writeFileE2E(t, b, "import { a } from \"@/lib/forge-cycle-a\";\n\nexport function b(): number {\n  return typeof a === \"function\" ? 1 : 0;\n}\n")
	defer func() {
		_ = os.Remove(a)
		_ = os.Remove(b)
	}()
	cmd := exec.Command("npx", "--no-install", "eslint", a, b)
	cmd.Dir = feDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("eslint exited 0 on a two-file import cycle — import-x/no-cycle is not reaching .ts/.tsx files:\n%s", out)
	}
	if got := strings.Count(string(out), "import-x/no-cycle"); got < 2 {
		t.Fatalf("eslint reported %d import-x/no-cycle findings for a cycle of two files, want one per file:\n%s", got, out)
	}
}

// preImportXESLintConfig is the Next.js eslint.config.mjs as forge rendered it
// before the import-x switch, reconstructed from today's template: the
// eslint-plugin-import import and plugin key, the `import/*` rule and setting
// keys, and no import-x resolver or extensions.
func preImportXESLintConfig(t *testing.T) string {
	t.Helper()
	raw, err := templates.FrontendTemplates().Get("nextjs/eslint.config.mjs")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, r := range [][2]string{
		{`import { createTypeScriptImportResolver } from "eslint-import-resolver-typescript";` + "\n", ""},
		{`import { importX } from "eslint-plugin-import-x";`, `import importPlugin from "eslint-plugin-import";`},
		{`"import-x": importX,`, `import: importPlugin,`},
		{`"import-x/`, `"import/`},
	} {
		if !strings.Contains(s, r[0]) {
			t.Fatalf("nextjs/eslint.config.mjs no longer contains %q; update this reconstruction of the pre-import-x config", r[0])
		}
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, `"import/resolver-next"`) || strings.Contains(line, `"import/extensions"`) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestE2EUpgradedFrontendLintsWithImportX is the upgrade path of the import-x
// switch. A project scaffolded before it has an eslint.config.mjs forge
// rendered (forge:hash marker intact) and a package.json declaring
// eslint-plugin-import. `forge project upgrade` rewrites the config to the
// import-x one; `forge generate` then declares the two packages it imports, so
// the install brings them and the frontend lints green instead of failing with
// ERR_MODULE_NOT_FOUND — and import-x/no-cycle really reports cycles.
func TestE2EUpgradedFrontendLintsWithImportX(t *testing.T) {
	t.Parallel()
	requireTool(t, "node", "npm", "pnpm", "git")
	forgeBin := buildforgeBinary(t)
	projectDir := newProjectFromSharedE2E(t, forgeBin, dashboardCatalogProjectE2E)
	feDir := filepath.Join(projectDir, "frontends", "dashboard")
	cfgRel := filepath.Join("frontends", "dashboard", "eslint.config.mjs")

	// The pre-switch vintage, as forge last wrote it.
	old, ok := checksums.Stamp(cfgRel, []byte(preImportXESLintConfig(t)))
	if !ok {
		t.Fatalf("eslint.config.mjs is not stampable")
	}
	writeFileE2E(t, filepath.Join(projectDir, cfgRel), string(old))
	pkgPath := filepath.Join(feDir, "package.json")
	pkg := readFileE2E(t, pkgPath)
	for _, p := range templates.ESLintImportPackages {
		entry := `    "` + p.Name + `": "` + p.Version + `",` + "\n"
		if !strings.Contains(pkg, entry) {
			t.Fatalf("scaffolded package.json does not declare %s:\n%s", strings.TrimSpace(entry), pkg)
		}
		pkg = strings.Replace(pkg, entry, "", 1)
	}
	pkg = strings.Replace(pkg, `"devDependencies": {`, `"devDependencies": {`+"\n"+`    "eslint-plugin-import": "^2.31.0",`, 1)
	writeFileE2E(t, pkgPath, pkg)
	runCmd(t, projectDir, "git", "add", "-A")
	runCmd(t, projectDir, "git", "-c", "user.email=e2e@forge.test", "-c", "user.name=e2e", "commit", "-q", "--no-verify", "--allow-empty", "-m", "fixture: pre-import-x frontend")

	// The target is the forge go.mod already requires: the test binary is a
	// +dirty dev build, which is not a module version.
	mods, err := projectForgeModules(projectDir)
	if err != nil || len(mods) == 0 {
		t.Fatalf("project requires no forge module: %v %v", mods, err)
	}
	up := exec.Command(forgeBin, "project", "upgrade", "--to", mods[0].version, "--allow-dirty", "--skip", "playbooks", "--skip", "lint")
	up.Dir = projectDir
	up.Env = append(os.Environ(), "FORGE_ALLOW_VERSION_SKEW=1")
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("forge project upgrade: %v\n%s", err, out)
	}
	want, err := templates.FrontendTemplates().Get("nextjs/eslint.config.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFileE2E(t, filepath.Join(projectDir, cfgRel)); string(checksums.StripMarker([]byte(got))) != string(want) {
		t.Fatalf("forge project upgrade did not rewrite the unedited pre-import-x eslint.config.mjs to the template:\n%s", got)
	}

	genOut := runCmdKeep(t, projectDir, forgeBin, "generate")
	var doc struct {
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal([]byte(readFileE2E(t, pkgPath)), &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range templates.ESLintImportPackages {
		if doc.DevDependencies[p.Name] != p.Version {
			t.Errorf("after upgrade + generate, devDependencies[%s] = %q, want %q\ngenerate output:\n%s",
				p.Name, doc.DevDependencies[p.Name], p.Version, genOut)
		}
	}
	if doc.DevDependencies["eslint-plugin-import"] != "^2.31.0" {
		t.Errorf("forge generate touched the project's own eslint-plugin-import entry: %q", doc.DevDependencies["eslint-plugin-import"])
	}

	prebuildWebRuntimeE2E(t)
	runCmdTimeout(t, feDir, 5*time.Minute, "pnpm", "install", "--no-frozen-lockfile", "--prefer-offline")
	assertScaffoldIsEslintCleanAsEmitted(t, feDir)
	assertImportCycleIsReported(t, feDir)
}
