// Tests for `forge fmt`. The interesting behavior is the `-local`
// derivation (forge.yaml > go.mod fallback) and the end-to-end
// "project-local import gets bucketed into its own group" effect.
//
// Goimports must be on PATH for the end-to-end test to run; if not,
// the test skips rather than failing — the unit half of the suite
// (resolveFmtModulePath / defaultFmtTargets) still runs.
package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveFmtModulePath_FromGoMod confirms the go.mod fallback
// path: no forge.yaml, well-formed go.mod → module directive value.
func TestResolveFmtModulePath_FromGoMod(t *testing.T) {
	dir := t.TempDir()
	writeFmtFile(t, filepath.Join(dir, "go.mod"), "module github.com/example/widget\n\ngo 1.22\n")
	withFmtDir(t, dir, func() {
		got := resolveFmtModulePath(dir)
		if got != "github.com/example/widget" {
			t.Errorf("module path = %q, want github.com/example/widget", got)
		}
	})
}

// TestResolveFmtModulePath_Empty confirms the all-fallbacks-fail
// branch: empty directory → empty string return (caller logs a
// warning and runs goimports without -local).
func TestResolveFmtModulePath_Empty(t *testing.T) {
	dir := t.TempDir()
	withFmtDir(t, dir, func() {
		got := resolveFmtModulePath(dir)
		if got != "" {
			t.Errorf("module path = %q, want empty (no forge.yaml, no go.mod)", got)
		}
	})
}

// TestDefaultFmtTargets_FiltersMissingDirs pins the existence-aware
// filter: only the directories that actually exist make it into the
// returned target list.
func TestDefaultFmtTargets_FiltersMissingDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := defaultFmtTargets(dir)
	wantSet := map[string]bool{"cmd": true, "internal": true}
	if len(got) != 2 {
		t.Fatalf("got %d targets (%v), want 2", len(got), got)
	}
	for _, name := range got {
		if !wantSet[name] {
			t.Errorf("unexpected target %q", name)
		}
	}
}

// TestRunFmt_GroupsLocalImports is the end-to-end test: write a Go
// file where a project-local import is interleaved with stdlib +
// third-party, run `forge fmt`, verify goimports re-grouped to
// stdlib / third-party / local.
//
// Skips when goimports is not on PATH so the test suite stays runnable
// on minimal CI environments.
func TestRunFmt_GroupsLocalImports(t *testing.T) {
	if _, err := exec.LookPath("goimports"); err != nil {
		t.Skip("goimports not on PATH; install with `go install golang.org/x/tools/cmd/goimports@latest` to run this test")
	}

	dir := t.TempDir()
	writeFmtFile(t, filepath.Join(dir, "go.mod"), "module example.com/widget\n\ngo 1.22\n")

	// Three imports: one stdlib (fmt), one third-party (cobra-like),
	// one project-local. goimports without -local would sort cobra
	// alphabetically into the same group as widget/foo; WITH -local
	// example.com/widget gets its own group.
	srcPath := filepath.Join(dir, "main.go")
	writeFmtFile(t, srcPath, `package main

import (
	"fmt"

	"example.com/widget/foo"
	"github.com/spf13/cobra"
)

func main() {
	_ = cobra.Command{}
	_ = foo.X
	fmt.Println("hi")
}
`)
	// Stub the local package so goimports doesn't drop the import as
	// "package not found" before we can check the grouping.
	if err := os.MkdirAll(filepath.Join(dir, "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFmtFile(t, filepath.Join(dir, "foo", "foo.go"), "package foo\n\nvar X = 1\n")

	withFmtDir(t, dir, func() {
		if err := runFmt(context.Background(), []string{"main.go"}); err != nil {
			t.Fatalf("runFmt: %v", err)
		}
	})

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	// Expected grouping after goimports -local example.com/widget:
	//   - stdlib (fmt)
	//   - third-party (cobra)
	//   - first-party (example.com/widget/foo) in its own bucket
	// Verify by counting blank lines in the import block — three groups
	// means two blank lines separating them.
	text := string(got)
	importStart := strings.Index(text, "import (")
	importEnd := strings.Index(text[importStart:], ")")
	if importStart < 0 || importEnd < 0 {
		t.Fatalf("could not find import block in:\n%s", text)
	}
	importBlock := text[importStart : importStart+importEnd]
	blanks := strings.Count(importBlock, "\n\n")
	if blanks < 2 {
		t.Errorf("expected at least 2 blank lines in import block (3 groups), got %d.\nimport block:\n%s", blanks, importBlock)
	}
	// Specifically check that the third-party (cobra) appears BEFORE the
	// project-local (example.com/widget) — goimports sorts groups in
	// stdlib / third-party / local order.
	cobraIdx := strings.Index(text, `"github.com/spf13/cobra"`)
	localIdx := strings.Index(text, `"example.com/widget/foo"`)
	if cobraIdx < 0 || localIdx < 0 {
		t.Fatalf("missing expected imports; got:\n%s", text)
	}
	if cobraIdx >= localIdx {
		t.Errorf("project-local import should be sorted AFTER third-party; got:\n%s", text)
	}
}

// writeFmtFile is a small helper — t.TempDir+os.WriteFile boilerplate
// repeats often enough that a one-liner is worth it. Local to this
// file (prefixed Fmt) so it doesn't collide with similarly named
// helpers elsewhere in the package.
func writeFmtFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// withFmtDir runs fn with os.Getwd pointing at dir, then restores the
// prior cwd. runFmt and resolveFmtModulePath both read from cwd so
// the test needs to chdir into the temp tree.
func withFmtDir(t *testing.T, dir string, fn func()) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(orig)
	}()
	fn()
}
