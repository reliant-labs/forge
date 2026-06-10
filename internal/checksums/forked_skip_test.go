// File: internal/checksums/forked_skip_test.go
//
// Tests for the forked-skip aggregation. The regression these tests
// pin: when an entry in `.forge/checksums.json` carries `Forked: true`,
// every WriteGeneratedFile* call against that path is dropped — the
// rendered content never reaches disk (it is parked as a side render
// instead). Before this fix, the silent drop made it look like the
// codegen had run but done nothing; downstream agents (cp-forge
// wire_gen.go case) hand-rolled workarounds for what was actually a
// one-flag-flip away.
//
// The aggregation surface (NoteForkedSkip / ForkedSkipsThisRun) is what
// the CLI pipeline reads to print the loud-once fork report naming
// every skipped path + the `forge unfork --merge` command. These tests
// pin the recording side; the CLI report is covered under internal/cli.
package checksums

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWriteGeneratedFile_ForkedSkipRecorded(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetSkipWrite()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"pkg/app/wire_gen.go": {
			Hash:    "deadbeef",
			History: []string{"deadbeef"},
			Tier:    1,
			Forked:  true,
		},
	}}

	wrote, err := WriteGeneratedFile(root, "pkg/app/wire_gen.go",
		[]byte("// freshly rendered content\npackage app\n"), cs, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wrote {
		t.Errorf("wrote = true; want false (forked entry must be skipped)")
	}
	// File must not have been written even with force=true — that's the
	// "forks survive --force" contract from the unfork docstring.
	if _, err := os.Stat(filepath.Join(root, "pkg/app/wire_gen.go")); !os.IsNotExist(err) {
		t.Errorf("file written despite forked-skip: err=%v", err)
	}
	// The aggregate must record the path so the CLI report can name it.
	want := []string{"pkg/app/wire_gen.go"}
	if got := ForkedSkipsThisRun(); !reflect.DeepEqual(got, want) {
		t.Errorf("ForkedSkipsThisRun() = %v, want %v", got, want)
	}
}

func TestWriteGeneratedFile_MultiplePathsAggregate(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetSkipWrite()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"a.go": {Hash: "h1", Tier: 1, Forked: true},
		"b.go": {Hash: "h2", Tier: 1, Forked: true},
		"c.go": {Hash: "h3", Tier: 1, Forked: false}, // not forked → normal write
	}}

	for _, p := range []string{"a.go", "b.go", "c.go"} {
		if _, err := WriteGeneratedFile(root, p, []byte("package x\n"), cs, false); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	// a.go and b.go were skipped; c.go went through normally.
	// ForkedSkipsThisRun returns sorted output.
	want := []string{"a.go", "b.go"}
	if got := ForkedSkipsThisRun(); !reflect.DeepEqual(got, want) {
		t.Errorf("ForkedSkipsThisRun() = %v, want %v", got, want)
	}
	// c.go should be on disk because it wasn't forked.
	if _, err := os.Stat(filepath.Join(root, "c.go")); err != nil {
		t.Errorf("c.go missing: %v (non-forked entry should have been written)", err)
	}
}

func TestWriteGeneratedFile_RepeatedSkipsDeduped(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetSkipWrite()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"a.go": {Hash: "h", Tier: 1, Forked: true},
	}}
	// Same path written 3 times (e.g. multiple emit steps target the
	// same checksum entry). NoteForkedSkip dedupes at the recording
	// layer so the loud-once report never names a path twice.
	for i := 0; i < 3; i++ {
		if _, err := WriteGeneratedFile(root, "a.go", []byte("x"), cs, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := ForkedSkipsThisRun(); len(got) != 1 {
		t.Errorf("ForkedSkipsThisRun() = %v, want exactly one entry (recording layer dedupes)", got)
	}
}

func TestWriteGeneratedFileTier2_ForkedSkipRecorded(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	ResetTier2State()
	defer ResetSkipWrite()
	defer ResetPerRunState()
	defer ResetTier2State()

	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"middleware.go": {Hash: "h", Tier: 2, Forked: true},
	}}
	if _, err := WriteGeneratedFileTier2(root, "middleware.go", []byte("package m"), cs, false); err != nil {
		t.Fatal(err)
	}
	if !contains(ForkedSkipsThisRun(), "middleware.go") {
		t.Errorf("Tier-2 forked-skip not recorded: %v", ForkedSkipsThisRun())
	}
}

func TestResetPerRunState_ClearsForkedAggregate(t *testing.T) {
	NoteForkedSkip("a")
	NoteForkedSkip("b")
	ResetPerRunState()
	if got := ForkedSkipsThisRun(); len(got) != 0 {
		t.Errorf("ResetPerRunState() did not clear the forked-skip set: %v", got)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
