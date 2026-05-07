package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestORMSyncLint_NoGenDir verifies the lint is a no-op when the project
// has no gen/db/v1/ tree (CLI projects, projects without entities).
func TestORMSyncLint_NoGenDir(t *testing.T) {
	root := t.TempDir()
	if err := runORMSyncLint(root); err != nil {
		t.Errorf("expected nil for missing gen/db/v1, got %v", err)
	}
}

// TestORMSyncLint_PbGoOnly verifies the warning case: a .pb.go with no
// .pb.orm.go siblings.
func TestORMSyncLint_PbGoOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gen", "db", "v1")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stripe_entities.pb.go"), []byte("package v1"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Just make sure it doesn't error out — output goes to stdout.
	if err := runORMSyncLint(root); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestORMSyncLint_BothPresentInSync verifies no findings when both files
// exist and ORM is at least as fresh as the proto stub.
func TestORMSyncLint_BothPresentInSync(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gen", "db", "v1")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	pbPath := filepath.Join(dir, "stripe_entities.pb.go")
	ormPath := filepath.Join(dir, "stripe_entities_customer.pb.orm.go")
	if err := os.WriteFile(pbPath, []byte("package v1"), 0644); err != nil {
		t.Fatalf("write pb: %v", err)
	}
	if err := os.WriteFile(ormPath, []byte("package v1"), 0644); err != nil {
		t.Fatalf("write orm: %v", err)
	}
	// Bump ORM mtime to be newer than pb.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ormPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := runORMSyncLint(root); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
