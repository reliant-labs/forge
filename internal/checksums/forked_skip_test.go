// File: internal/checksums/forked_skip_test.go
//
// Tests for the SkippedForkedThisRun aggregation. The regression these
// tests pin: when an entry in `.forge/checksums.json` carries
// `Forked: true`, every WriteGeneratedFile* call against that path is
// silently dropped — the rendered content never reaches disk. Before
// this fix, the silent drop made it look like the codegen had run but
// done nothing; downstream agents (cp-forge wire_gen.go case)
// hand-rolled workarounds for what was actually a one-flag-flip away.
//
// The aggregation surface (SkippedForkedThisRun) is what the CLI
// pipeline reads to print a single end-of-run summary naming every
// skipped path + the `forge generate unfork` command to re-enable
// regeneration. These tests pin the recording side; the CLI summary is
// covered by an integration test under internal/cli.
package checksums

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestWriteGeneratedFile_ForkedSkipRecorded(t *testing.T) {
	ResetSkipWrite()
	defer ResetSkipWrite()

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
	// The aggregate must record the path so the CLI summary can name it.
	want := []string{"pkg/app/wire_gen.go"}
	if !reflect.DeepEqual(SkippedForkedThisRun, want) {
		t.Errorf("SkippedForkedThisRun = %v, want %v", SkippedForkedThisRun, want)
	}
}

func TestWriteGeneratedFile_MultiplePathsAggregate(t *testing.T) {
	ResetSkipWrite()
	defer ResetSkipWrite()

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

	// a.go and b.go were silently skipped; c.go went through normally.
	want := []string{"a.go", "b.go"}
	got := append([]string(nil), SkippedForkedThisRun...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkippedForkedThisRun = %v, want %v", got, want)
	}
	// c.go should be on disk because it wasn't forked.
	if _, err := os.Stat(filepath.Join(root, "c.go")); err != nil {
		t.Errorf("c.go missing: %v (non-forked entry should have been written)", err)
	}
}

func TestWriteGeneratedFile_RepeatedSkipsRecordedEachTime(t *testing.T) {
	ResetSkipWrite()
	defer ResetSkipWrite()

	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"a.go": {Hash: "h", Tier: 1, Forked: true},
	}}
	// Same path written 3 times (e.g. multiple emit steps target the
	// same checksum entry). Recording each call is OK — the CLI summary
	// is responsible for dedup; we don't want the lower layer guessing
	// at uniqueness.
	for i := 0; i < 3; i++ {
		if _, err := WriteGeneratedFile(root, "a.go", []byte("x"), cs, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(SkippedForkedThisRun); got != 3 {
		t.Errorf("SkippedForkedThisRun len = %d, want 3 (raw recording, CLI dedupes)", got)
	}
}

func TestWriteGeneratedFileTier2_ForkedSkipRecorded(t *testing.T) {
	ResetSkipWrite()
	ResetTier2State()
	defer ResetSkipWrite()
	defer ResetTier2State()

	root := t.TempDir()
	cs := &FileChecksums{Files: map[string]FileChecksumEntry{
		"middleware.go": {Hash: "h", Tier: 2, Forked: true},
	}}
	if _, err := WriteGeneratedFileTier2(root, "middleware.go", []byte("package m"), cs, false); err != nil {
		t.Fatal(err)
	}
	if !contains(SkippedForkedThisRun, "middleware.go") {
		t.Errorf("Tier-2 forked-skip not recorded: %v", SkippedForkedThisRun)
	}
}

func TestResetSkipWrite_ClearsForkedAggregate(t *testing.T) {
	SkippedForkedThisRun = []string{"a", "b"}
	ResetSkipWrite()
	if SkippedForkedThisRun != nil {
		t.Errorf("ResetSkipWrite() did not clear SkippedForkedThisRun: %v", SkippedForkedThisRun)
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
