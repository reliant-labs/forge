package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteGeneratedFileTier2_ForcePreservesHandEdits is the headline
// behavior change from item 15 of FORGE_BACKLOG.md. Before: --force
// would happily overwrite a hand-edited Tier-2 scaffold, silently
// nuking user code. After: --force is ignored for Tier-2 entirely;
// the user must opt-in to overwrite via --reset-tier2.
func TestWriteGeneratedFileTier2_ForcePreservesHandEdits(t *testing.T) {
	ResetTier2State()
	defer ResetTier2State()
	ResetSkipWrite()

	root := t.TempDir()
	cs := &FileChecksums{}

	// Initial Tier-2 write scaffolds the file (no marker: user-owned
	// from birth).
	scaffold := []byte("// scaffold\npackage svc\n")
	wrote, err := WriteGeneratedFileTier2(root, "svc.go", scaffold, cs, false)
	if err != nil || !wrote {
		t.Fatalf("initial Tier-2 write: wrote=%v err=%v", wrote, err)
	}
	if onDisk, _ := os.ReadFile(filepath.Join(root, "svc.go")); Verify(onDisk) != NoMarker {
		t.Fatalf("Tier-2 scaffold must carry no marker; got %q", onDisk)
	}

	// User hand-edits the file.
	handEdited := []byte("// user code\npackage svc\n\nfunc Important() {}\n")
	if err := os.WriteFile(filepath.Join(root, "svc.go"), handEdited, 0o644); err != nil {
		t.Fatal(err)
	}

	// A subsequent generate run with force=true must NOT overwrite the
	// hand-edited content. Item 15: --force is Tier-1-only.
	wrote, err = WriteGeneratedFileTier2(root, "svc.go", []byte("// new template\npackage svc\n"), cs, true)
	if err != nil {
		t.Fatalf("forced re-write: %v", err)
	}
	if wrote {
		t.Errorf("force=true overwrote a hand-edited Tier-2 file (regression: item 15 contract violated)")
	}

	got, _ := os.ReadFile(filepath.Join(root, "svc.go"))
	if string(got) != string(handEdited) {
		t.Errorf("Tier-2 hand-edits not preserved under --force; got:\n%s\nwant:\n%s", got, handEdited)
	}
	if Tier2PreservedCount != 1 {
		t.Errorf("Tier2PreservedCount = %d, want 1", Tier2PreservedCount)
	}
}

// TestWriteGeneratedFileTier2_ResetTier2YesOverwrites verifies that
// installing a Tier2OverwriteFn returning true (the shape the
// --reset-tier2 --yes hook installs) does overwrite hand-edited
// Tier-2 files.
func TestWriteGeneratedFileTier2_ResetTier2YesOverwrites(t *testing.T) {
	ResetTier2State()
	defer ResetTier2State()
	ResetSkipWrite()

	root := t.TempDir()
	cs := &FileChecksums{}

	scaffold := []byte("// v1\npackage svc\n")
	_, _ = WriteGeneratedFileTier2(root, "svc.go", scaffold, cs, false)

	// User hand-edit.
	if err := os.WriteFile(filepath.Join(root, "svc.go"), []byte("// user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --reset-tier2 --yes installs an unconditional "true" hook. The
	// hook is consulted whenever existing content differs from the
	// fresh render.
	Tier2OverwriteFn = func(string) bool { return true }

	newTemplate := []byte("// v2\npackage svc\n")
	wrote, err := WriteGeneratedFileTier2(root, "svc.go", newTemplate, cs, true)
	if err != nil {
		t.Fatalf("Tier-2 overwrite under --reset-tier2 --yes: %v", err)
	}
	if !wrote {
		t.Error("--reset-tier2 --yes hook returned true but Tier-2 write was skipped")
	}

	got, _ := os.ReadFile(filepath.Join(root, "svc.go"))
	if string(got) != string(newTemplate) {
		t.Errorf("Tier-2 not overwritten under explicit reset-tier2 hook; got:\n%s", got)
	}
}

// TestWriteGeneratedFileTier2_IdenticalContentIsNoOp: an existing file
// byte-equal to the fresh render is left alone without consulting the
// overwrite hook or bumping the preserved counter.
func TestWriteGeneratedFileTier2_IdenticalContentIsNoOp(t *testing.T) {
	ResetTier2State()
	defer ResetTier2State()
	ResetSkipWrite()

	root := t.TempDir()
	cs := &FileChecksums{}
	scaffold := []byte("// v1\npackage svc\n")
	_, _ = WriteGeneratedFileTier2(root, "svc.go", scaffold, cs, false)

	hookCalled := false
	Tier2OverwriteFn = func(string) bool { hookCalled = true; return true }

	wrote, err := WriteGeneratedFileTier2(root, "svc.go", scaffold, cs, false)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("identical Tier-2 content should be a no-op")
	}
	if hookCalled {
		t.Error("overwrite hook consulted for identical content")
	}
	if Tier2PreservedCount != 0 {
		t.Errorf("Tier2PreservedCount = %d, want 0 for identical content", Tier2PreservedCount)
	}
}

// TestWriteGeneratedFileTier1_ForceStillOverwrites verifies the
// unchanged-behavior leg of item 15: Tier-1 + --force continues to
// clobber hand-edits. Tier-1 IS "regenerated every run, DO NOT EDIT"
// — that contract didn't move.
func TestWriteGeneratedFileTier1_ForceStillOverwrites(t *testing.T) {
	ResetTier2State()
	defer ResetTier2State()
	ResetSkipWrite()
	ResetPerRunState() // unscoped force
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{}

	initial := []byte("// generated\npackage handlers\n")
	_, _ = WriteGeneratedFileTier1(root, "handlers_gen.go", initial, cs, false)

	// User hand-edit that keeps the marker (a wholesale replace would
	// drop the marker and become the untracked-overwrite path instead).
	stamped, _ := os.ReadFile(filepath.Join(root, "handlers_gen.go"))
	if err := os.WriteFile(filepath.Join(root, "handlers_gen.go"), append(stamped, []byte("// user\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without force the stomp guard skips it…
	newTemplate := []byte("// regen\npackage handlers\n")
	wrote, err := WriteGeneratedFileTier1(root, "handlers_gen.go", newTemplate, cs, false)
	if err != nil {
		t.Fatalf("Tier-1 unforced write: %v", err)
	}
	if wrote {
		t.Error("hand-edited Tier-1 file overwritten without --force")
	}

	// …--force on a Tier-1 file overwrites — unchanged behavior.
	wrote, err = WriteGeneratedFileTier1(root, "handlers_gen.go", newTemplate, cs, true)
	if err != nil {
		t.Fatalf("Tier-1 force write: %v", err)
	}
	if !wrote {
		t.Error("force=true on a hand-edited Tier-1 file did NOT overwrite (regression)")
	}
	got, _ := os.ReadFile(filepath.Join(root, "handlers_gen.go"))
	wantStamped, _ := Stamp("handlers_gen.go", newTemplate)
	if string(got) != string(wantStamped) {
		t.Errorf("Tier-1 not overwritten+stamped under --force; got:\n%s", got)
	}
}
