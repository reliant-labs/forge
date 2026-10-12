package migrationlint

import (
	"strings"
	"testing"
)

// TestAdoptingBaselineIsClean: the shape a goose cutover leaves — a baseline
// that declares goose's history, then ordinary migrations — is not a finding.
func TestAdoptingBaselineIsClean(t *testing.T) {
	dir := t.TempDir()
	writeMigrationIn(t, dir, "20261011000000_baseline.up.sql",
		"-- forge:adopts goose 20260211000002 20260211000003\n-- forge:adopts goose 20261011203444\nCREATE TABLE chats (id INT PRIMARY KEY);\n")
	writeMigrationIn(t, dir, "20261012000000_add_orgs.up.sql", "CREATE TABLE orgs (id INT);")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsForRule(result, RuleInvalidAdoption); len(got) != 0 {
		t.Errorf("findings = %+v; want none for a well-formed baseline", got)
	}
}

// TestInvalidAdoptionIsAnError: each declaration the migrator would refuse —
// on every database, at boot — is an error at review time, on its line.
func TestInvalidAdoptionIsAnError(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		file  string
		line  int
		want  string
	}{
		{
			name: "unreadable list",
			files: map[string]string{
				"20261011000000_baseline.up.sql": "-- squashed\n-- forge:adopts goose 20260211000002 latest\nCREATE TABLE chats (id INT);",
			},
			file: "20261011000000_baseline.up.sql", line: 2, want: `"latest" is not a goose version`,
		},
		{
			name: "misspelled directive",
			files: map[string]string{
				"20261011000000_baseline.up.sql": "-- forge:adopt goose 1\nCREATE TABLE chats (id INT);",
			},
			file: "20261011000000_baseline.up.sql", line: 1, want: "forge:adopts",
		},
		{
			name: "not the first migration",
			files: map[string]string{
				"20261010000000_first.up.sql":    "CREATE TABLE a (id INT);",
				"20261011000000_baseline.up.sql": "SELECT 1;\n-- forge:adopts goose 1\nCREATE TABLE chats (id INT);",
			},
			file: "20261011000000_baseline.up.sql", line: 2, want: "20261010000000_first.up.sql comes before this one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				writeMigrationIn(t, dir, name, body)
			}
			result, err := LintMigrationsDir(dir, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			got := findingsForRule(result, RuleInvalidAdoption)
			if len(got) != 1 {
				t.Fatalf("findings = %+v; want exactly one %s", result.Findings, RuleInvalidAdoption)
			}
			f := got[0]
			if !strings.HasSuffix(f.File, tc.file) || f.Line != tc.line || f.Severity != SeverityError {
				t.Errorf("finding at %s:%d (%s); want error at %s:%d", f.File, f.Line, f.Severity, tc.file, tc.line)
			}
			if !strings.Contains(f.Message, tc.want) {
				t.Errorf("message %q does not say %q", f.Message, tc.want)
			}
			if !strings.Contains(RemediationFor(f.Rule), "FIRST migration") {
				t.Errorf("remediation for %s is not the adoption fix: %q", f.Rule, RemediationFor(f.Rule))
			}
		})
	}
}
