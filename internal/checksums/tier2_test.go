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

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	// Initial Tier-2 write records the scaffold.
	scaffold := []byte("// scaffold\npackage svc\n")
	wrote, err := WriteGeneratedFileTier2(root, "svc.go", scaffold, cs, false)
	if err != nil || !wrote {
		t.Fatalf("initial Tier-2 write: wrote=%v err=%v", wrote, err)
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

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	scaffold := []byte("// v1\npackage svc\n")
	_, _ = WriteGeneratedFileTier2(root, "svc.go", scaffold, cs, false)

	// User hand-edit.
	if err := os.WriteFile(filepath.Join(root, "svc.go"), []byte("// user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --reset-tier2 --yes installs an unconditional "true" hook.
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

// TestWriteGeneratedFileTier1_ForceStillOverwrites verifies the
// unchanged-behavior leg of item 15: Tier-1 + --force continues to
// clobber hand-edits. Tier-1 IS "regenerated every run, DO NOT EDIT"
// — that contract didn't move.
func TestWriteGeneratedFileTier1_ForceStillOverwrites(t *testing.T) {
	ResetTier2State()
	defer ResetTier2State()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	initial := []byte("// generated\npackage handlers\n")
	_, _ = WriteGeneratedFileTier1(root, "handlers_gen.go", initial, cs, false)

	// User hand-edit.
	if err := os.WriteFile(filepath.Join(root, "handlers_gen.go"), []byte("// user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --force on a Tier-1 file overwrites — unchanged behavior.
	newTemplate := []byte("// regen\npackage handlers\n")
	wrote, err := WriteGeneratedFileTier1(root, "handlers_gen.go", newTemplate, cs, true)
	if err != nil {
		t.Fatalf("Tier-1 force write: %v", err)
	}
	if !wrote {
		t.Error("force=true on a hand-edited Tier-1 file did NOT overwrite (regression)")
	}
	got, _ := os.ReadFile(filepath.Join(root, "handlers_gen.go"))
	if string(got) != string(newTemplate) {
		t.Errorf("Tier-1 not overwritten under --force; got:\n%s", got)
	}
}
