package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func testProjectConfig() *config.ProjectConfig {
	return &config.ProjectConfig{
		Name:       "test-project",
		ModulePath: "github.com/example/test-project",
		Services: []config.ServiceConfig{
			{Name: "api", Port: 8080},
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
		"cmd/server.go":            true,
		"cmd/main.go":              true,
		"cmd/version.go":           true,
		"cmd/otel.go":              true,
		"Taskfile.yml":             true,
		"Dockerfile":               true,
		"docker-compose.yml":       true,
		".golangci.yml":            true,
		"pkg/middleware/cors.go":   true,
		"pkg/middleware/auth.go":   true,
		"pkg/middleware/claims.go": true,
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

	// Write all managed files from templates
	for _, f := range managedFiles() {
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

	// Write files from templates, record checksums
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	for _, f := range managedFiles() {
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
		cs.RecordFile(f.destPath, content)
	}
	if err := SaveChecksums(dir, cs); err != nil {
		t.Fatal(err)
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
		if r.Path == "cmd/otel.go" {
			// Tier 1 files report as up-to-date (since we didn't modify it) or updated
			if r.Status == UpgradeUserModified {
				t.Errorf("cmd/otel.go: Tier 1 file should never be user-modified, got %q", r.Status)
			}
		}
	}
}

func TestUpgradeForceOverwrites(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Write files, record checksums
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	for _, f := range managedFiles() {
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
		cs.RecordFile(f.destPath, content)
	}
	if err := SaveChecksums(dir, cs); err != nil {
		t.Fatal(err)
	}

	// Modify a file
	modifiedPath := filepath.Join(dir, "cmd/otel.go")
	if err := os.WriteFile(modifiedPath, []byte("// user modified\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run with force=true, checkOnly=false
	results, err := Upgrade(dir, cfg, true, false)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	for _, r := range results {
		if r.Path == "cmd/otel.go" {
			if r.Status != UpgradeUpdated {
				t.Errorf("cmd/otel.go: status = %q, want %q with --force", r.Status, UpgradeUpdated)
			}
		}
	}

	// Verify the file was actually overwritten
	content, err := os.ReadFile(modifiedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) == "// user modified\n" {
		t.Error("cmd/otel.go was not overwritten by --force")
	}
}

func TestUpgradeAutoUpdatesUnmodified(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Write an older version of a static file with a recorded checksum
	oldContent := []byte("// old template content\npackage main\n")
	otelPath := filepath.Join(dir, "cmd", "otel.go")
	if err := os.MkdirAll(filepath.Dir(otelPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otelPath, oldContent, 0644); err != nil {
		t.Fatal(err)
	}

	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	cs.RecordFile("cmd/otel.go", oldContent) // checksum matches disk
	if err := SaveChecksums(dir, cs); err != nil {
		t.Fatal(err)
	}

	// Write other files from current templates so they're up-to-date
	for _, f := range managedFiles() {
		if f.destPath == "cmd/otel.go" {
			continue
		}
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

	// Run upgrade (not check-only, not force)
	results, err := Upgrade(dir, cfg, false, false)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	for _, r := range results {
		if r.Path == "cmd/otel.go" {
			if r.Status != UpgradeUpdated {
				t.Errorf("cmd/otel.go: status = %q, want %q (unmodified file should auto-update)", r.Status, UpgradeUpdated)
			}
		}
	}

	// Verify file was actually updated
	content, err := os.ReadFile(otelPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) == string(oldContent) {
		t.Error("cmd/otel.go was not updated")
	}
}

// TestUpgradeAutoUpdatesStaleCodegen simulates the FORGE_BACKLOG #2 scenario:
// a Tier-2 file's template was updated, but the on-disk file is a *prior*
// render (not the current template, not user-edited). Pre-history, forge
// would flag this as user-modified (skipped) because the stored checksum
// didn't match the on-disk hash. With prior-render history tracking, forge
// recognises the on-disk content as a known prior render and auto-updates
// it cleanly — no --force, no manual reconciliation required.
//
// Concretely, we:
//
//  1. Render Tier-2 file v1 via the current template, write to disk, record
//     checksum H1 (Hash=H1, History=[H1]).
//  2. Simulate a template update by re-rendering with patched content v2
//     and recording H2 (Hash=H2, History=[H1, H2]). The on-disk file is
//     left at v1 — that's the "stale codegen" state we're testing.
//  3. Patch the on-disk file to a v3 the user never wrote — but we will
//     stub the upgrade-time render to return v3 so the comparison is
//     between disk=v1 and template=v3, with H1 in history.
//  4. Call Upgrade and assert the Tier-2 file reports UpgradeUpdated
//     (auto-update path) rather than UpgradeUserModified.
//
// The test uses the real Dockerfile template path because that's a Tier-2
// template that's part of the canonical managedFiles list; we synthesize
// the template-update by hand-recording an extra hash into history.
func TestUpgradeAutoUpdatesStaleCodegen(t *testing.T) {
	cfg := testProjectConfig()
	data := buildTemplateData(cfg, "")

	dir := t.TempDir()

	// Step 1+2: render every managed file to disk. For the file we'll
	// exercise (Dockerfile), record an additional fake "prior render"
	// hash in history so the on-disk content (current render) sits at
	// the tail and the older fake hash sits in history. We then mutate
	// the disk content to the older fake content — that's our stale
	// codegen.
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	for _, f := range managedFiles() {
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
		cs.RecordFile(f.destPath, content)
	}

	// Inject a "prior render" of Dockerfile that no longer matches what
	// the current template renders. This is what would happen in real
	// life: the template was updated between v1 and v2, the user never
	// re-ran upgrade, and the on-disk file is the v1 render.
	staleDockerfile := []byte("# stale prior-render Dockerfile\nFROM golang:1.18\n")
	cs.RecordFile("Dockerfile", staleDockerfile) // history now: [<current>, staleHash]
	// Then overwrite with the current render again, so Hash points to the
	// current template output but staleHash sits earlier in History.
	currentDockerfile, err := renderManagedFile(managedFiles()[findManagedIdx(t, "Dockerfile")], data)
	if err != nil {
		t.Fatal(err)
	}
	cs.RecordFile("Dockerfile", currentDockerfile)
	if err := SaveChecksums(dir, cs); err != nil {
		t.Fatal(err)
	}

	// Now flip the on-disk Dockerfile back to the stale prior-render
	// content. This is the "stale codegen" state.
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), staleDockerfile, 0644); err != nil {
		t.Fatal(err)
	}

	// Sanity: the disk content matches a prior-render in history but not
	// the current Hash.
	if !cs.MatchesAnyKnownRender("Dockerfile", staleDockerfile) {
		t.Fatalf("test setup: stale content should match a prior render in history")
	}
	if cs.Files["Dockerfile"].Hash == HashContent(staleDockerfile) {
		t.Fatalf("test setup: current Hash should not equal stale-render hash")
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
		t.Errorf("Dockerfile status = %q (user-modified) — stale codegen should auto-update via history match", dockerfileResult.Status)
	}
	if dockerfileResult.Status != UpgradeUpdated {
		t.Errorf("Dockerfile status = %q, want %q (auto-update)", dockerfileResult.Status, UpgradeUpdated)
	}
}

// findManagedIdx returns the index of the managed file with the given
// destPath. Test helper; fails the test if not found.
func findManagedIdx(t *testing.T, destPath string) int {
	t.Helper()
	for i, f := range managedFiles() {
		if f.destPath == destPath {
			return i
		}
	}
	t.Fatalf("managed file %q not found", destPath)
	return -1
}