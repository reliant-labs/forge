package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGenerateConfigLoader_DefaultScaffoldCompiles renders the DEFAULT
// scaffold config (the exact shape every new project gets) and compiles
// it with the host toolchain. Template syntax tests catch parse errors;
// this catches type errors (generics inference, unused imports, typed
// duration assignments).
func TestGenerateConfigLoader_DefaultScaffoldCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("compile check skipped in -short")
	}
	dir := t.TempDir()
	if err := GenerateConfigLoader(DefaultConfigMessages(), dir, nil); err != nil {
		t.Fatalf("GenerateConfigLoader: %v", err)
	}
	src := filepath.Join(dir, "pkg", "config", "config.go")

	// Build inside a scratch package of THIS module so cobra resolves
	// from forge's own go.mod (no network).
	scratch := t.TempDir()
	// go build needs the file under the module; copy into a temp dir
	// under the repo root.
	repoScratch, err := os.MkdirTemp(repoRootForCompileCheck(t), "cfgcompile-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoScratch) })
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoScratch, "config.go"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(scratch, "out.a"), "./"+filepath.Base(repoScratch))
	cmd.Dir = filepath.Dir(repoScratch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated config.go does not compile: %v\n%s\n--- SOURCE ---\n%s", err, out, data)
	}
}

// repoRootForCompileCheck walks up from the package dir to the go.mod root.
func repoRootForCompileCheck(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above package dir")
		}
		dir = parent
	}
}
