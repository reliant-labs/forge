package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// Disown lifecycle contract (the two-state model):
//
//   - DisownPaths flips a Tier-1 entry to Tier-2 + Disowned, recording
//     the on-disk (user) content as the entry hash.
//   - While the file exists, NO WriteGeneratedFile* call touches it —
//     not even force=true.
//   - Re-adoption is by deletion: with the file gone, the next Tier-1
//     write re-emits and the entry returns to Tier-1, marker cleared.

func writeDisownFixture(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDisownPaths_FlipsEntryToUserOwned(t *testing.T) {
	orig := nowRFC3339
	nowRFC3339 = func() string { return "2026-06-10T00:00:00Z" }
	defer func() { nowRFC3339 = orig }()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	const rel = "pkg/app/wire_gen.go"

	// Pristine Tier-1 render, then a user hand-edit.
	pristine := []byte("package app // pristine\n")
	if _, err := WriteGeneratedFileTier1(root, rel, pristine, cs, false); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("package app // user edit\n")
	writeDisownFixture(t, root, rel, userContent)

	// Legacy flags present (simulating an old fork) must be cleared.
	entry := cs.Files[rel]
	entry.Forked = true
	entry.Accepted = true
	entry.ForkedAt = "2026-01-01T00:00:00Z"
	cs.Files[rel] = entry

	if err := cs.DisownPaths(root, []string{rel}); err != nil {
		t.Fatalf("DisownPaths: %v", err)
	}
	entry = cs.Files[rel]
	if entry.Tier != 2 || !entry.Disowned {
		t.Errorf("entry = %+v, want Tier=2 Disowned=true", entry)
	}
	if entry.DisownedAt != "2026-06-10T00:00:00Z" {
		t.Errorf("DisownedAt = %q, want pinned timestamp", entry.DisownedAt)
	}
	if entry.Forked || entry.Accepted || entry.ForkedAt != "" {
		t.Errorf("legacy fork-era flags not cleared: %+v", entry)
	}
	if entry.Hash != Hash(userContent) {
		t.Errorf("hash = %q, want hash of the user's content at disown time", entry.Hash)
	}
}

func TestDisownPaths_MissingFileErrors(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"gone.go": {Hash: "abc", Tier: 1},
	}}
	if err := cs.DisownPaths(root, []string{"gone.go"}); err == nil {
		t.Fatalf("DisownPaths on a missing file must error (the on-disk content IS the thing being disowned)")
	}
}

func TestWriteGeneratedFile_DisownedSkipsEvenWithForce(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	const rel = "pkg/app/wire_gen.go"
	userContent := []byte("package app // user-owned\n")
	writeDisownFixture(t, root, rel, userContent)
	if err := cs.DisownPaths(root, []string{rel}); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		wrote, err := WriteGeneratedFile(root, rel, []byte("package app // fresh render\n"), cs, force)
		if err != nil {
			t.Fatalf("force=%v: %v", force, err)
		}
		if wrote {
			t.Errorf("force=%v: disowned file must never be written", force)
		}
	}
	// The Tier-1 wrapper takes the same skip path.
	wrote, err := WriteGeneratedFileTier1(root, rel, []byte("package app // fresh render\n"), cs, true)
	if err != nil || wrote {
		t.Errorf("Tier-1 wrapper: wrote=%v err=%v, want skip", wrote, err)
	}
	// The Tier-2 writer skips too (disowned hash is the USER's content,
	// so the modified-file check alone would not protect it).
	wrote, err = WriteGeneratedFileTier2(root, rel, []byte("package app // fresh render\n"), cs, false)
	if err != nil || wrote {
		t.Errorf("Tier-2 writer: wrote=%v err=%v, want skip", wrote, err)
	}

	onDisk, _ := os.ReadFile(filepath.Join(root, rel))
	if string(onDisk) != string(userContent) {
		t.Errorf("disowned file modified: %q", onDisk)
	}
	// No side renders are parked for disowned paths — there is no
	// reconcile-later state.
	if _, err := os.Stat(filepath.Join(root, RenderDir, rel)); !os.IsNotExist(err) {
		t.Errorf(".forge/render side render must not be parked for disowned paths")
	}
}

func TestWriteGeneratedFile_DeletedDisownedFileIsReAdopted(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	const rel = "pkg/app/wire_gen.go"
	writeDisownFixture(t, root, rel, []byte("package app // user-owned\n"))
	if err := cs.DisownPaths(root, []string{rel}); err != nil {
		t.Fatal(err)
	}

	// The documented re-adoption path: delete the file, regenerate.
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}
	fresh := []byte("package app // pristine render\n")
	wrote, err := WriteGeneratedFileTier1(root, rel, fresh, cs, false)
	if err != nil {
		t.Fatalf("re-adopting write: %v", err)
	}
	if !wrote {
		t.Fatalf("deleted disowned file must be re-emitted")
	}
	onDisk, _ := os.ReadFile(filepath.Join(root, rel))
	if string(onDisk) != string(fresh) {
		t.Errorf("on-disk = %q, want the pristine render", onDisk)
	}
	entry := cs.Files[rel]
	if entry.Tier != 1 || entry.Disowned || entry.DisownedAt != "" {
		t.Errorf("entry after re-adoption = %+v, want Tier=1 with the disowned marker cleared", entry)
	}
	if entry.Hash != Hash(fresh) {
		t.Errorf("hash = %q, want hash of the fresh render", entry.Hash)
	}
}

func TestCheckTier1Drift_IgnoresDisowned(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	const rel = "pkg/app/wire_gen.go"
	writeDisownFixture(t, root, rel, []byte("package app // user-owned\n"))
	if err := cs.DisownPaths(root, []string{rel}); err != nil {
		t.Fatal(err)
	}
	// Edit AFTER disowning — drift from the recorded hash.
	writeDisownFixture(t, root, rel, []byte("package app // edited again\n"))

	if drift := cs.CheckTier1Drift(root); len(drift) != 0 {
		t.Errorf("disowned file must never trip the Tier-1 stomp guard; got %+v", drift)
	}
}

func TestWriteGeneratedFileTier2_ReScaffoldClearsDisownedMarker(t *testing.T) {
	ResetSkipWrite()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	const rel = "internal/svc/service.go"
	writeDisownFixture(t, root, rel, []byte("package svc // user\n"))
	if err := cs.DisownPaths(root, []string{rel}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}

	scaffold := []byte("package svc // scaffold\n")
	wrote, err := WriteGeneratedFileTier2(root, rel, scaffold, cs, false)
	if err != nil || !wrote {
		t.Fatalf("Tier-2 re-scaffold of a deleted disowned file: wrote=%v err=%v", wrote, err)
	}
	entry := cs.Files[rel]
	if entry.Tier != 2 || entry.Disowned || entry.DisownedAt != "" {
		t.Errorf("entry = %+v, want plain Tier-2 starter (marker cleared)", entry)
	}
}
