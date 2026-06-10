package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// Side-render contract (see render.go):
//
//   - skipping a forked Tier-1 write parks the fresh render at
//     .forge/render/<relpath> (refreshed every run)
//   - the FIRST side-render after the fork is also copied to
//     .forge/render-base/<relpath> (the merge base) and later renders
//     never overwrite it
//   - unforking removes both

func readSideRender(t *testing.T, root, relPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, RenderDir, relPath))
	if err != nil {
		t.Fatalf("side render missing: %v", err)
	}
	return string(data)
}

func readSideRenderBase(t *testing.T, root, relPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, RenderBaseDir, relPath))
	if err != nil {
		t.Fatalf("side render base missing: %v", err)
	}
	return string(data)
}

func TestWriteGeneratedFile_ForkedSkipParksSideRender(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	const rel = "pkg/app/wire_gen.go"
	userContent := []byte("package app // user fork\n")
	forkEntry(t, root, cs, rel, userContent)

	// Run 1 after the fork: render v1 — parked as both theirs and base.
	v1 := []byte("package app // render v1\n")
	if wrote, err := WriteGeneratedFile(root, rel, v1, cs, false); err != nil || wrote {
		t.Fatalf("expected forked skip (wrote=%v err=%v)", wrote, err)
	}
	if got := readSideRender(t, root, rel); got != string(v1) {
		t.Errorf("render = %q, want v1", got)
	}
	if got := readSideRenderBase(t, root, rel); got != string(v1) {
		t.Errorf("render-base = %q, want v1", got)
	}

	// Run 2: template moved to v2 — render refreshed, base untouched.
	v2 := []byte("package app // render v2\n")
	if wrote, err := WriteGeneratedFile(root, rel, v2, cs, false); err != nil || wrote {
		t.Fatalf("expected forked skip on run 2 (wrote=%v err=%v)", wrote, err)
	}
	if got := readSideRender(t, root, rel); got != string(v2) {
		t.Errorf("render = %q, want v2 (must refresh every run)", got)
	}
	if got := readSideRenderBase(t, root, rel); got != string(v1) {
		t.Errorf("render-base = %q, want v1 (merge base must never be clobbered)", got)
	}

	// User's on-disk file untouched throughout.
	onDisk, _ := os.ReadFile(filepath.Join(root, rel))
	if string(onDisk) != string(userContent) {
		t.Errorf("forked file modified: %q", onDisk)
	}
}

func TestCleanSideRenders(t *testing.T) {
	root := t.TempDir()
	const rel = "pkg/app/wire_gen.go"
	if err := WriteSideRender(root, rel, []byte("render\n")); err != nil {
		t.Fatal(err)
	}

	if err := CleanSideRenders(root, rel); err != nil {
		t.Fatalf("CleanSideRenders: %v", err)
	}
	for _, p := range []string{
		filepath.Join(root, RenderDir, rel),
		filepath.Join(root, RenderBaseDir, rel),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after clean", p)
		}
	}

	// Idempotent: cleaning a never-rendered path is not an error.
	if err := CleanSideRenders(root, "never/rendered.go"); err != nil {
		t.Errorf("CleanSideRenders on missing files: %v", err)
	}
}
