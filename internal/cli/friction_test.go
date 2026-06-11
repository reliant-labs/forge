// File: internal/cli/friction_test.go
//
// Tests for `forge friction` — the durable downstream→upstream
// generator-friction capture loop. The properties under test are the
// ones that make capture trustworthy:
//
//   - add/list/export round-trip all fields (flags, repeatable
//     --context, stdin text)
//   - the log is append-only JSONL: one self-contained object per line,
//     existing lines never rewritten
//   - reads tolerate (skip + count) malformed lines — a torn write must
//     never brick the log
//   - `forge audit` carries an additive `friction` category summary
//   - the project .gitignore template negates friction.jsonl back into
//     version control (it travels with the repo like checksums.json)
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalFrictionForgeYAML = `name: testproj
module_path: example.com/testproj
version: 0.1.0
kind: service
`

// runFriction executes `forge friction <args...>` against a fresh
// command tree, capturing stdout/stderr. stdin is optional.
func runFriction(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newFrictionCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	if stdin != "" {
		cmd.SetIn(strings.NewReader(stdin))
	}
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

func TestFrictionAddListExport_RoundTrip(t *testing.T) {
	dir := withTempProject(t, minimalFrictionForgeYAML)

	stdout, _, err := runFriction(t, "",
		"add", "wire_gen drops Deps fields added after scaffold",
		"--severity", "p1",
		"--area", "codegen",
		"--source", "fix-validate-agent",
		"--context", "pkg/app/wire_gen.go:42",
		"--context", "forge generate --steps mocks",
	)
	if err != nil {
		t.Fatalf("friction add: %v", err)
	}
	if !strings.Contains(stdout, "recorded fr-") || !strings.Contains(stdout, ".forge/friction.jsonl") {
		t.Errorf("add output should name the id and file, got: %q", stdout)
	}

	// Second entry via stdin ('-'), default severity.
	longText := "List-style RPCs should return empty lists,\nnot FailedPrecondition errors."
	if _, _, err := runFriction(t, longText, "add", "-", "--area", "api"); err != nil {
		t.Fatalf("friction add via stdin: %v", err)
	}

	// The log must be two self-contained JSON lines.
	raw, err := os.ReadFile(filepath.Join(dir, ".forge", "friction.jsonl"))
	if err != nil {
		t.Fatalf("read friction.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d:\n%s", len(lines), raw)
	}
	for i, line := range lines {
		var e FrictionEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i+1, err)
		}
	}

	// list --json round-trips every field.
	listOut, _, err := runFriction(t, "", "list", "--json")
	if err != nil {
		t.Fatalf("friction list --json: %v", err)
	}
	var entries []FrictionEntry
	if err := json.Unmarshal([]byte(listOut), &entries); err != nil {
		t.Fatalf("parse list --json: %v\n%s", err, listOut)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	first := entries[0]
	if first.Severity != "p1" || first.Area != "codegen" || first.Source != "fix-validate-agent" {
		t.Errorf("first entry fields wrong: %+v", first)
	}
	if len(first.Context) != 2 || first.Context[0] != "pkg/app/wire_gen.go:42" {
		t.Errorf("repeatable --context not round-tripped: %v", first.Context)
	}
	if !strings.HasPrefix(first.ID, "fr-") || len(first.ID) != len("fr-")+10 {
		t.Errorf("id shape wrong: %q", first.ID)
	}
	if first.RecordedAt.IsZero() || first.RecordedAt.Location() != time.UTC {
		t.Errorf("recorded_at should be a UTC timestamp, got %v", first.RecordedAt)
	}
	if first.ForgeVersion == "" {
		t.Errorf("forge_version must be stamped from buildinfo")
	}
	second := entries[1]
	if second.Severity != "note" {
		t.Errorf("default severity should be note, got %q", second.Severity)
	}
	if !strings.Contains(second.Text, "FailedPrecondition") {
		t.Errorf("stdin text not captured: %q", second.Text)
	}

	// Filters: severity, area, since.
	out, _, err := runFriction(t, "", "list", "--json", "--severity", "p1")
	if err != nil {
		t.Fatalf("list --severity: %v", err)
	}
	var filtered []FrictionEntry
	if err := json.Unmarshal([]byte(out), &filtered); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Severity != "p1" {
		t.Errorf("severity filter: want 1 p1 entry, got %v", filtered)
	}
	out, _, err = runFriction(t, "", "list", "--json", "--area", "api")
	if err != nil {
		t.Fatalf("list --area: %v", err)
	}
	filtered = nil
	if err := json.Unmarshal([]byte(out), &filtered); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Area != "api" {
		t.Errorf("area filter: want 1 api entry, got %v", filtered)
	}
	// --since with a duration covering both entries, then one excluding all.
	out, _, err = runFriction(t, "", "list", "--json", "--since", "1h")
	if err != nil {
		t.Fatalf("list --since duration: %v", err)
	}
	filtered = nil
	_ = json.Unmarshal([]byte(out), &filtered)
	if len(filtered) != 2 {
		t.Errorf("--since 1h should include both fresh entries, got %d", len(filtered))
	}
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	out, _, err = runFriction(t, "", "list", "--json", "--since", future)
	if err != nil {
		t.Fatalf("list --since RFC3339: %v", err)
	}
	filtered = nil
	_ = json.Unmarshal([]byte(out), &filtered)
	if len(filtered) != 0 {
		t.Errorf("--since future timestamp should exclude everything, got %d", len(filtered))
	}

	// Table view names the entries without requiring --json.
	tableOut, _, err := runFriction(t, "", "list")
	if err != nil {
		t.Fatalf("friction list: %v", err)
	}
	if !strings.Contains(tableOut, "SEVERITY") || !strings.Contains(tableOut, first.ID) {
		t.Errorf("table output missing header or id:\n%s", tableOut)
	}

	// export renders markdown grouped by severity then area.
	mdOut, _, err := runFriction(t, "", "export", "--format", "md")
	if err != nil {
		t.Fatalf("friction export: %v", err)
	}
	for _, want := range []string{
		"# Friction log",
		"## P1",
		"### codegen",
		"## NOTE",
		"### api",
		first.ID,
		"context: `pkg/app/wire_gen.go:42`",
	} {
		if !strings.Contains(mdOut, want) {
			t.Errorf("export missing %q:\n%s", want, mdOut)
		}
	}
	// Severity groups must come out p-first: P1 section before NOTE.
	if strings.Index(mdOut, "## P1") > strings.Index(mdOut, "## NOTE") {
		t.Errorf("export should order severities p0..note:\n%s", mdOut)
	}

	// Unsupported export format fails loudly.
	if _, _, err := runFriction(t, "", "export", "--format", "html"); err == nil {
		t.Error("export --format html should error")
	}
}

func TestFrictionAdd_RejectsBadInput(t *testing.T) {
	withTempProject(t, minimalFrictionForgeYAML)

	if _, _, err := runFriction(t, "", "add", "text", "--severity", "urgent"); err == nil {
		t.Error("invalid severity should error")
	}
	if _, _, err := runFriction(t, "   \n", "add", "-"); err == nil {
		t.Error("blank stdin text should error")
	}
	// Nothing may have been written by the failed attempts.
	if _, err := os.Stat(filepath.Join(".forge", "friction.jsonl")); !os.IsNotExist(err) {
		t.Error("failed adds must not create the log")
	}
}

// TestFrictionList_ToleratesMalformedLines pins the torn-write
// contract: garbage lines are skipped and counted, never fatal, and
// valid lines on either side still load.
func TestFrictionList_ToleratesMalformedLines(t *testing.T) {
	dir := withTempProject(t, minimalFrictionForgeYAML)

	if _, _, err := runFriction(t, "", "add", "entry one", "--severity", "p2"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Simulate a torn write + stray garbage between two valid appends.
	logPath := filepath.Join(dir, ".forge", "friction.jsonl")
	torn := `{"id":"fr-torn","recorded_at":"2026-06-10T00:00:00Z","sev` + "\n" + "not json at all\n"
	if err := appendRawForTest(logPath, torn); err != nil {
		t.Fatalf("append garbage: %v", err)
	}
	if _, _, err := runFriction(t, "", "add", "entry two"); err != nil {
		t.Fatalf("add after garbage: %v", err)
	}

	out, stderr, err := runFriction(t, "", "list", "--json")
	if err != nil {
		t.Fatalf("list must not fail on malformed lines: %v", err)
	}
	var entries []FrictionEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("stdout must stay pure JSON: %v\n%s", err, out)
	}
	if len(entries) != 2 {
		t.Errorf("want the 2 valid entries, got %d", len(entries))
	}
	if !strings.Contains(stderr, "skipped 2 malformed line(s)") {
		t.Errorf("stderr should count skipped lines, got: %q", stderr)
	}

	// Loader-level assertion too (audit reuses it).
	loaded, malformed, err := loadFrictionEntries(logPath)
	if err != nil {
		t.Fatalf("loadFrictionEntries: %v", err)
	}
	if len(loaded) != 2 || malformed != 2 {
		t.Errorf("loadFrictionEntries: got %d entries / %d malformed, want 2 / 2", len(loaded), malformed)
	}
}

// TestAuditFriction covers the audit category: empty, populated, and
// torn-write states — and that the category is additive (status stays
// ok for healthy logs so standing friction never gates CI).
func TestAuditFriction(t *testing.T) {
	t.Run("no log file", func(t *testing.T) {
		cat := auditFriction(t.TempDir())
		if cat.Status != AuditStatusOK {
			t.Errorf("missing log should be ok, got %s", cat.Status)
		}
		if !strings.Contains(cat.Summary, "no friction recorded") {
			t.Errorf("summary: %q", cat.Summary)
		}
	})

	t.Run("entries summarized by severity", func(t *testing.T) {
		dir := withTempProject(t, minimalFrictionForgeYAML)
		for _, args := range [][]string{
			{"add", "p0 thing", "--severity", "p0", "--area", "codegen"},
			{"add", "p1 thing", "--severity", "p1"},
			{"add", "another p1", "--severity", "p1"},
			{"add", "just a note"},
		} {
			if _, _, err := runFriction(t, "", args...); err != nil {
				t.Fatalf("add %v: %v", args, err)
			}
		}

		cat := auditFriction(dir)
		if cat.Status != AuditStatusOK {
			t.Errorf("healthy log must stay ok (friction never gates CI), got %s", cat.Status)
		}
		if !strings.Contains(cat.Summary, "4 friction entries") ||
			!strings.Contains(cat.Summary, "p0: 1") ||
			!strings.Contains(cat.Summary, "p1: 2") ||
			!strings.Contains(cat.Summary, "note: 1") {
			t.Errorf("summary should count by severity: %q", cat.Summary)
		}
		if cat.Details["count"] != 4 {
			t.Errorf("details.count = %v", cat.Details["count"])
		}
		bySev, ok := cat.Details["by_severity"].(map[string]int)
		if !ok || bySev["p1"] != 2 {
			t.Errorf("details.by_severity = %v", cat.Details["by_severity"])
		}
		newest, ok := cat.Details["newest_recorded_at"].(string)
		if !ok {
			t.Fatalf("details.newest_recorded_at missing: %v", cat.Details)
		}
		if _, err := time.Parse(time.RFC3339, newest); err != nil {
			t.Errorf("newest_recorded_at not RFC3339: %q", newest)
		}
		hint, _ := cat.Details["hint"].(string)
		if !strings.Contains(hint, "friction list") {
			t.Errorf("hint should point at `forge friction list`: %q", hint)
		}
	})

	t.Run("malformed lines warn", func(t *testing.T) {
		dir := withTempProject(t, minimalFrictionForgeYAML)
		if _, _, err := runFriction(t, "", "add", "valid entry"); err != nil {
			t.Fatalf("add: %v", err)
		}
		logPath := filepath.Join(dir, ".forge", "friction.jsonl")
		if err := appendRawForTest(logPath, "{torn\n"); err != nil {
			t.Fatalf("append: %v", err)
		}
		cat := auditFriction(dir)
		if cat.Status != AuditStatusWarn {
			t.Errorf("torn writes should warn, got %s", cat.Status)
		}
		if cat.Details["malformed_lines"] != 1 {
			t.Errorf("details.malformed_lines = %v", cat.Details["malformed_lines"])
		}
	})

	t.Run("additive: full report carries the category and round-trips", func(t *testing.T) {
		dir := withTempProject(t, minimalFrictionForgeYAML)
		if _, _, err := runFriction(t, "", "add", "report-level entry", "--severity", "p2"); err != nil {
			t.Fatalf("add: %v", err)
		}
		report, err := buildAuditReport(dir)
		if err != nil {
			t.Fatalf("buildAuditReport: %v", err)
		}
		cat, ok := report.Categories["friction"]
		if !ok {
			t.Fatal("audit report missing friction category")
		}
		if cat.Status != AuditStatusOK {
			t.Errorf("friction category status = %s, want ok", cat.Status)
		}
		// Additivity: the JSON shape must survive marshal/unmarshal so
		// existing consumers (which iterate categories) keep working.
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded AuditReport
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := decoded.Categories["friction"]; !ok {
			t.Error("friction category lost in JSON round-trip")
		}
	})
}

// TestFrictionAdd_NeverRewritesExistingLines pins the append-only
// contract byte-for-byte: whatever the file held before an add, it
// still holds (as a prefix) after.
func TestFrictionAdd_NeverRewritesExistingLines(t *testing.T) {
	dir := withTempProject(t, minimalFrictionForgeYAML)
	logPath := filepath.Join(dir, ".forge", "friction.jsonl")

	if _, _, err := runFriction(t, "", "add", "first"); err != nil {
		t.Fatalf("add: %v", err)
	}
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, _, err := runFriction(t, "", "add", "second"); err != nil {
		t.Fatalf("add: %v", err)
	}
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Error("add rewrote existing bytes — the log must be append-only")
	}
}

func appendRawForTest(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(content)
	return err
}
