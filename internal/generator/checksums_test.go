package generator

import (
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
	cs := &FileChecksums{Files: make(map[string]string)}
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
	cs := &FileChecksums{Files: make(map[string]string)}

	// Untracked file should not be considered modified
	if cs.IsFileModified(root, "nonexistent.go") {
		t.Errorf("untracked file should not be considered modified")
	}
}

func TestFileChecksums_IsModified_DeletedFile(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]string)}

	// Record a checksum for a file that doesn't exist on disk
	cs.Files["deleted.go"] = HashContent([]byte("old content"))

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
	cs.Files["a.go"] = HashContent([]byte("aaa"))
	cs.Files["b.go"] = HashContent([]byte("bbb"))

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
	if cs2.Files["a.go"] != cs.Files["a.go"] {
		t.Errorf("checksum for a.go doesn't match")
	}
}

func TestWriteGeneratedFile(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]string)}

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

func TestWriteGeneratedFile_CreatesSubdirectories(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]string)}

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
