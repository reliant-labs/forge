package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

// forkEntry seeds cs with a forked Tier-1 entry whose recorded content
// is `content` (mirrors what AcceptTier1Drift leaves behind).
func forkEntry(t *testing.T, root string, cs *FileChecksums, relPath string, content []byte) {
	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	cs.RecordFile(relPath, content)
	entry := cs.Files[relPath]
	entry.Tier = 1
	entry.Forked = true
	cs.Files[relPath] = entry
}

// TestWriteGeneratedFile_ForkedSkipIsTracked pins the item-2 contract:
// a forked-skip is never silent — it lands in ForkedSkipsThisRun so the
// pipeline can report it loudly at exit.
func TestWriteGeneratedFile_ForkedSkipIsTracked(t *testing.T) {
	tests := []struct {
		name      string
		force     bool
		wantSkips []string
	}{
		{name: "plain run skips and tracks", force: false, wantSkips: []string{"pkg/app/wire_gen.go"}},
		{name: "--force still skips and tracks", force: true, wantSkips: []string{"pkg/app/wire_gen.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetSkipWrite()
			ResetPerRunState()
			defer ResetPerRunState()

			root := t.TempDir()
			cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
			userContent := []byte("package app // user fork\n")
			forkEntry(t, root, cs, "pkg/app/wire_gen.go", userContent)

			wrote, err := WriteGeneratedFile(root, "pkg/app/wire_gen.go", []byte("package app // fresh\n"), cs, tt.force)
			if err != nil {
				t.Fatalf("WriteGeneratedFile: %v", err)
			}
			if wrote {
				t.Fatalf("forked file was overwritten (force=%v)", tt.force)
			}

			got := ForkedSkipsThisRun()
			if len(got) != len(tt.wantSkips) || (len(got) > 0 && got[0] != tt.wantSkips[0]) {
				t.Errorf("ForkedSkipsThisRun = %v, want %v", got, tt.wantSkips)
			}

			// User content untouched.
			onDisk, _ := os.ReadFile(filepath.Join(root, "pkg/app/wire_gen.go"))
			if string(onDisk) != string(userContent) {
				t.Errorf("forked content modified: %q", onDisk)
			}
		})
	}
}

// TestForkedSkipsThisRun_DedupAndReset: an emitter retrying the same
// path reports once; ResetPerRunState clears the set between runs.
func TestForkedSkipsThisRun_DedupAndReset(t *testing.T) {
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	forkEntry(t, root, cs, "b.go", []byte("b\n"))
	forkEntry(t, root, cs, "a.go", []byte("a\n"))

	for i := 0; i < 2; i++ {
		_, _ = WriteGeneratedFile(root, "b.go", []byte("fresh-b\n"), cs, false)
	}
	_, _ = WriteGeneratedFile(root, "a.go", []byte("fresh-a\n"), cs, false)

	got := ForkedSkipsThisRun()
	want := []string{"a.go", "b.go"} // sorted, deduplicated
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ForkedSkipsThisRun = %v, want %v", got, want)
	}

	ResetPerRunState()
	if len(ForkedSkipsThisRun()) != 0 {
		t.Errorf("ResetPerRunState did not clear the skip set")
	}
}

// TestAcceptTier1Drift_StampsForkedAt: forks carry a "forked since"
// timestamp for audit/doctor reporting.
func TestAcceptTier1Drift_StampsForkedAt(t *testing.T) {
	orig := nowRFC3339
	nowRFC3339 = func() string { return "2026-06-10T00:00:00Z" }
	defer func() { nowRFC3339 = orig }()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	rendered := []byte("package app // rendered\n")
	cs.RecordFile("pkg/app/bootstrap.go", rendered)
	entry := cs.Files["pkg/app/bootstrap.go"]
	entry.Tier = 1
	cs.Files["pkg/app/bootstrap.go"] = entry

	// User hand-edits, then accepts the drift.
	full := filepath.Join(root, "pkg/app/bootstrap.go")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("package app // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift := cs.CheckTier1Drift(root)
	if len(drift) != 1 {
		t.Fatalf("expected 1 drift entry, got %d", len(drift))
	}
	if err := cs.AcceptTier1Drift(root, drift); err != nil {
		t.Fatalf("AcceptTier1Drift: %v", err)
	}

	got := cs.Files["pkg/app/bootstrap.go"]
	if !got.Forked {
		t.Errorf("entry not marked Forked")
	}
	if got.ForkedAt != "2026-06-10T00:00:00Z" {
		t.Errorf("ForkedAt = %q, want pinned timestamp", got.ForkedAt)
	}
}
