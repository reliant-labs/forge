package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
)

// writeManagedRender writes content at dir/rel, optionally stamped with
// the embedded forge:hash marker — the on-disk state "forge previously
// wrote this file" (every managed-file format is stampable).
func writeManagedRender(t *testing.T, dir, rel string, content []byte, stamp bool) {
	t.Helper()
	if stamp {
		stamped, ok := checksums.Stamp(rel, content)
		if !ok {
			t.Fatalf("Stamp(%q): unstampable", rel)
		}
		content = stamped
	}
	dest := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testProjectConfig() *config.ProjectConfig {
	return &config.ProjectConfig{
		Name:       "test-project",
		ModulePath: "github.com/example/test-project",
		Components: []config.ComponentConfig{
			{Name: "api", Kind: config.ComponentKindServer, Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}},
		},
	}
}

func TestBuildTemplateData(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	if data.Name != "test-project" {
		t.Errorf("Name = %q, want %q", data.Name, "test-project")
	}
	if data.ProtoName != "test_project" {
		t.Errorf("ProtoName = %q, want %q", data.ProtoName, "test_project")
	}
	if data.Module != "github.com/example/test-project" {
		t.Errorf("Module = %q, want %q", data.Module, "github.com/example/test-project")
	}
	if data.ServiceName != "api" {
		t.Errorf("ServiceName = %q, want %q", data.ServiceName, "api")
	}
	if data.ServicePort != 8080 {
		t.Errorf("ServicePort = %d, want %d", data.ServicePort, 8080)
	}
	if data.GoVersionMinor == "" {
		t.Error("GoVersionMinor should not be empty")
	}
}

func TestBuildTemplateDataDefaults(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name:       "myapp",
		ModulePath: "github.com/example/myapp",
	}
	data := buildTemplateData(cfg, "")

	if data.ServiceName != "api" {
		t.Errorf("ServiceName = %q, want default %q", data.ServiceName, "api")
	}
	if data.ServicePort != 8080 {
		t.Errorf("ServicePort = %d, want default %d", data.ServicePort, 8080)
	}
}

func TestManagedFiles(t *testing.T) {
	files := managedFiles()
	if len(files) == 0 {
		t.Fatal("managedFiles() returned empty list")
	}

	// Check that expected files are in the list
	expected := map[string]bool{
		"cmd/cmd/serve.go":   true,
		"cmd/cmd/server.go":  true,
		"cmd/cmd/root.go":    true,
		"cmd/main.go":        true,
		"cmd/cmd/version.go": true,
		"Taskfile.yml":       true,
		"Dockerfile":              true,
		"docker-compose.yml":      true,
		".golangci.yml":           true,
		// The thin auth-policy pair is the ONLY middleware the project
		// keeps; the mechanism files (cors/auth/claims/…) moved to
		// forge/pkg/{authn,authz,middleware} and must NOT be managed.
		"pkg/middleware/middleware.go":      true,
		"pkg/middleware/middleware_test.go": true,
	}

	found := make(map[string]bool)
	for _, f := range files {
		found[f.destPath] = true
	}

	for path := range expected {
		if !found[path] {
			t.Errorf("managedFiles() missing %q", path)
		}
	}
}

func TestRenderManagedFile(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	for _, f := range managedFiles() {
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Errorf("renderManagedFile(%q): %v", f.templateName, err)
			continue
		}
		if len(content) == 0 {
			t.Errorf("renderManagedFile(%q) returned empty content", f.templateName)
		}
	}
}

// golangciManagedFile returns the .golangci.yml managed-file descriptor so
// the guardrail tests render exactly what `forge upgrade` would emit.
func golangciManagedFile(t *testing.T) managedFile {
	t.Helper()
	for _, f := range managedFiles() {
		if f.destPath == ".golangci.yml" {
			return f
		}
	}
	t.Fatal(".golangci.yml not found in managedFiles()")
	return managedFile{}
}

func TestGolangciTypedAccessGuard_Variants(t *testing.T) {
	f := golangciManagedFile(t)

	render := func(mode string) string {
		cfg := testProjectConfig()
		cfg.Config.EnforceTypedAccess = mode
		out, err := renderManagedFile(f, buildTemplateData(cfg, ""))
		if err != nil {
			t.Fatalf("render %q: %v", mode, err)
		}
		return string(out)
	}

	// off → no forbidigo anywhere.
	off := render(config.EnforceTypedAccessOff)
	if strings.Contains(off, "forbidigo") {
		t.Errorf("off mode must not emit forbidigo, got:\n%s", off)
	}

	// warn → forbidigo SETTINGS + loader allowlist present, but NOT in the
	// gating linters.enable list (advisory pipeline step runs it instead).
	warn := render(config.EnforceTypedAccessWarn)
	enableBlock := warn[strings.Index(warn, "enable:"):strings.Index(warn, "settings:")]
	if strings.Contains(enableBlock, "forbidigo") {
		t.Errorf("warn mode must keep forbidigo OUT of linters.enable, got enable block:\n%s", enableBlock)
	}
	for _, want := range []string{"forbidigo:", `os\.Getenv`, `os\.LookupEnv`, `os\.Environ`,
		"(forge.v1.config) annotation", "//nolint:forbidigo", "path: ^pkg/config/"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warn mode missing %q in:\n%s", want, warn)
		}
	}

	// error → forbidigo ALSO in the gating linters.enable list.
	errMode := render(config.EnforceTypedAccessError)
	errEnable := errMode[strings.Index(errMode, "enable:"):strings.Index(errMode, "settings:")]
	if !strings.Contains(errEnable, "forbidigo") {
		t.Errorf("error mode must enable forbidigo (gating), got enable block:\n%s", errEnable)
	}
	if !strings.Contains(errMode, "path: ^pkg/config/") {
		t.Errorf("error mode must allowlist the loader package, got:\n%s", errMode)
	}

	// absent config: block → resolves to warn (advisory), same shape as warn.
	cfgAbsent := testProjectConfig()
	absent, err := renderManagedFile(f, buildTemplateData(cfgAbsent, ""))
	if err != nil {
		t.Fatalf("render absent: %v", err)
	}
	absentEnable := string(absent)[strings.Index(string(absent), "enable:"):strings.Index(string(absent), "settings:")]
	if strings.Contains(absentEnable, "forbidigo") {
		t.Errorf("absent config (→warn) must keep forbidigo out of enable, got:\n%s", absentEnable)
	}
	if !strings.Contains(string(absent), "forbidigo:") {
		t.Errorf("absent config (→warn) must emit forbidigo settings, got:\n%s", string(absent))
	}
}

func TestGolangciTypedAccessGuard_CustomLoaderPackage(t *testing.T) {
	f := golangciManagedFile(t)
	cfg := testProjectConfig()
	cfg.Config.EnforceTypedAccess = config.EnforceTypedAccessError
	cfg.Config.LoaderPackage = "internal/appconfig"
	out, err := renderManagedFile(f, buildTemplateData(cfg, ""))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "path: ^internal/appconfig/") {
		t.Errorf("custom loader_package not honored in exclusion, got:\n%s", string(out))
	}
}

// TestGolangciTypedAccessGuard_TeachingMessage pins the forbidigo `msg`
// content so the actionable teaching path can't silently regress. The
// message is the primary DX surface for both humans and LLMs hitting the
// guardrail, so it must name the concrete proto path, the regenerate step,
// and both opt-out levers (per-line nolint and the forge.yaml dial).
func TestGolangciTypedAccessGuard_TeachingMessage(t *testing.T) {
	f := golangciManagedFile(t)
	cfg := testProjectConfig()
	cfg.Config.EnforceTypedAccess = config.EnforceTypedAccessError
	out, err := renderManagedFile(f, buildTemplateData(cfg, ""))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"proto/config/v1/*.proto",                               // the concrete path to declare settings
		"forge generate",                                        // the regenerate step that produces the loader
		"(forge.v1.config) annotation",                          // the annotation that drives codegen
		"//nolint:forbidigo on this line",                       // per-line opt-out
		"config.enforce_typed_access to warn/off in forge.yaml", // project-wide dial
	} {
		if !strings.Contains(got, want) {
			t.Errorf("forbidigo teaching msg missing %q in:\n%s", want, got)
		}
	}
}

func TestSimpleDiff(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	new := []byte("line1\nmodified\nline3\n")

	diff := simpleDiff("test.go", old, new)
	if diff == "" {
		t.Fatal("simpleDiff returned empty string for different inputs")
	}
	if !strings.Contains(diff, "--- a/test.go") {
		t.Error("diff missing old file header")
	}
	if !strings.Contains(diff, "+++ b/test.go") {
		t.Error("diff missing new file header")
	}
	if !strings.Contains(diff, "-line2") {
		t.Error("diff missing removed line")
	}
	if !strings.Contains(diff, "+modified") {
		t.Error("diff missing added line")
	}
}

func TestSimpleDiffIdentical(t *testing.T) {
	content := []byte("line1\nline2\n")
	diff := simpleDiff("test.go", content, content)
	if diff != "" {
		t.Errorf("simpleDiff returned non-empty for identical inputs: %q", diff)
	}
}

func TestUpgradeUpToDate(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	// Create temp project with files matching templates
	dir := t.TempDir()

	// Write all managed files from templates. Use managedFilesForCfg so the
	// destPaths match the binary-scoped cmd/<bin>/ layout that Upgrade
	// (which also consults the cfg) expects.
	for _, f := range managedFilesForCfg(cfg) {
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Fatalf("render %s: %v", f.templateName, err)
		}
		dest := filepath.Join(dir, f.destPath)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, content, 0644); err != nil {
			t.Fatal(err)
		}
	}

	results, err := Upgrade(dir, cfg, false, true)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	for _, r := range results {
		if r.Status != UpgradeUpToDate {
			t.Errorf("%s: status = %q, want %q", r.Path, r.Status, UpgradeUpToDate)
		}
	}
}

func TestUpgradeDetectsModified(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Write files from templates, stamped (forge-certified renders).
	for _, f := range managedFilesForCfg(cfg) {
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Fatalf("render %s: %v", f.templateName, err)
		}
		writeManagedRender(t, dir, f.destPath, content, true)
	}

	// Modify a Tier 2 file (simulate user edit) — Tier 2 files are checksum-protected
	modifiedPath := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(modifiedPath, []byte("# user modified this file\nFROM golang:1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}

	results, err := Upgrade(dir, cfg, false, true)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Path == "Dockerfile" {
			found = true
			if r.Status != UpgradeUserModified {
				t.Errorf("Dockerfile: status = %q, want %q", r.Status, UpgradeUserModified)
			}
			if r.Diff == "" {
				t.Error("Dockerfile: expected non-empty diff for user-modified file")
			}
		}
	}
	if !found {
		t.Error("Dockerfile not found in results")
	}

	// Verify a Tier 1 file would still be overwritten even if modified
	for _, r := range results {
		if r.Path == "cmd/test-project/cmd/version.go" {
			// Tier 1 files report as up-to-date (since we didn't modify it) or updated
			if r.Status == UpgradeUserModified {
				t.Errorf("cmd/test-project/cmd/version.go: Tier 1 file should never be user-modified, got %q", r.Status)
			}
		}
	}
}

func TestUpgradeForceOverwrites(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Write files stamped (forge-certified renders).
	for _, f := range managedFilesForCfg(cfg) {
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Fatalf("render %s: %v", f.templateName, err)
		}
		writeManagedRender(t, dir, f.destPath, content, true)
	}

	// Modify a Tier-1 file and a Tier-2 file (user edits: markers gone).
	modifiedPath := filepath.Join(dir, "cmd/test-project/cmd/version.go")
	if err := os.WriteFile(modifiedPath, []byte("// user modified\n"), 0644); err != nil {
		t.Fatal(err)
	}
	modifiedTier2 := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(modifiedTier2, []byte("# user modified\nFROM golang:1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run with force=true, checkOnly=false
	results, err := Upgrade(dir, cfg, true, false)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	for _, r := range results {
		if r.Path == "cmd/test-project/cmd/version.go" || r.Path == "Dockerfile" {
			if r.Status != UpgradeUpdated {
				t.Errorf("%s: status = %q, want %q with --force", r.Path, r.Status, UpgradeUpdated)
			}
		}
	}

	// Verify the files were actually overwritten.
	content, err := os.ReadFile(modifiedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) == "// user modified\n" {
		t.Error("cmd/test-project/cmd/version.go was not overwritten by --force")
	}
	content, err = os.ReadFile(modifiedTier2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "# user modified") {
		t.Error("Dockerfile (user-modified Tier-2) was not overwritten by --force")
	}
}

func TestUpgradeAutoUpdatesUnmodified(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Write an older version of a static file, stamped — a pristine
	// forge render of an older vintage (the marker is the "checksum
	// matches disk" state).
	oldContent := []byte("// old template content\npackage main\n")
	writeManagedRender(t, dir, "cmd/test-project/cmd/version.go", oldContent, true)
	otelPath := filepath.Join(dir, "cmd", "test-project", "cmd", "version.go")

	// Write other files from current templates so they're up-to-date
	for _, f := range managedFilesForCfg(cfg) {
		if f.destPath == "cmd/test-project/cmd/version.go" {
			continue
		}
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Fatalf("render %s: %v", f.templateName, err)
		}
		writeManagedRender(t, dir, f.destPath, content, false)
	}

	// Run upgrade (not check-only, not force)
	results, err := Upgrade(dir, cfg, false, false)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	for _, r := range results {
		if r.Path == "cmd/test-project/cmd/version.go" {
			if r.Status != UpgradeUpdated {
				t.Errorf("cmd/test-project/cmd/version.go: status = %q, want %q (unmodified file should auto-update)", r.Status, UpgradeUpdated)
			}
		}
	}

	// Verify file was actually updated
	content, err := os.ReadFile(otelPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) == string(oldContent) {
		t.Error("cmd/test-project/cmd/version.go was not updated")
	}
}

// TestUpgradeAutoUpdatesStaleCodegen simulates the FORGE_BACKLOG #2 scenario:
// a Tier-2 file's template was updated, but the on-disk file is a *prior*
// render (not the current template, not user-edited). Pre-history, forge
// would flag this as user-modified (skipped). The legacy fix tracked a
// per-path render History; the self-certifying replacement is the
// embedded forge:hash marker — the stale prior render still VERIFIES,
// proving the user never touched it, so upgrade auto-updates it cleanly
// — no --force, no manual reconciliation required.
func TestUpgradeAutoUpdatesStaleCodegen(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Render every managed file to disk at the current template, except
	// the Dockerfile, which we materialize as a STAMPED stale prior
	// render — the "template updated, upgrade never ran" state.
	staleDockerfile := []byte("# stale prior-render Dockerfile\nFROM golang:1.18\n")
	for _, f := range managedFilesForCfg(cfg) {
		if f.destPath == "Dockerfile" {
			writeManagedRender(t, dir, f.destPath, staleDockerfile, true)
			continue
		}
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Fatalf("render %s: %v", f.templateName, err)
		}
		writeManagedRender(t, dir, f.destPath, content, false)
	}

	// Sanity: the on-disk content self-certifies as a forge render but
	// is not the current template's output.
	onDisk, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if checksums.Verify(onDisk) != checksums.Pristine {
		t.Fatalf("test setup: stale render must verify Pristine")
	}

	// Run upgrade (check-only is fine — we're asserting the classification).
	results, err := Upgrade(dir, cfg, false, true)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	var dockerfileResult *UpgradeResult
	for i, r := range results {
		if r.Path == "Dockerfile" {
			dockerfileResult = &results[i]
			break
		}
	}
	if dockerfileResult == nil {
		t.Fatal("Dockerfile not in upgrade results")
	}
	if dockerfileResult.Status == UpgradeUserModified {
		t.Errorf("Dockerfile status = %q (user-modified) — stale codegen should auto-update via the verifying marker", dockerfileResult.Status)
	}
	if dockerfileResult.Status != UpgradeUpdated {
		t.Errorf("Dockerfile status = %q, want %q (auto-update)", dockerfileResult.Status, UpgradeUpdated)
	}
}

// TestUpgradeSkipsDisownedFiles: a disowned (or legacy forked) entry is
// user-owned — `forge upgrade` must leave the on-disk file untouched
// while it exists, reporting it as skipped instead.
func TestUpgradeSkipsDisownedFiles(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()
	cs := &FileChecksums{Disowned: map[string]DisownedEntry{}, Unstampable: map[string]string{}}

	// Materialize every managed file pristine, then disown the first one
	// with hand-edited content.
	files := managedFilesForCfg(cfg)
	for _, f := range files {
		content, err := renderManagedFile(f, data)
		if err != nil {
			t.Fatalf("render %s: %v", f.templateName, err)
		}
		writeManagedRender(t, dir, f.destPath, content, false)
	}
	disownedPath := files[0].destPath
	userContent := []byte("# user-owned content after disown\n")
	if err := os.WriteFile(filepath.Join(dir, disownedPath), userContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := cs.DisownPaths(dir, []string{disownedPath}, "hand-maintained"); err != nil {
		t.Fatal(err)
	}
	if err := SaveChecksums(dir, cs); err != nil {
		t.Fatal(err)
	}

	results, err := Upgrade(dir, cfg, false, false)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	var sawSkip bool
	for _, r := range results {
		if r.Path == disownedPath {
			sawSkip = r.Status == UpgradeSkipped
		}
	}
	if !sawSkip {
		t.Errorf("disowned %s not reported as skipped: %+v", disownedPath, results)
	}
	got, err := os.ReadFile(filepath.Join(dir, disownedPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(userContent) {
		t.Errorf("upgrade overwrote a disowned file:\n%s", got)
	}
}

// TestUpgrade_SkipsThinMiddlewareInLegacyLayout: a pre-library-split
// project (old pkg/middleware mechanism files, no middleware.go) must
// NOT receive the thin policy pair from `forge upgrade` — the legacy
// files declare the same symbols and the package would stop compiling.
// Adoption is the user-driven migrations/v0.x-to-middleware-lib path.
func TestUpgrade_SkipsThinMiddlewareInLegacyLayout(t *testing.T) {
	dir := t.TempDir()
	mwDir := filepath.Join(dir, "pkg", "middleware")
	if err := os.MkdirAll(mwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"auth.go", "claims.go", "cors.go"} {
		if err := os.WriteFile(filepath.Join(mwDir, legacy), []byte("package middleware\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if !hasLegacyMiddlewareLayout(dir) {
		t.Fatal("legacy mechanism files present without middleware.go must be detected")
	}

	cfg := testProjectConfig()
	results, err := Upgrade(dir, cfg, false, true /* checkOnly */)
	if err != nil {
		t.Fatalf("Upgrade(checkOnly): %v", err)
	}
	for _, r := range results {
		if strings.HasPrefix(r.Path, "pkg/middleware/") && r.Status != UpgradeSkipped {
			t.Errorf("%s: want skipped in legacy layout, got %s", r.Path, r.Status)
		}
	}

	// Once the thin file exists, the layout is no longer legacy and
	// upgrade manages the pair normally.
	if err := os.WriteFile(filepath.Join(mwDir, "middleware.go"), []byte("package middleware\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasLegacyMiddlewareLayout(dir) {
		t.Fatal("a project with middleware.go is on the thin layout even if old files linger")
	}
}
