package generator

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/reliant-labs/forge/internal/atomicfile"
)

func TestScaffoldProjectInstanceIdentity(t *testing.T) {
	t.Parallel()
	a, b := t.TempDir(), t.TempDir()
	first, err := scaffoldProjectInstanceID(a)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := uuid.Parse(first); err != nil || id == uuid.Nil {
		t.Fatalf("new project identity is not a UUID: %q, %v", first, err)
	}
	second, err := scaffoldProjectInstanceID(b)
	if err != nil || first == second {
		t.Fatalf("independent projects collided: %v", err)
	}
	for _, dir := range []string{a, b} {
		if err := atomicfile.WriteFile(filepath.Join(dir, "forge.yaml"), []byte("name: counter\nproject_instance_id: "+first+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		id, err := scaffoldProjectInstanceID(dir)
		if err != nil || id != first {
			t.Fatalf("resume/clone changed identity: %q, %v", id, err)
		}
	}
	if err := atomicfile.WriteFile(filepath.Join(a, "forge.yaml"), []byte("name: counter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id, err := scaffoldProjectInstanceID(a); err != nil || id != "" {
		t.Fatalf("legacy project must keep legacy identity: %q, %v", id, err)
	}
}
