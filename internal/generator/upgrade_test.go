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
	cs := &FileChecksums{Files: make(map[string]string)}
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
	cs := &FileChecksums{Files: make(map[string]string)}
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

	cs := &FileChecksums{Files: make(map[string]string)}
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