package checksums

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCoherenceGroupFor(t *testing.T) {
	tests := []struct {
		path      string
		wantGroup string
		wantOK    bool
	}{
		{"pkg/app/bootstrap.go", "app-wiring", true},
		{"pkg/app/app_gen.go", "app-wiring", true},
		{"pkg/app/wire_gen.go", "app-wiring", true},
		{"pkg/app/testing.go", "app-wiring", true},
		{"pkg/app/migrate.go", "", false},
		{"handlers/echo/authorizer_gen.go", "", false},
		{"pkg/app/nested/bootstrap.go", "", false}, // path.Match is non-recursive
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			g, ok := CoherenceGroupFor(tt.path)
			if ok != tt.wantOK || g.Name != tt.wantGroup {
				t.Errorf("CoherenceGroupFor(%q) = (%q, %v), want (%q, %v)", tt.path, g.Name, ok, tt.wantGroup, tt.wantOK)
			}
		})
	}
}

func TestCoherenceGroup_SiblingPatterns(t *testing.T) {
	g, ok := CoherenceGroupFor("pkg/app/bootstrap.go")
	if !ok {
		t.Fatal("bootstrap.go not in a group")
	}
	sibs := g.SiblingPatterns("pkg/app/bootstrap.go")
	want := map[string]bool{
		"pkg/app/app_gen.go":  true,
		"pkg/app/wire_gen.go": true,
		"pkg/app/testing.go":  true,
	}
	if len(sibs) != len(want) {
		t.Fatalf("SiblingPatterns = %v, want the 3 other members", sibs)
	}
	for _, s := range sibs {
		if !want[s] {
			t.Errorf("unexpected sibling %q", s)
		}
	}
}

// TestAcceptTier1Drift_RecordsGroup: forking a coherence-group member
// stamps the group name on the entry so audit can report it.
func TestAcceptTier1Drift_RecordsGroup(t *testing.T) {
	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	tests := []struct {
		path      string
		wantGroup string
	}{
		{"pkg/app/wire_gen.go", "app-wiring"},
		{"pkg/config/config.go", ""}, // ungrouped — no group recorded
	}
	for _, tt := range tests {
		cs.RecordFile(tt.path, []byte("rendered\n"))
		entry := cs.Files[tt.path]
		entry.Tier = 1
		cs.Files[tt.path] = entry
		full := filepath.Join(root, tt.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	drift := cs.CheckTier1Drift(root)
	if len(drift) != 2 {
		t.Fatalf("drift = %d entries, want 2", len(drift))
	}
	if err := cs.AcceptTier1Drift(root, drift); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		if got := cs.Files[tt.path].Group; got != tt.wantGroup {
			t.Errorf("Group for %s = %q, want %q", tt.path, got, tt.wantGroup)
		}
	}
}

// TestChangedRendersThisRun pins the "did the template output actually
// move?" tracking that feeds the coherence warning: a genuinely new
// render is recorded; an idempotent re-render (hash already in
// History) is not.
func TestChangedRendersThisRun(t *testing.T) {
	ResetSkipWrite()
	ResetPerRunState()
	defer ResetPerRunState()

	root := t.TempDir()
	cs := &FileChecksums{Files: make(map[string]FileChecksumEntry)}

	v1 := []byte("package app // v1\n")
	v2 := []byte("package app // v2\n")

	// First write: new path, render counts as changed.
	if _, err := WriteGeneratedFile(root, "pkg/app/app_gen.go", v1, cs, false); err != nil {
		t.Fatal(err)
	}
	// Simulate next run: idempotent re-render of v1 must NOT count.
	ResetPerRunState()
	if _, err := WriteGeneratedFile(root, "pkg/app/app_gen.go", v1, cs, false); err != nil {
		t.Fatal(err)
	}
	if got := ChangedRendersThisRun(); len(got) != 0 {
		t.Errorf("idempotent re-render tracked as changed: %v", got)
	}
	// Run after a template change: v2 counts.
	ResetPerRunState()
	if _, err := WriteGeneratedFile(root, "pkg/app/app_gen.go", v2, cs, false); err != nil {
		t.Fatal(err)
	}
	if got := ChangedRendersThisRun(); len(got) != 1 || got[0] != "pkg/app/app_gen.go" {
		t.Errorf("ChangedRendersThisRun = %v, want [pkg/app/app_gen.go]", got)
	}
}
