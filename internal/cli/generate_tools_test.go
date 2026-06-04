package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureGenGoMod_BootstrapsFreshWorktree covers the headline case:
// a checkout has `go.work` declaring `use gen`, a root `go.mod` with a
// module directive, but `gen/go.mod` is missing on disk (the situation a
// fresh worktree carved from a never-generated checkout lands in).
func TestEnsureGenGoMod_BootstrapsFreshWorktree(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.26.2\n")
	writeForTest(t, filepath.Join(dir, "go.work"), "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")

	if err := ensureGenGoMod(dir); err != nil {
		t.Fatalf("ensureGenGoMod: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "gen", "go.mod"))
	if err != nil {
		t.Fatalf("expected gen/go.mod to be created: %v", err)
	}
	if !strings.Contains(string(body), "module example.com/proj/gen") {
		t.Errorf("rendered gen/go.mod missing expected module line; got:\n%s", body)
	}
	if !strings.Contains(string(body), "go 1.26.2") {
		t.Errorf("rendered gen/go.mod missing go directive; got:\n%s", body)
	}
}

// TestEnsureGenGoMod_NoopWhenPresent confirms an existing gen/go.mod is
// left untouched.
func TestEnsureGenGoMod_NoopWhenPresent(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.26.2\n")
	writeForTest(t, filepath.Join(dir, "go.work"), "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
	writeForTest(t, filepath.Join(dir, "gen", "go.mod"), "// hand-edited\n")

	if err := ensureGenGoMod(dir); err != nil {
		t.Fatalf("ensureGenGoMod: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "gen", "go.mod"))
	if string(body) != "// hand-edited\n" {
		t.Errorf("ensureGenGoMod clobbered an existing gen/go.mod; content=%q", body)
	}
}

// TestEnsureGenGoMod_NoGoWorkSkips confirms we don't synthesize a
// gen/go.mod for projects that aren't using a workspace at all (no
// go.work file means the missing gen/ is by design).
func TestEnsureGenGoMod_NoGoWorkSkips(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.26.2\n")

	if err := ensureGenGoMod(dir); err != nil {
		t.Fatalf("ensureGenGoMod: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen", "go.mod")); !os.IsNotExist(err) {
		t.Errorf("ensureGenGoMod created gen/go.mod despite no go.work declaring the gen workspace")
	}
}

// writeForTest is a small fixture helper that ensures the parent dir
// exists before writing. The package's other mustWrite helper
// (generate_cleanup_test.go) assumes the parent exists; the gen/go.mod
// test paths need an extra os.MkdirAll, so we keep a local variant
// rather than churn the existing helper.
func writeForTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
