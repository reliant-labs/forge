// Tests for the rollback's other-writer guard (forge#2267).
//
// Contract pinned here: a failed run rewinds a journaled path only while it
// is still what this run's writers left there. A path another writer
// changed after forge's write — an agent editing a hand-owned file in the
// same checkout — is spared and named, never reverted; and a file under a
// captured directory is deleted only when it is the one the run created.
package checksums

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func readFileT(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The observed failure: forge rewrote a hand-owned file early in the run,
// another agent then added helpers to it, and the failed run's rewind put
// back the pre-run bytes and erased them.
func TestRestore_SparesAPathAnotherWriterChangedAfterForge(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "internal/reclaim/restore.go", "package reclaim // pre-run\n")
	seedFile(t, root, "internal/db/models_gen.go", "package db // pre-run\n")

	BeginRollbackJournal(root)
	t.Cleanup(CommitRollback)

	// Forge's writes this run: one through the chokepoint's precise note,
	// one recorded at the step's settle point.
	recordPreWrite(root, "internal/reclaim/restore.go")
	forgeBytes := []byte("package reclaim // forge's rewrite\n")
	seedFile(t, root, "internal/reclaim/restore.go", string(forgeBytes))
	notePostWrite("internal/reclaim/restore.go", forgeBytes)
	recordPreWrite(root, "internal/db/models_gen.go")
	seedFile(t, root, "internal/db/models_gen.go", "package db // forge's render\n")
	SettleJournal(root)

	// Another agent edits the hand-owned file after forge wrote it.
	const theirs = "package reclaim // forge's rewrite\n\nfunc newHelper() {}\n"
	seedFile(t, root, "internal/reclaim/restore.go", theirs)

	res := RestoreRollbackDetailed(root)
	if !reflect.DeepEqual(res.Spared, []string{"internal/reclaim/restore.go"}) {
		t.Errorf("spared = %v, want the file another writer changed", res.Spared)
	}
	if !reflect.DeepEqual(res.Restored, []string{"internal/db/models_gen.go"}) || len(res.Failed) != 0 {
		t.Errorf("restored = %v, failed = %v; want only forge's own output rewound", res.Restored, res.Failed)
	}
	if got := readFileT(t, root, "internal/reclaim/restore.go"); got != theirs {
		t.Errorf("the other writer's bytes were not kept:\n%s", got)
	}
	if got := readFileT(t, root, "internal/db/models_gen.go"); got != "package db // pre-run\n" {
		t.Errorf("forge's output was not rewound: %q", got)
	}
}

// RestoreRollbackReport keeps its contract: a spared path is one it could
// not restore, so no caller claims a restore that did not happen.
func TestRestoreRollbackReport_CountsSparedAsNotRestored(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "a.go", "package a // pre-run\n")
	BeginRollbackJournal(root)
	t.Cleanup(CommitRollback)
	recordPreWrite(root, "a.go")
	seedFile(t, root, "a.go", "package a // forge\n")
	SettleJournal(root)
	seedFile(t, root, "a.go", "package a // theirs\n")

	restored, failed := RestoreRollbackReport(root)
	if len(restored) != 0 || !reflect.DeepEqual(failed, []string{"a.go"}) {
		t.Errorf("restored = %v, failed = %v; want a.go reported as not restored", restored, failed)
	}
}

// A path the other writer put back to its pre-run bytes, or deleted when
// forge had created it, is already where the rewind would put it: restored,
// not spared.
func TestRestore_APathAlreadyAtItsPreRunStateIsNotSpared(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "a.go", "package a // pre-run\n")
	BeginRollbackJournal(root)
	t.Cleanup(CommitRollback)
	recordPreWrite(root, "a.go")
	recordPreWrite(root, "born.go")
	seedFile(t, root, "a.go", "package a // forge\n")
	seedFile(t, root, "born.go", "package a // forge created me\n")
	SettleJournal(root)
	seedFile(t, root, "a.go", "package a // pre-run\n")
	if err := os.Remove(filepath.Join(root, "born.go")); err != nil {
		t.Fatal(err)
	}

	res := RestoreRollbackDetailed(root)
	if len(res.Spared) != 0 || !reflect.DeepEqual(res.Restored, []string{"a.go", "born.go"}) {
		t.Errorf("restored = %v, spared = %v; want both restored", res.Restored, res.Spared)
	}
}

// Under a directory an external tool writes into, the rewind deletes the
// files the tool created this run and spares one another writer created or
// changed there afterwards.
func TestRestore_SparesFilesAnotherWriterLeftUnderACapturedTree(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "gen/a/v1/a.pb.go", "package a // pre-run\n")
	BeginRollbackJournal(root)
	t.Cleanup(CommitRollback)

	CaptureExternalWrites(root, "gen")
	// The tool's step: rewrite one stub, create two.
	seedFile(t, root, "gen/a/v1/a.pb.go", "package a // tool\n")
	seedFile(t, root, "gen/b/v1/b.pb.go", "package b // tool\n")
	seedFile(t, root, "gen/c/v1/c.pb.go", "package c // tool\n")
	SettleJournal(root)

	// Another agent's own buf run, after the step: a new stub, and an edit
	// to one the tool created.
	seedFile(t, root, "gen/d/v1/d.pb.go", "package d // theirs\n")
	seedFile(t, root, "gen/c/v1/c.pb.go", "package c // theirs\n")

	res := RestoreRollbackDetailed(root)
	if want := []string{"gen/c/v1/c.pb.go", "gen/d/v1/d.pb.go"}; !reflect.DeepEqual(res.Spared, want) {
		t.Errorf("spared = %v, want %v", res.Spared, want)
	}
	if want := []string{"gen/a/v1/a.pb.go", "gen/b/v1/b.pb.go"}; !reflect.DeepEqual(res.Restored, want) {
		t.Errorf("restored = %v, want %v", res.Restored, want)
	}
	if _, err := os.Stat(filepath.Join(root, "gen/b/v1/b.pb.go")); !os.IsNotExist(err) {
		t.Error("the stub the tool created this run was not deleted")
	}
	for rel, want := range map[string]string{
		"gen/a/v1/a.pb.go": "package a // pre-run\n",
		"gen/c/v1/c.pb.go": "package c // theirs\n",
		"gen/d/v1/d.pb.go": "package d // theirs\n",
	} {
		if got := readFileT(t, root, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// A caller that never settles (forge project upgrade's module step) keeps
// the old behaviour: everything written since the journal began is forge's,
// and is rewound.
func TestRestore_WithoutSettlePointsRewindsEverything(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go.mod", "module x // pre-run\n")
	BeginRollbackJournal(root)
	t.Cleanup(CommitRollback)
	CaptureExternalWrites(root, "go.mod")
	seedFile(t, root, "go.mod", "module x // tidied\n")

	res := RestoreRollbackDetailed(root)
	if len(res.Spared) != 0 || !reflect.DeepEqual(res.Restored, []string{"go.mod"}) {
		t.Errorf("restored = %v, spared = %v; want go.mod rewound", res.Restored, res.Spared)
	}
	if got := readFileT(t, root, "go.mod"); got != "module x // pre-run\n" {
		t.Errorf("go.mod = %q", got)
	}
}
