package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// TestAuditCodegen_ForkedFiles pins the machine-readable fork surface
// in `forge audit --json`: Tier-1 forked entries show up under
// codegen.details.forked_files with path / forked_at / group, the
// summary counts them, and the category status degrades to warn.
// Tier-2 forked entries (scaffold ownership transfers) are excluded.
func TestAuditCodegen_ForkedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Forked wire_gen.go content on disk matching its recorded hash —
	// forked files are not "modified", they're a distinct state.
	content := []byte("package app // fork\n")
	full := filepath.Join(dir, "pkg", "app", "wire_gen.go")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	csBody := `{
  "forge_version": "test",
  "files": {
    "pkg/app/wire_gen.go": {
      "hash": "` + checksums.Hash(content) + `",
      "history": ["` + checksums.Hash(content) + `"],
      "tier": 1,
      "forked": true,
      "forked_at": "2026-06-01T00:00:00Z",
      "group": "app-wiring"
    },
    "handlers/echo/handlers_crud_gen_test.go": {
      "hash": "deadbeef",
      "tier": 2,
      "forked": true
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), []byte(csBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditCodegen(nil, dir)
	if cat.Status != AuditStatusWarn {
		t.Errorf("status = %s, want warn (summary: %s)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "1 forked") {
		t.Errorf("summary %q missing forked count", cat.Summary)
	}
	forked, ok := cat.Details["forked_files"].([]auditForkedFile)
	if !ok {
		t.Fatalf("forked_files detail missing or wrong shape: %#v", cat.Details["forked_files"])
	}
	if len(forked) != 1 {
		t.Fatalf("forked_files = %+v, want exactly the Tier-1 fork", forked)
	}
	got := forked[0]
	if got.Path != "pkg/app/wire_gen.go" || got.ForkedAt != "2026-06-01T00:00:00Z" || got.Group != "app-wiring" {
		t.Errorf("forked_files[0] = %+v", got)
	}
	if _, ok := cat.Details["forked_hint"]; !ok {
		t.Errorf("forked_hint detail missing")
	}
}
