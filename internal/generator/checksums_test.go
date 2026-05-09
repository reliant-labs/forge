package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHashContent(t *testing.T) {
	h1 := HashContent([]byte("hello"))
	h2 := HashContent([]byte("hello"))
	h3 := HashContent([]byte("world"))

	if h1 != h2 {
		t.Errorf("same content should produce same hash")
	}
	if h1 == h3 {
		t.Errorf("different content should produce different hash")
	}
	if len(h1) != 64 {
		t.Errorf("sha256 hex digest should be 64 chars, got %d", len(h1))
	}
}

func TestFileChecksums_RecordAndIsModified(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	content := []byte("generated content")

	// Write the file to disk
	relPath := "test/file.go"
	fullPath := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Record the checksum
	cs.RecordFile(relPath, content)

	// File should not be modified (content matches)
	if cs.IsFileModified(root, relPath) {
		t.Errorf("file should not be modified when content matches checksum")
	}

	// Modify the file on disk
	if err := os.WriteFile(fullPath, []byte("user modified"), 0644); err != nil {
		t.Fatal(err)
	}

	// Now it should be detected as modified
	if !cs.IsFileModified(root, relPath) {
		t.Errorf("file should be detected as modified after content change")
	}
}

func TestFileChecksums_IsModified_UntrackedFile(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	// Untracked file should not be considered modified
	if cs.IsFileModified(root, "nonexistent.go") {
		t.Errorf("untracked file should not be considered modified")
	}
}

func TestFileChecksums_IsModified_DeletedFile(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	// Record a checksum for a file that doesn't exist on disk
	h := HashContent([]byte("old content"))
	cs.Files["deleted.go"] = FileChecksumEntry{Hash: h, History: []string{h}}

	// Deleted file should not be considered modified
	if cs.IsFileModified(root, "deleted.go") {
		t.Errorf("deleted file should not be considered modified")
	}
}

func TestLoadSaveChecksums(t *testing.T) {
	root := t.TempDir()

	// Loading from nonexistent file should return empty checksums
	cs, err := LoadChecksums(root)
	if err != nil {
		t.Fatalf("LoadChecksums from empty dir: %v", err)
	}
	if len(cs.Files) != 0 {
		t.Errorf("expected empty files map, got %d entries", len(cs.Files))
	}

	// Save checksums
	cs.ForgeVersion = "1.0.0"
	cs.RecordFile("a.go", []byte("aaa"))
	cs.RecordFile("b.go", []byte("bbb"))

	if err := SaveChecksums(root, cs); err != nil {
		t.Fatalf("SaveChecksums: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(filepath.Join(root, checksumFile)); err != nil {
		t.Fatalf("checksum file should exist: %v", err)
	}

	// Load back and verify
	cs2, err := LoadChecksums(root)
	if err != nil {
		t.Fatalf("LoadChecksums: %v", err)
	}
	if cs2.ForgeVersion != "1.0.0" {
		t.Errorf("ForgeVersion: got %q, want %q", cs2.ForgeVersion, "1.0.0")
	}
	if len(cs2.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(cs2.Files))
	}
	if cs2.Files["a.go"].Hash != cs.Files["a.go"].Hash {
		t.Errorf("checksum for a.go doesn't match")
	}
}

func TestWriteGeneratedFile(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	content := []byte("// generated code\npackage main\n")

	// First write should succeed
	written, err := WriteGeneratedFile(root, "main.go", content, cs, false)
	if err != nil {
		t.Fatalf("WriteGeneratedFile: %v", err)
	}
	if !written {
		t.Errorf("first write should succeed")
	}

	// Verify file exists with correct content
	got, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("file content mismatch")
	}

	// Second write with same generator should succeed (not modified)
	written, err = WriteGeneratedFile(root, "main.go", []byte("// updated\n"), cs, false)
	if err != nil {
		t.Fatalf("WriteGeneratedFile (update): %v", err)
	}
	if !written {
		t.Errorf("re-write of unmodified file should succeed")
	}

	// Now simulate user modification
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("// user edit\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Write should be skipped (user modified)
	written, err = WriteGeneratedFile(root, "main.go", []byte("// new gen\n"), cs, false)
	if err != nil {
		t.Fatalf("WriteGeneratedFile (skip): %v", err)
	}
	if written {
		t.Errorf("write should be skipped when file is user-modified")
	}

	// Force write should override
	written, err = WriteGeneratedFile(root, "main.go", []byte("// forced\n"), cs, true)
	if err != nil {
		t.Fatalf("WriteGeneratedFile (force): %v", err)
	}
	if !written {
		t.Errorf("force write should succeed even when file is user-modified")
	}
}

// TestRecordFile_TracksHistory exercises the prior-render history that
// closes the stale-codegen-vs-user-modified gap. Each successive RecordFile
// call appends to History; existing entries that are re-recorded are
// deduped (existing prior occurrence dropped, new entry appended at the
// tail) so identical re-renders don't churn the list.
func TestRecordFile_TracksHistory(t *testing.T) {
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	rel := "x.go"

	cs.RecordFile(rel, []byte("v1"))
	if got := cs.Files[rel].History; len(got) != 1 || got[0] != HashContent([]byte("v1")) {
		t.Fatalf("after first record, history = %v", got)
	}

	cs.RecordFile(rel, []byte("v2"))
	hist := cs.Files[rel].History
	if len(hist) != 2 || hist[0] != HashContent([]byte("v1")) || hist[1] != HashContent([]byte("v2")) {
		t.Fatalf("after second record, history = %v", hist)
	}
	if cs.Files[rel].Hash != HashContent([]byte("v2")) {
		t.Fatalf("Hash should mirror tail of history")
	}

	// Re-recording v1 should dedupe — drop prior occurrence, push to tail.
	cs.RecordFile(rel, []byte("v1"))
	hist = cs.Files[rel].History
	if len(hist) != 2 || hist[0] != HashContent([]byte("v2")) || hist[1] != HashContent([]byte("v1")) {
		t.Fatalf("after re-record of v1, history = %v", hist)
	}
}

// TestRecordFile_HistoryBound verifies the history list stays bounded to
// testHistoryLimit entries — long-running projects re-rendering many template
// updates shouldn't grow .forge/checksums.json without bound.
func TestRecordFile_HistoryBound(t *testing.T) {
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	rel := "y.go"

	for i := 0; i < testHistoryLimit*2; i++ {
		cs.RecordFile(rel, []byte{byte(i)})
	}
	hist := cs.Files[rel].History
	if len(hist) != testHistoryLimit {
		t.Fatalf("history len = %d, want %d", len(hist), testHistoryLimit)
	}
	// Tail must be the most recent render.
	if hist[len(hist)-1] != HashContent([]byte{byte(testHistoryLimit*2 - 1)}) {
		t.Errorf("tail of history is not the most recent render")
	}
}

// TestIsFileModified_MatchesPriorRender — the headline behaviour. A
// template update produces a v2 render; the on-disk file is still the v1
// render (stale codegen). Forge must NOT treat the file as user-modified
// because the v1 render is in history.
func TestIsFileModified_MatchesPriorRender(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}
	rel := "stale.go"

	// Initial render: v1 written, recorded.
	v1 := []byte("// rendered v1\n")
	if err := os.WriteFile(filepath.Join(root, rel), v1, 0644); err != nil {
		t.Fatal(err)
	}
	cs.RecordFile(rel, v1)

	// Template update: forge now renders v2 and re-records the new bytes.
	// (We simulate the "next render"; on-disk file stays at v1 because
	// upgrade hasn't run yet.)
	v2 := []byte("// rendered v2\n")
	cs.RecordFile(rel, v2)

	// The on-disk content (v1) is now stale codegen — NOT user-modified.
	if cs.IsFileModified(root, rel) {
		t.Errorf("stale codegen (matches prior render) should not be flagged as modified")
	}
	if !cs.MatchesAnyKnownRender(rel, v1) {
		t.Errorf("MatchesAnyKnownRender should accept a prior-render content")
	}
	if !cs.MatchesAnyKnownRender(rel, v2) {
		t.Errorf("MatchesAnyKnownRender should accept the current-render content")
	}

	// A real user edit should still be detected.
	user := []byte("// user wrote this\n")
	if err := os.WriteFile(filepath.Join(root, rel), user, 0644); err != nil {
		t.Fatal(err)
	}
	if !cs.IsFileModified(root, rel) {
		t.Errorf("genuine user edit should still be flagged as modified")
	}
	if cs.MatchesAnyKnownRender(rel, user) {
		t.Errorf("MatchesAnyKnownRender should reject content forge has never rendered")
	}
}

// TestLoadChecksums_LegacyFlatShape verifies backwards-compat with the
// pre-history checksum file format (files: path -> hex string). Loading
// such a file promotes each entry into a FileChecksumEntry with a
// 1-element history seeded from the legacy hash.
func TestLoadChecksums_LegacyFlatShape(t *testing.T) {
	root := t.TempDir()

	// Hand-craft a legacy-shape checksums.json.
	legacy := map[string]any{
		"forge_version": "0.5.0",
		"files": map[string]string{
			"a.go": HashContent([]byte("aaa")),
			"b.go": HashContent([]byte("bbb")),
		},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".forge")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Load — should promote each legacy entry into a FileChecksumEntry.
	cs, err := LoadChecksums(root)
	if err != nil {
		t.Fatalf("LoadChecksums: %v", err)
	}
	entry, ok := cs.Files["a.go"]
	if !ok {
		t.Fatalf("a.go missing after legacy load")
	}
	if entry.Hash != HashContent([]byte("aaa")) {
		t.Errorf("a.go hash mismatch: %s", entry.Hash)
	}
	if len(entry.History) != 1 || entry.History[0] != entry.Hash {
		t.Errorf("a.go history should be seeded with current hash, got %v", entry.History)
	}

	// Round-trip: save → load preserves both fields.
	if err := SaveChecksums(root, cs); err != nil {
		t.Fatal(err)
	}
	cs2, err := LoadChecksums(root)
	if err != nil {
		t.Fatal(err)
	}
	if cs2.Files["a.go"].Hash != cs.Files["a.go"].Hash {
		t.Errorf("hash didn't round-trip")
	}
	if len(cs2.Files["a.go"].History) != 1 {
		t.Errorf("history didn't round-trip: %v", cs2.Files["a.go"].History)
	}
}

// TestWriteGeneratedFile_CreatesSubdirectories verifies that nested
// destination paths get their parent directories created.
func TestWriteGeneratedFile_CreatesSubdirectories(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	written, err := WriteGeneratedFile(root, "deep/nested/dir/file.go", []byte("pkg"), cs, false)
	if err != nil {
		t.Fatalf("WriteGeneratedFile: %v", err)
	}
	if !written {
		t.Errorf("write should succeed")
	}

	got, err := os.ReadFile(filepath.Join(root, "deep/nested/dir/file.go"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if string(got) != "pkg" {
		t.Errorf("content mismatch")
	}
}
