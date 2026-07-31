package cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// starterGoWork is the workspace the project generator emits before the dev
// bridge runs: the main module + its gen/ submodule.
const starterGoWork = "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n"

// usePaths returns the disk paths of every `use` directive in the go.work at
// path, parsed with the real modfile parser.
func usePaths(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read go.work: %v", err)
	}
	wf, err := modfile.ParseWork(path, data, nil)
	if err != nil {
		t.Fatalf("parse go.work: %v", err)
	}
	paths := make([]string, 0, len(wf.Use))
	for _, u := range wf.Use {
		paths = append(paths, u.Path)
	}
	return paths
}

func containsUse(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestWriteDevForgeGoWork_DevBuildAddsPkgUse: a dev build with a stamped
// source root augments the starter go.work with `use <root>/pkg`, preserving
// the existing `.` and `gen` uses. It does NOT add the main forge module (the
// scaffold imports only forge/pkg).
func TestWriteDevForgeGoWork_DevBuildAddsPkgUse(t *testing.T) {
	dir := t.TempDir()
	workPath := filepath.Join(dir, "go.work")
	if err := os.WriteFile(workPath, []byte(starterGoWork), 0o644); err != nil {
		t.Fatal(err)
	}

	forgeRoot := t.TempDir()
	buildinfo.SetDevBuild(true)
	t.Cleanup(buildinfo.ClearDevBuild)
	prev := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = forgeRoot
	t.Cleanup(func() { buildinfo.DevForgeRoot = prev })

	writeDevForgeGoWork(dir)

	got := usePaths(t, workPath)
	wantPkg := filepath.Join(forgeRoot, "pkg")
	if !containsUse(got, wantPkg) {
		t.Errorf("go.work missing `use %s`; uses = %v", wantPkg, got)
	}
	if !containsUse(got, ".") || !containsUse(got, "gen") {
		t.Errorf("dev bridge dropped the starter uses; uses = %v", got)
	}
	if containsUse(got, forgeRoot) {
		t.Errorf("dev bridge added the main forge module `use %s`; only forge/pkg is imported by scaffolds. uses = %v", forgeRoot, got)
	}
}

// TestWriteDevForgeGoWork_Idempotent: running the bridge twice yields exactly
// one forge/pkg use (a re-scaffold or repeated call must not duplicate lines).
func TestWriteDevForgeGoWork_Idempotent(t *testing.T) {
	dir := t.TempDir()
	workPath := filepath.Join(dir, "go.work")
	if err := os.WriteFile(workPath, []byte(starterGoWork), 0o644); err != nil {
		t.Fatal(err)
	}

	forgeRoot := t.TempDir()
	buildinfo.SetDevBuild(true)
	t.Cleanup(buildinfo.ClearDevBuild)
	prev := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = forgeRoot
	t.Cleanup(func() { buildinfo.DevForgeRoot = prev })

	writeDevForgeGoWork(dir)
	writeDevForgeGoWork(dir)

	wantPkg := filepath.Join(forgeRoot, "pkg")
	count := 0
	for _, p := range usePaths(t, workPath) {
		if p == wantPkg {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one `use %s`, got %d", wantPkg, count)
	}
}

// TestWriteDevForgeGoWork_ReleaseBuildNoOp: a released binary never writes a
// bridge, even if a DevForgeRoot value somehow leaked in.
func TestWriteDevForgeGoWork_ReleaseBuildNoOp(t *testing.T) {
	dir := t.TempDir()
	workPath := filepath.Join(dir, "go.work")
	if err := os.WriteFile(workPath, []byte(starterGoWork), 0o644); err != nil {
		t.Fatal(err)
	}

	buildinfo.SetDevBuild(false)
	t.Cleanup(buildinfo.ClearDevBuild)
	prev := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = t.TempDir() // leaked value must be ignored
	t.Cleanup(func() { buildinfo.DevForgeRoot = prev })

	writeDevForgeGoWork(dir)

	after, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != starterGoWork {
		t.Errorf("release build modified go.work:\n%s", string(after))
	}
}

// TestWriteDevForgeGoWork_DevBuildNoRootNoWrite: a dev build with neither a
// stamped source root NOR a discoverable one on disk (e.g. a trimpath'd or
// shipped dev binary) leaves go.work untouched — it emits a hint instead of
// guessing a path. Discovery is pinned to "" here because under `go test`
// runtime.Caller would otherwise resolve to the live forge checkout.
func TestWriteDevForgeGoWork_DevBuildNoRootNoWrite(t *testing.T) {
	dir := t.TempDir()
	workPath := filepath.Join(dir, "go.work")
	if err := os.WriteFile(workPath, []byte(starterGoWork), 0o644); err != nil {
		t.Fatal(err)
	}

	buildinfo.SetDevBuild(true)
	t.Cleanup(buildinfo.ClearDevBuild)
	prev := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = ""
	t.Cleanup(func() { buildinfo.DevForgeRoot = prev })
	buildinfo.SetDiscoveredForgeRoot("") // nothing discoverable on disk
	t.Cleanup(buildinfo.ClearDiscoveredForgeRoot)

	writeDevForgeGoWork(dir)

	after, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != starterGoWork {
		t.Errorf("dev build with empty DevForgeRoot modified go.work:\n%s", string(after))
	}
}

// TestWriteDevForgeGoWork_DevBuildDiscoversRoot: a dev build with NO stamped
// DevForgeRoot still bridges when the forge source is discoverable at runtime
// (the embedded-in-reliant case, where the host binary never stamped forge's
// ldflag). The discovered root is used exactly like a stamped one.
func TestWriteDevForgeGoWork_DevBuildDiscoversRoot(t *testing.T) {
	dir := t.TempDir()
	workPath := filepath.Join(dir, "go.work")
	if err := os.WriteFile(workPath, []byte(starterGoWork), 0o644); err != nil {
		t.Fatal(err)
	}

	forgeRoot := t.TempDir()
	buildinfo.SetDevBuild(true)
	t.Cleanup(buildinfo.ClearDevBuild)
	prev := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = "" // no ldflag stamp — force the discovery fallback
	t.Cleanup(func() { buildinfo.DevForgeRoot = prev })
	buildinfo.SetDiscoveredForgeRoot(forgeRoot)
	t.Cleanup(buildinfo.ClearDiscoveredForgeRoot)

	writeDevForgeGoWork(dir)

	want := filepath.Join(forgeRoot, "pkg")
	if got := usePaths(t, workPath); !containsUse(got, want) {
		t.Errorf("discovered-root bridge missing %q; go.work uses = %v", want, got)
	}
}

// TestWriteDevForgeGoWork_NoGoWorkNoCreate: with no go.work present (e.g. a
// library scaffold), the bridge is a no-op and creates no file.
func TestWriteDevForgeGoWork_NoGoWorkNoCreate(t *testing.T) {
	dir := t.TempDir()

	buildinfo.SetDevBuild(true)
	t.Cleanup(buildinfo.ClearDevBuild)
	prev := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = t.TempDir()
	t.Cleanup(func() { buildinfo.DevForgeRoot = prev })

	writeDevForgeGoWork(dir)

	if _, err := os.Stat(filepath.Join(dir, "go.work")); !os.IsNotExist(err) {
		t.Errorf("expected no go.work to be created, stat err = %v", err)
	}
}
