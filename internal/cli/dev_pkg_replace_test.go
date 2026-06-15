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

// TestSyncDevForgePkgReplace_LocalVendorNoSiblingIsSilentNoop pins the
// fix for kalshi-trader FORGE_BACKLOG #14: go.mod already points the
// replace at ./.forge-pkg and there is NO sibling <parent>/forge
// checkout to refresh from. The vendored copy is the source of truth in
// that layout — the sync must report vendored=true with NO error.
// Pre-fix, sourceDir stayed "" and fell into looksLikeForgePkgDir(""),
// emitting `replace target "" does not look like forge/pkg ... refusing
// to vendor` on every `forge generate`.
func TestSyncDevForgePkgReplace_LocalVendorNoSiblingIsSilentNoop(t *testing.T) {
	root := t.TempDir()
	// Project nested one level down so its parent (root) demonstrably
	// has no `forge/` sibling checkout.
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace " + forgePkgModulePath + " => ./" + localForgePkgVendorDir + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	// Vendored copy present (the steady state the warning fired in).
	fakeForgePkgDir(t, filepath.Join(project, localForgePkgVendorDir), "vendored")

	vendored, err := syncDevForgePkgReplace(project)
	if err != nil {
		t.Fatalf("expected silent no-op, got error: %v", err)
	}
	if !vendored {
		t.Fatal("expected vendored=true (vendor dir present)")
	}
}

// TestSyncDevForgePkgReplace_LocalVendorNoSiblingNoVendorDir covers the
// degenerate variant of the same path: replace points at ./.forge-pkg
// but the vendor dir was deleted and no sibling exists to rebuild it
// from. Still no error — there is nothing to sync FROM, so warning
// about a failed sync would be misleading. vendored=false lets the
// Dockerfile COPY gate do the right thing.
func TestSyncDevForgePkgReplace_LocalVendorNoSiblingNoVendorDir(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nreplace " + forgePkgModulePath + " => ./" + localForgePkgVendorDir + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	vendored, err := syncDevForgePkgReplace(project)
	if err != nil {
		t.Fatalf("expected silent no-op, got error: %v", err)
	}
	if vendored {
		t.Fatal("expected vendored=false (no vendor dir on disk)")
	}
}

// TestSyncDevForgePkgReplace_CleanVersionPinUntouched pins the
// release-flow safety contract (docs/pkg-versioning.md): a project whose
// go.mod carries a clean `require github.com/reliant-labs/forge/pkg
// vX.Y.Z` pin and NO replace must pass through `forge generate`'s
// vendor-sync byte-identical — even when a sibling forge checkout sits
// right next to it on disk. Rewriting the pin into a replace (or
// warning about it) would silently drag released projects back onto the
// dev path.
func TestSyncDevForgePkgReplace_CleanVersionPinUntouched(t *testing.T) {
	root := t.TempDir()
	// Sibling forge checkout present — the strongest temptation for the
	// sync to "help".
	fakeForgePkgDir(t, filepath.Join(root, "forge", "pkg"), "sibling")

	project := filepath.Join(root, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/myproj\n\ngo 1.22\n\nrequire " + forgePkgModulePath + " v0.3.0\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	vendored, err := syncDevForgePkgReplace(project)
	if err != nil {
		t.Fatalf("clean pin must not produce an error/warning, got: %v", err)
	}
	if vendored {
		t.Fatal("expected vendored=false (pinned release mode, no vendor dir)")
	}
	if _, statErr := os.Stat(filepath.Join(project, localForgePkgVendorDir)); !os.IsNotExist(statErr) {
		t.Fatalf("sync must not create %s/ for a pinned project", localForgePkgVendorDir)
	}
	got, err := os.ReadFile(filepath.Join(project, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != gomod {
		t.Fatalf("go.mod was modified.\nbefore:\n%s\nafter:\n%s", gomod, string(got))
	}
}
