package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeForgePkgDir creates a minimal directory tree that looksLikeForgePkgDir
// will accept: a go.mod declaring the canonical forge/pkg module path plus
// at least one source file so syncDir has something to copy.
func fakeForgePkgDir(t *testing.T, root, marker string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	gomod := "module " + forgePkgModulePath + "\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "auth")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(subdir, "auth.go"),
		[]byte("package auth // marker:"+marker+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInspectDevPkgReplace_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/x\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => /tmp/somewhere/forge/pkg\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := inspectDevPkgReplace(dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.HasReplace || !st.IsAbsolutePath || st.IsLocalVendor {
		t.Fatalf("unexpected state: %+v", st)
	}
	if st.Target != "/tmp/somewhere/forge/pkg" {
		t.Fatalf("target = %q, want /tmp/somewhere/forge/pkg", st.Target)
	}
}

func TestInspectDevPkgReplace_LocalVendor(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/x\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => ./.forge-pkg\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := inspectDevPkgReplace(dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.HasReplace || st.IsAbsolutePath || !st.IsLocalVendor {
		t.Fatalf("unexpected state: %+v", st)
	}
}

func TestInspectDevPkgReplace_NoReplace(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/x\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := inspectDevPkgReplace(dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.HasReplace {
		t.Fatalf("expected no replace, got %+v", st)
	}
}

func TestSyncDevForgePkgReplace_RewritesAbsolutePath(t *testing.T) {
	root := t.TempDir()
	srcForge := fakeForgePkgDir(t, filepath.Join(root, "forge", "pkg"), "v1")
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => " + srcForge + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	vendored, err := syncDevForgePkgReplace(project)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !vendored {
		t.Fatal("expected vendored=true after sync")
	}
	if _, err := os.Stat(filepath.Join(project, ".forge-pkg", "go.mod")); err != nil {
		t.Fatalf(".forge-pkg/go.mod not present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".forge-pkg", "auth", "auth.go")); err != nil {
		t.Fatalf(".forge-pkg/auth/auth.go not present: %v", err)
	}

	// go.mod replace should now be the relative form.
	rewritten, _ := os.ReadFile(filepath.Join(project, "go.mod"))
	if !strings.Contains(string(rewritten), "replace github.com/reliant-labs/forge/pkg => ./.forge-pkg") {
		t.Fatalf("go.mod replace not rewritten:\n%s", string(rewritten))
	}
	if strings.Contains(string(rewritten), srcForge) {
		t.Fatalf("absolute path still in go.mod:\n%s", string(rewritten))
	}
}

func TestSyncDevForgePkgReplace_Idempotent(t *testing.T) {
	root := t.TempDir()
	srcForge := fakeForgePkgDir(t, filepath.Join(root, "forge", "pkg"), "v1")
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => " + srcForge + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := syncDevForgePkgReplace(project); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(project, ".forge-pkg", "auth", "auth.go"))

	// Re-running shouldn't break anything; relative form points at the
	// .forge-pkg dir which means siblingForgePkg won't be consulted (no
	// sibling forge/pkg next to project), so a no-op refresh.
	if _, err := syncDevForgePkgReplace(project); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(project, ".forge-pkg", "auth", "auth.go"))
	if string(first) != string(second) {
		t.Fatalf("idempotency violated:\n  first  = %s\n  second = %s", first, second)
	}
}

func TestSyncDevForgePkgReplace_RefreshesFromSibling(t *testing.T) {
	root := t.TempDir()
	srcForge := fakeForgePkgDir(t, filepath.Join(root, "forge", "pkg"), "v1")
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// Project already in vendored state; src has v1.
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => ./.forge-pkg\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := syncDevForgePkgReplace(project); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	got1, _ := os.ReadFile(filepath.Join(project, ".forge-pkg", "auth", "auth.go"))
	if !strings.Contains(string(got1), "marker:v1") {
		t.Fatalf("first sync did not pull v1 from sibling: %s", got1)
	}

	// Bump the source: rewrite auth.go to v2 and re-run.
	if err := os.WriteFile(
		filepath.Join(srcForge, "auth", "auth.go"),
		[]byte("package auth // marker:v2\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := syncDevForgePkgReplace(project); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	got2, _ := os.ReadFile(filepath.Join(project, ".forge-pkg", "auth", "auth.go"))
	if !strings.Contains(string(got2), "marker:v2") {
		t.Fatalf("refresh from sibling did not pick up v2: %s", got2)
	}
}

func TestSyncDevForgePkgReplace_RemovesStaleFiles(t *testing.T) {
	root := t.TempDir()
	srcForge := fakeForgePkgDir(t, filepath.Join(root, "forge", "pkg"), "v1")
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => " + srcForge + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a stale file under .forge-pkg/ that is NOT in the source.
	stale := filepath.Join(project, ".forge-pkg", "stale", "ghost.go")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("package stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := syncDevForgePkgReplace(project); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file not removed: err=%v", err)
	}
}

func TestSyncDevForgePkgReplace_RejectsNonForgePkg(t *testing.T) {
	root := t.TempDir()
	// Source has go.mod but with a different module — should be rejected.
	bogus := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(bogus, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bogus, "go.mod"),
		[]byte("module example.com/not-forge\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace github.com/reliant-labs/forge/pkg => " + bogus + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := syncDevForgePkgReplace(project)
	if err == nil {
		t.Fatal("expected error when replace target isn't forge/pkg")
	}
	if !strings.Contains(err.Error(), "does not look like forge/pkg") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncDevForgePkgReplace_NoReplaceNoVendor(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/x\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	vendored, err := syncDevForgePkgReplace(dir)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if vendored {
		t.Fatal("expected vendored=false (no replace, no vendor)")
	}
}
