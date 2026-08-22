package kclvendor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/templates"
)

// legacyGitTagKclMod is the shape older scaffolds emitted: a git tag
// that was never published. EnsureVendorDep must heal it.
const legacyGitTagKclMod = `[package]
name = "proj-deploy"
edition = "v0.11.0"
version = "0.0.1"

# The ` + "`forge`" + ` KCL module ships the typed schemas this package's env
# main.k files import.
[dependencies]
forge = { git = "https://github.com/reliant-labs/forge.git", tag = "kcl-v0.1.0" }
`

// writeKclMod writes content at <projectDir>/<rel> and returns the path.
func writeKclMod(t *testing.T, projectDir, rel, content string) string {
	t.Helper()
	path := filepath.Join(projectDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestEnsureVendorDep_SwapsGitTagWithDepthCorrectPath(t *testing.T) {
	cases := []struct {
		name    string
		rel     string
		wantDep string
	}{
		{"deploy-kcl depth", "deploy/kcl/kcl.mod", `forge = { path = "../../.forge-kcl" }`},
		{"project-root depth", "kcl.mod", `forge = { path = "./.forge-kcl" }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeKclMod(t, dir, tc.rel, legacyGitTagKclMod)
			res, err := EnsureVendorDep(path, dir)
			if err != nil {
				t.Fatalf("EnsureVendorDep: %v", err)
			}
			if !res.Changed || res.Warning != "" {
				t.Fatalf("want Changed with no warning, got %+v", res)
			}
			got := readFile(t, path)
			if !strings.Contains(got, tc.wantDep) {
				t.Errorf("missing %q in:\n%s", tc.wantDep, got)
			}
			if !strings.Contains(got, MarkerHeader) {
				t.Errorf("missing marker header in:\n%s", got)
			}
			if strings.Contains(got, "git = ") {
				t.Errorf("published git dep should be gone:\n%s", got)
			}
			// The [package] block and the scaffold's own comment survive.
			for _, keep := range []string{`name = "proj-deploy"`, "typed schemas"} {
				if !strings.Contains(got, keep) {
					t.Errorf("user content %q clobbered:\n%s", keep, got)
				}
			}
		})
	}
}

func TestEnsureVendorDep_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeKclMod(t, dir, "deploy/kcl/kcl.mod", legacyGitTagKclMod)
	if _, err := EnsureVendorDep(path, dir); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := readFile(t, path)
	res, err := EnsureVendorDep(path, dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if res.Changed || res.Warning != "" {
		t.Fatalf("second call must be a no-op, got %+v", res)
	}
	if got := readFile(t, path); got != first {
		t.Errorf("second call changed bytes:\n--- first ---\n%s\n--- second ---\n%s", first, got)
	}
}

func TestEnsureVendorDep_RewritesAbsolutePathHandPatch(t *testing.T) {
	// The historical workaround this package replaces: an absolute host
	// path, with a hand-written comment above it that must survive.
	dir := t.TempDir()
	content := `[package]
name = "proj-deploy"
edition = "v0.11.0"
version = "0.0.1"

[dependencies]
# Local-dev override: resolve the module from the local clone.
forge = { path = "/Users/someone/src/forge/kcl" }
`
	path := writeKclMod(t, dir, "deploy/kcl/kcl.mod", content)
	res, err := EnsureVendorDep(path, dir)
	if err != nil {
		t.Fatalf("EnsureVendorDep: %v", err)
	}
	if !res.Changed {
		t.Fatalf("want Changed, got %+v", res)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `forge = { path = "../../.forge-kcl" }`) {
		t.Errorf("abs path not rewritten:\n%s", got)
	}
	if strings.Contains(got, "/Users/someone") {
		t.Errorf("absolute host path lingers:\n%s", got)
	}
	if !strings.Contains(got, "# Local-dev override: resolve the module from the local clone.") {
		t.Errorf("user comment above the dep line was deleted:\n%s", got)
	}
}

func TestEnsureVendorDep_HandMangledWarnsAndNoops(t *testing.T) {
	cases := []struct {
		name string
		dep  string
	}{
		{"toml table form", "[dependencies.forge]\npath = \"../../somewhere\""},
		{"extra keys", `forge = { path = "../../.forge-kcl", version = "0.1.0" }`},
		{"relative non-vendor path", `forge = { path = "../forge/kcl" }`},
		{"registry version", `forge = "0.1.0"`},
		{"two forge lines", "forge = { path = \"./a\" }\nforge = { path = \"./b\" }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := "[package]\nname = \"p\"\n\n[dependencies]\n" + tc.dep + "\n"
			path := writeKclMod(t, dir, "deploy/kcl/kcl.mod", content)
			res, err := EnsureVendorDep(path, dir)
			if err != nil {
				t.Fatalf("EnsureVendorDep: %v", err)
			}
			if res.Changed {
				t.Fatalf("hand-mangled shape must not be edited, got %+v", res)
			}
			if res.Warning == "" {
				t.Fatalf("want a warning for shape %q", tc.dep)
			}
			if got := readFile(t, path); got != content {
				t.Errorf("content changed:\n%s", got)
			}
		})
	}
}

func TestEnsureVendorDep_NoForgeDepWarns(t *testing.T) {
	dir := t.TempDir()
	content := "[package]\nname = \"p\"\n\n[dependencies]\nk8s = \"1.31\"\n"
	path := writeKclMod(t, dir, "deploy/kcl/kcl.mod", content)
	res, err := EnsureVendorDep(path, dir)
	if err != nil {
		t.Fatalf("EnsureVendorDep: %v", err)
	}
	if res.Changed || res.Warning == "" {
		t.Fatalf("want warn+no-op, got %+v", res)
	}
	if got := readFile(t, path); got != content {
		t.Errorf("content changed:\n%s", got)
	}
}

func TestEnsureVendorDep_MissingFileIsSilentNoop(t *testing.T) {
	dir := t.TempDir()
	res, err := EnsureVendorDep(filepath.Join(dir, "deploy", "kcl", "kcl.mod"), dir)
	if err != nil {
		t.Fatalf("EnsureVendorDep: %v", err)
	}
	if res.Changed || res.Warning != "" {
		t.Fatalf("missing file must be a silent no-op, got %+v", res)
	}
}

// TestEnsureVendorDep_RewritesLegacyMarkerBlockInPlace: a project
// vendored by an older forge carries that forge's marker header. The
// upgrade must REPLACE the whole stale block, not stack a fresh one on
// top of it — otherwise every upgrade grows the file by a comment block.
func TestEnsureVendorDep_RewritesLegacyMarkerBlockInPlace(t *testing.T) {
	dir := t.TempDir()
	content := `[package]
name = "proj-deploy"

[dependencies]
` + legacyMarkerHeaders[0] + `
#
# Some older explanatory text that no longer describes what forge does.
forge = { path = "../../.forge-kcl" }
`
	path := writeKclMod(t, dir, "deploy/kcl/kcl.mod", content)
	res, err := EnsureVendorDep(path, dir)
	if err != nil {
		t.Fatalf("EnsureVendorDep: %v", err)
	}
	if !res.Changed || res.Warning != "" {
		t.Fatalf("want the legacy block rewritten, got %+v", res)
	}
	got := readFile(t, path)
	if strings.Contains(got, legacyMarkerHeaders[0]) {
		t.Errorf("legacy marker header survived the rewrite:\n%s", got)
	}
	if strings.Contains(got, "no longer describes") {
		t.Errorf("stale legacy comment body survived:\n%s", got)
	}
	if n := strings.Count(got, MarkerHeader); n != 1 {
		t.Errorf("want exactly one marker header, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, `forge = { path = "../../.forge-kcl" }`) {
		t.Errorf("dep line missing:\n%s", got)
	}
}

// TestMaterialize_StampsForgeVersionAndReportsStaleness: the vendored
// copy records the forge that wrote it, and Stale reports a mismatch.
// This is the mitigation for vendoring's one real cost — a project can
// otherwise sit silently on a module an older forge materialized.
func TestMaterialize_StampsForgeVersionAndReportsStaleness(t *testing.T) {
	dir := t.TempDir()
	if _, err := Materialize(dir); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	stampPath := filepath.Join(dir, VendorDirName, StampFileName)
	stamped := strings.TrimSpace(readFile(t, stampPath))
	if stamped != buildinfo.Version() {
		t.Errorf("stamp = %q, want this binary's version %q", stamped, buildinfo.Version())
	}
	if stale, _ := Stale(dir); stale {
		t.Errorf("a freshly materialized copy must not be stale")
	}

	// A copy written by a different forge is stale, and names the version.
	if err := os.WriteFile(stampPath, []byte("v0.0.1-ancient\n"), 0o644); err != nil {
		t.Fatalf("rewrite stamp: %v", err)
	}
	stale, got := Stale(dir)
	if !stale || got != "v0.0.1-ancient" {
		t.Errorf("Stale() = (%v, %q), want (true, \"v0.0.1-ancient\")", stale, got)
	}

	// An unstamped copy (materialized before the stamp existed) is stale.
	if err := os.Remove(stampPath); err != nil {
		t.Fatalf("remove stamp: %v", err)
	}
	if stale, got := Stale(dir); !stale || got != "" {
		t.Errorf("unstamped copy: Stale() = (%v, %q), want (true, \"\")", stale, got)
	}

	// Re-materializing heals it, and stays byte-idempotent afterwards.
	if _, err := Materialize(dir); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if stale, _ := Stale(dir); stale {
		t.Errorf("still stale after re-materialize")
	}
	changed, err := Materialize(dir)
	if err != nil {
		t.Fatalf("third materialize: %v", err)
	}
	if changed {
		t.Errorf("materialize must be a no-op once the stamp is current")
	}

	// No project is stale for lacking a vendor dir entirely.
	if stale, _ := Stale(t.TempDir()); stale {
		t.Errorf("a project with no vendor dir must not report stale")
	}
}

func TestMaterialize_IdempotentRefreshesDriftAndDeletesStrays(t *testing.T) {
	dir := t.TempDir()

	changed, err := Materialize(dir)
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if !changed {
		t.Fatalf("first materialize must report change")
	}
	if !Present(dir) {
		t.Fatalf("vendor dir not present after materialize")
	}
	// The embedded module's core files landed; excluded trees did not.
	for _, want := range []string{"kcl.mod", "schema.k", "render.k", filepath.Join("workloads", "schema.k"), filepath.Join("lib", "services.k")} {
		if _, err := os.Stat(filepath.Join(dir, VendorDirName, want)); err != nil {
			t.Errorf("expected vendored file %s: %v", want, err)
		}
	}
	for _, absent := range []string{"tests", "example", "embed.go", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, VendorDirName, absent)); err == nil {
			t.Errorf("%s must not be vendored", absent)
		}
	}

	// Second run: byte-idempotent.
	changed, err = Materialize(dir)
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if changed {
		t.Errorf("second materialize must be a no-op")
	}

	// Drift heal + stray deletion; a kpm-derived lock is tolerated.
	schemaPath := filepath.Join(dir, VendorDirName, "schema.k")
	if err := os.WriteFile(schemaPath, []byte("# drift"), 0o644); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	strayPath := filepath.Join(dir, VendorDirName, "stale.k")
	if err := os.WriteFile(strayPath, []byte("# stray"), 0o644); err != nil {
		t.Fatalf("inject stray: %v", err)
	}
	lockPath := filepath.Join(dir, VendorDirName, "kcl.mod.lock")
	if err := os.WriteFile(lockPath, []byte("[dependencies]\n"), 0o644); err != nil {
		t.Fatalf("inject lock: %v", err)
	}
	changed, err = Materialize(dir)
	if err != nil {
		t.Fatalf("heal materialize: %v", err)
	}
	if !changed {
		t.Errorf("heal materialize must report change")
	}
	if got := readFile(t, schemaPath); got == "# drift" {
		t.Errorf("drifted file not healed")
	}
	if _, err := os.Stat(strayPath); err == nil {
		t.Errorf("stray file not deleted")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("kpm lock must be tolerated, got %v", err)
	}
}

// TestScaffoldTemplateEmitsTheVendoredDep: the template must emit the
// vendored relative path DIRECTLY, so a scaffold is resolvable the
// instant it is written rather than depending on a later patch.
//
// The template previously emitted a published git tag here and the
// vendor patch only ran on dev builds, so a released forge produced a
// kcl.mod pointing at a tag that did not exist.
func TestScaffoldTemplateEmitsTheVendoredDep(t *testing.T) {
	rendered, err := templates.DeployTemplates().Render("kcl/kcl.mod.tmpl", struct{ ProjectName string }{"proj"})
	if err != nil {
		t.Fatalf("render kcl.mod.tmpl: %v", err)
	}
	dir := t.TempDir()
	path := writeKclMod(t, dir, "deploy/kcl/kcl.mod", string(rendered))

	wantDep, err := VendorDepPath(path, dir)
	if err != nil {
		t.Fatalf("VendorDepPath: %v", err)
	}
	if !strings.Contains(string(rendered), vendorDepLine(wantDep)) {
		t.Fatalf("kcl.mod.tmpl must emit %q directly:\n%s", vendorDepLine(wantDep), rendered)
	}
	if strings.Contains(string(rendered), "git = ") {
		t.Fatalf("kcl.mod.tmpl still emits a git dependency — the scaffold must be born vendored:\n%s", rendered)
	}

	// The patcher must AGREE with the template: what the scaffold emits
	// is already canonical, so re-patching it is a no-op. A mismatch here
	// means every `forge generate` would rewrite a fresh scaffold.
	if kind, _, _ := classifyDep(strings.Split(string(rendered), "\n")); kind != DepVendored {
		t.Fatalf("patcher classifies the scaffold template output as %v, want DepVendored", kind)
	}
	res, err := EnsureVendorDep(path, dir)
	if err != nil {
		t.Fatalf("EnsureVendorDep on rendered template: %v", err)
	}
	if res.Warning != "" {
		t.Fatalf("patcher does not recognize the scaffold template output: %+v", res)
	}
}
