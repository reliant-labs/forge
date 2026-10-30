package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/dbgrants"
)

// writeGrantsProject lays down a project whose only schema is migrations, so
// the lane has nothing proto-side to read.
func writeGrantsProject(t *testing.T, migrations map[string]string) string {
	t.Helper()
	root := t.TempDir()
	migDir := filepath.Join(root, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, sql := range migrations {
		if err := os.WriteFile(filepath.Join(migDir, name), []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const grantsBirthSQL = `CREATE TABLE git_credentials (
    id            TEXT PRIMARY KEY,
    provider      TEXT NOT NULL,
    access_token  TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    token_hash    TEXT NOT NULL,
    input_tokens  BIGINT NOT NULL DEFAULT 0
);
`

func grantsReport(t *testing.T, root string) dbGrantsReport {
	t.Helper()
	if testing.Short() {
		t.Skip("applies migrations to a shadow postgres; skipped under -short")
	}
	rep, err := collectDBGrantsFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectDBGrantsFindings: %v", err)
	}
	if rep.Unverified != "" {
		t.Fatalf("lane did not run: %s", rep.Unverified)
	}
	return rep
}

func names(rep dbGrantsReport) string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.qualified())
	}
	return strings.Join(out, ",")
}

// Every undecided credential-shaped column is named, with the file and line of
// its definition and both ways to decide. The numeric counter is not one.
func TestDBGrants_ReportsUndecidedCredentialColumns(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": grantsBirthSQL})
	rep := grantsReport(t, root)
	if got, want := names(rep), "git_credentials.access_token,git_credentials.refresh_token,git_credentials.token_hash"; got != want {
		t.Fatalf("findings = %s, want %s", got, want)
	}
	f := rep.Findings[0]
	if f.File != "db/migrations/0001_birth.up.sql" || f.Line != 4 {
		t.Errorf("located at %s:%d, want db/migrations/0001_birth.up.sql:4", f.File, f.Line)
	}
	for _, want := range []string{"forge:secret", "forge:readable", "COMMENT ON COLUMN git_credentials.access_token"} {
		if !strings.Contains(f.fixHint(), want) {
			t.Errorf("fix hint lacks %q: %s", want, f.fixHint())
		}
	}

}

// A decision, in either spelling, clears the finding; an undecided neighbour
// stays reported.
func TestDBGrants_DecisionsClearTheFinding(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{
		"0001_birth.up.sql": grantsBirthSQL,
		"0002_decide.up.sql": `
COMMENT ON COLUMN git_credentials.access_token IS 'forge:secret';
COMMENT ON COLUMN git_credentials.token_hash IS 'forge:readable — content hash of the clone URL';
`,
	})
	if got, want := names(grantsReport(t, root)), "git_credentials.refresh_token"; got != want {
		t.Errorf("findings = %s, want only %s", got, want)
	}
}

// The comment already on a column survives the fix: COMMENT ON replaces it, so
// the hint carries it forward.
func TestDBGrants_FixHintCarriesTheExistingComment(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{
		"0001_birth.up.sql": grantsBirthSQL,
		"0002_note.up.sql":  `COMMENT ON COLUMN git_credentials.refresh_token IS 'forge:immutable';`,
	})
	rep := grantsReport(t, root)
	for _, f := range rep.Findings {
		if f.Column == "refresh_token" && !strings.Contains(f.fixHint(), "forge:immutable") {
			t.Errorf("hint drops the existing comment: %s", f.fixHint())
		}
	}
}

func TestDBGrants_CleanProjectAndNoMigrations(t *testing.T) {
	clean := writeGrantsProject(t, map[string]string{"0001_notes.up.sql": "CREATE TABLE notes (id TEXT PRIMARY KEY, body TEXT NOT NULL);"})
	if rep := grantsReport(t, clean); len(rep.Findings) != 0 {
		t.Errorf("clean schema reported: %+v", rep.Findings)
	}
	rep, err := collectDBGrantsFindings(t.TempDir(), "db/migrations")
	if err != nil || len(rep.Findings) != 0 || rep.Unverified != "" {
		t.Errorf("no migrations: (%+v, %v), want nothing", rep, err)
	}
}

// The JSON lane gates: an undecided column is an error, and a lane that could
// not run is an error too, never silence.
func TestDBGrants_JSONSeverityIsError(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": grantsBirthSQL})
	grantsReport(t, root) // skips under -short
	fs, err := collectDBGrantsJSON(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var undecided []lintJSONFinding
	for _, f := range fs {
		// This project never ran `forge generate`, so its dashboard grants file
		// is missing too; that finding has its own tests below.
		if f.Rule == dbGrantsStaleRule {
			continue
		}
		undecided = append(undecided, f)
	}
	if len(undecided) != 3 {
		t.Fatalf("%d JSON findings, want 3: %+v", len(undecided), fs)
	}
	for _, f := range undecided {
		if f.Severity != lintSevError || f.Rule != dbGrantsRule || f.File == "" || f.Line == 0 || f.FixHint == "" {
			t.Errorf("finding not actionable: %+v", f)
		}
	}
}

// ---- db/grants/dashboard_gen.sql drift ----

const dashboardBirthSQL = `CREATE TABLE usage_events (
    id         TEXT PRIMARY KEY,
    model      TEXT NOT NULL,
    user_email TEXT NOT NULL
);
CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT NOT NULL);
COMMENT ON TABLE usage_events IS 'forge:dashboard';
COMMENT ON COLUMN usage_events.user_email IS 'forge:no-dashboard';
`

// writeDashboardGrants writes the file `forge generate` would for the project's
// own schema, optionally rewritten by edit.
func writeDashboardGrants(t *testing.T, root string, edit func(string) string) {
	t.Helper()
	tables := dashboardProjection(t, root)
	script, err := dbgrants.RenderDashboard(tables)
	if err != nil {
		t.Fatal(err)
	}
	body := string(codegen.DashboardGrantsFile(script))
	if edit != nil {
		body = edit(body)
	}
	path := filepath.Join(root, filepath.FromSlash(dbgrants.DashboardGrantsPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dashboardProjection(t *testing.T, root string) []dbgrants.Table {
	t.Helper()
	tables, err := shadowdb.Tables(root, filepath.Join(root, "db", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return codegen.ReadOnlyGrantTables(tables, nil)
}

func staleFiles(rep dbGrantsReport) []string {
	var out []string
	for _, s := range rep.Stale {
		if s.Missing {
			out = append(out, s.File+" (missing)")
		} else {
			out = append(out, s.File)
		}
	}
	return out
}

// A project whose dashboard grants file is what generate writes is clean.
func TestDBGrants_DashboardGrantsCurrentIsClean(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": dashboardBirthSQL})
	grantsReport(t, root) // skips under -short
	writeDashboardGrants(t, root, nil)
	rep := grantsReport(t, root)
	if len(rep.Stale) != 0 || len(rep.Findings) != 0 {
		t.Errorf("a current project reports stale=%v findings=%v", staleFiles(rep), names(rep))
	}
}

// A marker added in a migration without `forge generate`, a marker removed, a
// no-dashboard column added: each leaves the committed file disagreeing with
// the schema, and the lane names it.
func TestDBGrants_DashboardGrantsDriftIsReported(t *testing.T) {
	for name, tc := range map[string]struct {
		migration string
	}{
		"a new table marked":               {`COMMENT ON TABLE users IS 'forge:dashboard';`},
		"a marker removed":                 {`COMMENT ON TABLE usage_events IS NULL;`},
		"a column withheld":                {`COMMENT ON COLUMN usage_events.model IS 'forge:no-dashboard';`},
		"a column un-withheld":             {`COMMENT ON COLUMN usage_events.user_email IS NULL;`},
		"a column added to a marked table": {`ALTER TABLE usage_events ADD COLUMN cost BIGINT NOT NULL DEFAULT 0;`},
	} {
		root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": dashboardBirthSQL})
		grantsReport(t, root) // skips under -short
		writeDashboardGrants(t, root, nil)
		if err := os.WriteFile(filepath.Join(root, "db", "migrations", "0002_change.up.sql"), []byte(tc.migration), 0o644); err != nil {
			t.Fatal(err)
		}
		rep := grantsReport(t, root)
		if got := staleFiles(rep); len(got) != 1 || got[0] != dbgrants.DashboardGrantsPath {
			t.Errorf("%s: stale = %v, want %s", name, got, dbgrants.DashboardGrantsPath)
		}
	}
}

// The file missing altogether is stale too: the role would have no grants.
func TestDBGrants_MissingDashboardGrantsIsReported(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": dashboardBirthSQL})
	rep := grantsReport(t, root)
	if got := staleFiles(rep); len(got) != 1 || !strings.HasSuffix(got[0], "(missing)") {
		t.Errorf("stale = %v, want the missing dashboard file", got)
	}
}

// The ownership stamp is not part of the comparison: the file `forge generate`
// wrote (with its forge:hash line) is never stale against itself.
func TestDBGrants_StampIsIgnoredWhenJudgingStaleness(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": dashboardBirthSQL})
	grantsReport(t, root)
	writeDashboardGrants(t, root, func(body string) string {
		return strings.Replace(body, "-- Code generated by forge. DO NOT EDIT.\n", "-- Code generated by forge. DO NOT EDIT.\n-- forge:hash=deadbeef\n", 1)
	})
	if rep := grantsReport(t, root); len(rep.Stale) != 0 {
		t.Errorf("a stamped file is stale: %v", staleFiles(rep))
	}
}

// The JSON lane gates, with a fix hint that names the command and the skill.
func TestDBGrants_StaleJSONFinding(t *testing.T) {
	root := writeGrantsProject(t, map[string]string{"0001_birth.up.sql": dashboardBirthSQL})
	grantsReport(t, root)
	fs, err := collectDBGrantsJSON(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stale []lintJSONFinding
	for _, f := range fs {
		if f.Rule == dbGrantsStaleRule {
			stale = append(stale, f)
		}
	}
	if len(stale) != 1 {
		t.Fatalf("%d stale findings in %+v, want 1", len(stale), fs)
	}
	f := stale[0]
	if f.Severity != lintSevError || f.File != dbgrants.DashboardGrantsPath || !strings.Contains(f.FixHint, "forge generate") || !strings.Contains(f.FixHint, "Dashboards over your database") {
		t.Errorf("finding not actionable: %+v", f)
	}
}

// A project with no migrations reports nothing: there is no schema to be stale against.
func TestDBGrants_NoMigrationsHasNoStaleDashboardGrants(t *testing.T) {
	rep, err := collectDBGrantsFindings(t.TempDir(), "db/migrations")
	if err != nil || len(rep.Stale) != 0 {
		t.Errorf("(%+v, %v), want nothing", rep, err)
	}
}
