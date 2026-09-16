package buildinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestForgeRootFromFile exercises the pure upward walk behind runtime source
// discovery against fixture trees: it must find the forge root only when the
// module (github.com/reliant-labs/forge) and its pkg/ runtime-library
// directory are both present, and return "" for every shape that is not a
// genuine forge checkout.
func TestForgeRootFromFile(t *testing.T) {
	// Build a fixture forge checkout: <root>/go.mod (forge) + <root>/pkg/,
	// with a nested source file deep inside it. pkg/ carries no go.mod of its
	// own — it is a directory in the single forge module.
	root := t.TempDir()
	writeGoMod(t, root, "module "+forgeModulePath+"\n\ngo 1.26\n")
	mustMkdirAll(t, filepath.Join(root, "pkg"))
	deepFile := filepath.Join(root, "internal", "buildinfo", "buildinfo.go")
	mustMkdirAll(t, filepath.Dir(deepFile))

	if got := forgeRootFromFile(deepFile); got != root {
		t.Fatalf("valid checkout: forgeRootFromFile = %q, want %q", got, root)
	}

	// pkg/ missing → not a usable forge root (nothing to `use`).
	noPkg := t.TempDir()
	writeGoMod(t, noPkg, "module "+forgeModulePath+"\n\ngo 1.26\n")
	if got := forgeRootFromFile(filepath.Join(noPkg, "internal", "buildinfo", "x.go")); got != "" {
		t.Fatalf("missing pkg/: forgeRootFromFile = %q, want \"\"", got)
	}

	// A go.mod exists but declares a DIFFERENT module (e.g. an embedder like
	// reliant walking up past its own root) → not forge, return "".
	other := t.TempDir()
	writeGoMod(t, other, "module github.com/reliant-labs/reliant\n\ngo 1.26\n")
	if got := forgeRootFromFile(filepath.Join(other, "cmd", "reliant", "main.go")); got != "" {
		t.Fatalf("foreign module root: forgeRootFromFile = %q, want \"\"", got)
	}

	// No go.mod anywhere up the tree (e.g. a -trimpath stub path that does not
	// exist on disk resolves to nothing) → "".
	if got := forgeRootFromFile(filepath.Join(t.TempDir(), "a", "b", "c.go")); got != "" {
		t.Fatalf("no go.mod on path: forgeRootFromFile = %q, want \"\"", got)
	}

	// Module path right, but `pkg` is a FILE → reject. With pkg/ no longer
	// carrying its own go.mod, "is it a directory" is the entire remaining
	// discriminator, so it gets a case of its own.
	pkgIsFile := t.TempDir()
	writeGoMod(t, pkgIsFile, "module "+forgeModulePath+"\n\ngo 1.26\n")
	if err := os.WriteFile(filepath.Join(pkgIsFile, "pkg"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write pkg file: %v", err)
	}
	if got := forgeRootFromFile(filepath.Join(pkgIsFile, "internal", "x.go")); got != "" {
		t.Fatalf("pkg is a file: forgeRootFromFile = %q, want \"\"", got)
	}
}

// TestDiscoverDevForgeRootFromSource is the live end-to-end path: because this
// test file IS compiled from a real forge checkout (and `go test` does not
// trimpath), discovery must resolve to a directory that actually contains this
// package's source.
func TestDiscoverDevForgeRootFromSource(t *testing.T) {
	root := DiscoverDevForgeRootFromSource()
	if root == "" {
		t.Skip("no on-disk source root discoverable (e.g. -trimpath test binary); walk logic covered by TestForgeRootFromFile")
	}
	// The discovered root must hold the markers we require: the module's
	// go.mod, and the pkg/ directory the runtime libraries live in.
	for _, rel := range []string{"go.mod", "pkg"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("discovered root %q missing %s: %v", root, rel, err)
		}
	}
	// And this test's own package must live under it.
	if _, err := os.Stat(filepath.Join(root, "internal", "buildinfo", "discover_test.go")); err != nil {
		t.Fatalf("discovered root %q does not contain this package's source: %v", root, err)
	}
}

func writeGoMod(t *testing.T, dir, contents string) {
	t.Helper()
	mustMkdirAll(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write go.mod in %s: %v", dir, err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}
