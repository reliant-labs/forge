package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// TestAuditCodegen_DisownedFiles pins the machine-readable disown
// surface in `forge audit --json`:
//
//   - disowned entries (and legacy `forked: true` ones) show up under
//     codegen.details.disowned_files with path / since;
//   - the legacy forked_files key still exists but is ALWAYS an empty
//     array, with a deprecation note field (additive-extension
//     contract: keys are never repurposed);
//   - the summary counts disowned files but the status stays OK —
//     disowning is a legitimate end state, not a warning;
//   - disowned files are excluded from user_edited_gen_files even when
//     their content drifted after the disown.
func TestAuditCodegen_DisownedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Disowned wire_gen.go whose content drifted AFTER the disown — must
	// not surface as "modified" (the user owns it outright).
	content := []byte("package app // edited after disown\n")
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
      "hash": "` + checksums.Hash([]byte("content at disown time")) + `",
      "tier": 2,
      "disowned": true,
      "disowned_at": "2026-06-01T00:00:00Z"
    },
    "pkg/app/bootstrap.go": {
      "hash": "deadbeef",
      "tier": 1,
      "forked": true,
      "forked_at": "2026-01-01T00:00:00Z"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), []byte(csBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditCodegen(nil, dir)
	if cat.Status != AuditStatusOK {
		t.Errorf("status = %s, want ok — disowned files are a legitimate end state (summary: %s)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "2 disowned") {
		t.Errorf("summary %q missing disowned count", cat.Summary)
	}

	disowned, ok := cat.Details["disowned_files"].([]auditDisownedFile)
	if !ok {
		t.Fatalf("disowned_files detail missing or wrong shape: %#v", cat.Details["disowned_files"])
	}
	if len(disowned) != 2 {
		t.Fatalf("disowned_files = %+v, want both the disowned and the legacy forked entry", disowned)
	}
	if disowned[0].Path != "pkg/app/bootstrap.go" || disowned[0].Since != "2026-01-01T00:00:00Z" {
		t.Errorf("disowned_files[0] = %+v (legacy fork must inherit forked_at as since)", disowned[0])
	}
	if disowned[1].Path != "pkg/app/wire_gen.go" || disowned[1].Since != "2026-06-01T00:00:00Z" {
		t.Errorf("disowned_files[1] = %+v", disowned[1])
	}
	if _, ok := cat.Details["disowned_hint"]; !ok {
		t.Errorf("disowned_hint detail missing")
	}

	// Legacy key contract: always present, always empty, with a note.
	legacy, ok := cat.Details["forked_files"].([]auditDisownedFile)
	if !ok {
		t.Fatalf("legacy forked_files key missing or wrong shape: %#v", cat.Details["forked_files"])
	}
	if len(legacy) != 0 {
		t.Errorf("legacy forked_files must always be empty; got %+v", legacy)
	}
	note, _ := cat.Details["forked_files_note"].(string)
	if !strings.Contains(note, "deprecated") || !strings.Contains(note, "disowned_files") {
		t.Errorf("forked_files_note = %q, want a deprecation note pointing at disowned_files", note)
	}

	// Post-disown edits are not "drift".
	if mod, ok := cat.Details["user_edited_gen_files"]; ok {
		t.Errorf("disowned file surfaced as user-edited drift: %v", mod)
	}
}

// TestAuditCodegen_NoDisowned pins the empty-state shape: the legacy
// forked_files key + note are still emitted (consumers can rely on
// them unconditionally for the deprecation window) and no disowned
// keys appear.
func TestAuditCodegen_NoDisowned(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	csBody := `{"forge_version": "test", "files": {}}`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), []byte(csBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditCodegen(nil, dir)
	if cat.Status != AuditStatusOK {
		t.Errorf("status = %s, want ok", cat.Status)
	}
	legacy, ok := cat.Details["forked_files"].([]auditDisownedFile)
	if !ok || len(legacy) != 0 {
		t.Errorf("legacy forked_files must be an empty array even with no disowned files; got %#v", cat.Details["forked_files"])
	}
	if _, ok := cat.Details["disowned_files"]; ok {
		t.Errorf("disowned_files should be omitted when nothing is disowned")
	}
}
