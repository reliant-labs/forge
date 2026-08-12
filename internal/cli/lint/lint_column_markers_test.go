package lint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func writeColumnMarkersFixture(t *testing.T, name, sql string) string {
	t.Helper()
	dir := t.TempDir()
	migDir := filepath.Join(dir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, name), []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	return migDir
}

func TestCollectColumnMarkerFindings_FlagsUnknownMarker(t *testing.T) {
	dir := writeColumnMarkersFixture(t, "0001_immutable.up.sql", `
COMMENT ON COLUMN invoices.total_cents IS 'forge:immutible';
`)
	findings, err := collectColumnMarkerFindings(dir)
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Marker != "forge:immutible" {
		t.Errorf("marker = %q, want forge:immutible", f.Marker)
	}
	if f.Object != "invoices.total_cents" {
		t.Errorf("object = %q, want invoices.total_cents", f.Object)
	}
	if f.Line != 2 {
		t.Errorf("line = %d, want 2", f.Line)
	}
}

func TestCollectColumnMarkerFindings_KnownMarkersAreClean(t *testing.T) {
	dir := writeColumnMarkersFixture(t, "0001_known.up.sql", `
COMMENT ON COLUMN invoices.total_cents IS 'forge:immutable';
COMMENT ON CONSTRAINT orders_patient_id_fkey ON orders IS 'forge:ref derived-from=prescription_id';
`)
	findings, err := collectColumnMarkerFindings(dir)
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %+v", findings)
	}
}

func TestCollectColumnMarkerFindings_IgnoresProseWithoutForgeToken(t *testing.T) {
	dir := writeColumnMarkersFixture(t, "0001_prose.up.sql", `
COMMENT ON COLUMN invoices.notes IS 'free-text notes field, nothing special';
`)
	findings, err := collectColumnMarkerFindings(dir)
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings for a plain comment, got %+v", findings)
	}
}

func TestCollectColumnMarkerFindings_RejectsPrefixCollision(t *testing.T) {
	// forge:refactor must NOT be silently accepted as forge:ref — exact
	// token match only.
	dir := writeColumnMarkersFixture(t, "0001_prefix.up.sql", `
COMMENT ON CONSTRAINT orders_patient_id_fkey ON orders IS 'forge:refactor later';
`)
	findings, err := collectColumnMarkerFindings(dir)
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for forge:refactor, got %d: %+v", len(findings), findings)
	}
	if findings[0].Marker != "forge:refactor" {
		t.Errorf("marker = %q, want forge:refactor", findings[0].Marker)
	}
}

// A marker whose argument attaches with `=` arrives as ONE token
// (forge:fill=handler), so the registry comparison has to look at the part
// before the `=` too. forge:ref takes its argument after a space and so never
// exercised this; the gap appeared the moment a `=`-style marker existed, and
// showed up as a real project being warned about a marker forge had just
// shipped. Both spellings are pinned: alone, and alongside another marker.
func TestCollectColumnMarkerFindings_ValuedMarkerIsRecognized(t *testing.T) {
	dir := writeColumnMarkersFixture(t, "0001_valued.up.sql", `
COMMENT ON COLUMN invoices.number IS 'forge:fill=handler';
COMMENT ON COLUMN invoices.company_id IS 'forge:immutable forge:fill=handler';
COMMENT ON COLUMN invoices.token IS 'forge:fill=ulid';
`)
	findings, err := collectColumnMarkerFindings(dir)
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("forge:fill=<strategy> is a known marker, got findings: %+v", findings)
	}
}

// Splitting on `=` must not weaken the exact-match guarantee: a longer marker
// that merely STARTS with a known one is still unknown, however it is spelled.
func TestCollectColumnMarkerFindings_ValuedPrefixCollisionStillRejected(t *testing.T) {
	dir := writeColumnMarkersFixture(t, "0001_valued_prefix.up.sql", `
COMMENT ON COLUMN invoices.number IS 'forge:fillmode=handler';
`)
	findings, err := collectColumnMarkerFindings(dir)
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for forge:fillmode, got %d: %+v", len(findings), findings)
	}
	if findings[0].Marker != "forge:fillmode=handler" {
		t.Errorf("marker = %q, want forge:fillmode=handler", findings[0].Marker)
	}
}

func TestCollectColumnMarkerFindings_MissingDirIsNotAnError(t *testing.T) {
	findings, err := collectColumnMarkerFindings(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("collectColumnMarkerFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %+v", findings)
	}
}

func TestColumnMarkerFixHint_ListsKnownMarkers(t *testing.T) {
	f := columnMarkerFinding{Object: "invoices.total_cents", Marker: "forge:immutible"}
	hint := columnMarkerFixHint(f)
	for _, want := range []string{"forge:immutible", "invoices.total_cents", "forge:immutable", "forge:ref", "forge project annotations --kind column"} {
		if !strings.Contains(hint, want) {
			t.Errorf("fix hint missing %q:\n%s", want, hint)
		}
	}
}

func TestFormatColumnMarkers_CleanAndDirty(t *testing.T) {
	var buf bytes.Buffer
	formatColumnMarkers(&buf, nil)
	if !strings.Contains(buf.String(), "column-markers clean") {
		t.Errorf("clean output missing success line: %s", buf.String())
	}

	buf.Reset()
	formatColumnMarkers(&buf, []columnMarkerFinding{{
		File: "db/migrations/0001_x.up.sql", Line: 3,
		Object: "invoices.total_cents", Marker: "forge:immutible",
	}})
	out := buf.String()
	for _, want := range []string{
		"[forge-column-markers] db/migrations/0001_x.up.sql:3",
		"forge:immutible",
		"warnings only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dirty output missing %q:\n%s", want, out)
		}
	}
}

func TestCollectColumnMarkersJSON_Shape(t *testing.T) {
	dir := writeColumnMarkersFixture(t, "0001_immutable.up.sql", `
COMMENT ON COLUMN invoices.total_cents IS 'forge:immutible';
`)
	cfg := &config.ProjectConfig{Database: config.DatabaseConfig{MigrationsDir: dir}}
	fs, err := collectColumnMarkersJSON(cfg)
	if err != nil {
		t.Fatalf("collectColumnMarkersJSON: %v", err)
	}
	if len(fs) != 1 {
		t.Fatalf("expected 1 JSON finding, got %d: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Rule != "forge-column-markers" {
		t.Errorf("rule = %q, want forge-column-markers", f.Rule)
	}
	if f.Severity != lintSevWarning {
		t.Errorf("severity = %q, want %q (advisory linter)", f.Severity, lintSevWarning)
	}
	if f.FixHint == "" {
		t.Error("missing fix_hint")
	}
}
