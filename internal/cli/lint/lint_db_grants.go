// File: internal/cli/lint/lint_db_grants.go
//
// db-grants — `forge lint --db-grants`.
//
// The read-only role (pkg/dbgrants) may read a column only after someone
// decided it may. A column whose NAME looks like a credential (a token, a
// password, a key, ciphertext, a hash of one) and that carries neither
// `forge:secret` nor `forge:readable <reason>` is UNDECIDED: the generated
// grants withhold it, and this lane makes the omission an error, so a project
// cannot ship `forge_ro` reading credentials by oversight and cannot ship a
// silently unreadable column either.
//
// The decision is declared where the column lives:
//
//	COMMENT ON COLUMN git_credentials.access_token IS 'forge:secret';
//	COMMENT ON COLUMN usage_events.token_hash IS 'forge:readable — hash of the prompt, not a credential';
//
// or, for a column with a wire field, `// forge:secret` on the proto field.
// The same dbgrants.Column.Disposition judges here and in `forge generate`, so
// the lint and the grants cannot disagree about which columns are withheld.
//
// The lane also judges db/grants/dashboard_gen.sql and its per-table rows
// (db/grants/dashboard/), the dashboard role's grants: they are a function of the forge:dashboard / forge:no-dashboard
// comments, so a marker added or removed without `forge generate` leaves the
// file granting what the schema no longer says (or not granting what it does).
// The file is compared with a fresh projection and any difference is reported
// as stale. A hand edit to the file is the generated-drift lane's.

package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/dbgrants"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

const (
	dbGrantsRule           = "forge-db-grants-undecided"
	dbGrantsUnverifiedRule = "forge-db-grants-unverified"
	dbGrantsStaleRule      = "forge-db-grants-stale"
)

// dbGrantsFinding is one undecided credential-shaped column.
type dbGrantsFinding struct {
	Schema, Table, Column string
	// File and Line locate the column's definition in the migrations, best
	// effort: 0 when no migration text names it.
	File string
	Line int
	// Comment is the column's existing catalog comment. COMMENT ON replaces it
	// whole, so the fix must carry it forward.
	Comment string
}

func (f dbGrantsFinding) qualified() string {
	if f.Schema == "public" || f.Schema == "" {
		return f.Table + "." + f.Column
	}
	return f.Schema + "." + f.Table + "." + f.Column
}

func (f dbGrantsFinding) message() string {
	return fmt.Sprintf("%s looks like a credential and nobody decided it: forge_ro cannot read it", f.qualified())
}

func (f dbGrantsFinding) fixHint() string {
	target := f.Table + "." + f.Column
	if f.Schema != "public" && f.Schema != "" {
		target = f.Schema + "." + target
	}
	prior := ""
	if strings.TrimSpace(f.Comment) != "" {
		prior = fmt.Sprintf(" COMMENT ON replaces the whole comment, so keep its text: it is %q.", f.Comment)
	}
	return fmt.Sprintf("in a new migration, declare one: COMMENT ON COLUMN %s IS 'forge:secret'; (a credential: stays hidden) "+
		"or COMMENT ON COLUMN %s IS 'forge:readable <why it is not a credential>'; (granted to forge_ro).%s "+
		"A proto field marked `// forge:secret` also counts. See `forge skill load db/query`.", target, target, prior)
}

// dbGrantsReport is everything one run found. Unverified is non-empty when the
// lane could not run, which must never read as clean.
type dbGrantsReport struct {
	Findings []dbGrantsFinding
	// Stale are the generated grants files that no longer match the schema.
	Stale      []dbGrantsStale
	Unverified string
}

// dbGrantsStale is one generated grants file that differs from what the
// current schema projects.
type dbGrantsStale struct {
	// File is the grants file, relative to the project root.
	File string
	// Missing: the file does not exist at all.
	Missing bool
}

func (s dbGrantsStale) message() string {
	if s.Missing {
		return fmt.Sprintf("%s is missing: the dashboard role (%s) has no generated grants", s.File, dbgrants.DashboardRoleName)
	}
	return fmt.Sprintf("%s is stale: it no longer matches the forge:dashboard / forge:no-dashboard markers and the schema, so %s is granted something other than what they declare", s.File, dbgrants.DashboardRoleName)
}

func (dbGrantsStale) fixHint() string {
	return "run `forge generate` and commit db/grants/dashboard_gen.sql and db/grants/dashboard/; the migrator applies them after every run once the role exists. See `forge skill load db`, \"Dashboards over your database\""
}

// collectDBGrantsFindings is the shared engine behind text mode and
// `forge lint --json`. A project without migrations has no schema and yields
// nothing.
func collectDBGrantsFindings(root, migrationsDir string) (dbGrantsReport, error) {
	var rep dbGrantsReport
	migDir := migrationsDir
	if !filepath.IsAbs(migDir) {
		migDir = filepath.Join(root, migDir)
	}
	if !dirHasUpSQL(migDir) {
		return rep, nil
	}
	services, err := codegen.ParseReadOnlyGrantsServices(root)
	if err != nil {
		return rep, fmt.Errorf("read the compiled descriptor for forge:secret proto markers: %w", err)
	}
	if services == nil && dirExists(filepath.Join(root, protoDirDefault)) {
		// Without the descriptor the proto-side `forge:secret` markers are
		// invisible, and every column they cover would be reported undecided.
		rep.Unverified = "gen/forge_descriptor/ is missing, so proto `// forge:secret` markers cannot be read; run `forge generate` first"
		return rep, nil
	}
	tables, err := shadowdb.Tables(root, migDir)
	if err != nil {
		rep.Unverified = fmt.Sprintf("could not apply %s to a shadow postgres to read the schema: %v", migrationsDir, err)
		return rep, nil
	}
	if err := schemadef.CheckMarkers(tables); err != nil {
		return rep, err
	}
	comments := map[string]string{}
	for _, t := range tables {
		for _, c := range t.Columns {
			comments[t.Name+"."+c.Name] = c.Comment
		}
	}
	files, err := readMigrationSources(migDir)
	if err != nil {
		return rep, err
	}
	for _, u := range codegen.UndecidedReadOnlyColumns(tables, services) {
		f := dbGrantsFinding{Schema: u.Schema, Table: u.Table, Column: u.Column, Comment: comments[u.Table+"."+u.Column]}
		f.File, f.Line = files.locate(root, u.Table, u.Column)
		rep.Findings = append(rep.Findings, f)
	}
	stale, err := dashboardGrantsStale(root, codegen.ReadOnlyGrantTables(tables, services))
	if err != nil {
		return rep, err
	}
	if stale != nil {
		rep.Stale = append(rep.Stale, *stale)
	}
	return rep, nil
}

// dashboardGrantsStale compares the dashboard role's grants on disk
// (db/grants/dashboard_gen.sql and its per-table rows under
// db/grants/dashboard/) with the files `forge generate` would write for
// tables. Bodies are compared with the ownership marker stripped, so a
// freshly generated file is never stale; a row file no table accounts for is.
func dashboardGrantsStale(root string, tables []dbgrants.Table) (*dbGrantsStale, error) {
	want, err := codegen.DashboardGrantsFileSet(tables)
	if err != nil {
		return nil, err
	}
	// The script first, then its rows: a missing script is the finding to
	// name, whatever else is missing with it.
	rels := make([]string, 0, len(want))
	for rel := range want {
		if rel != dbgrants.DashboardGrantsPath {
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	rels = append([]string{dbgrants.DashboardGrantsPath}, rels...)
	for _, rel := range rels {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case os.IsNotExist(err):
			return &dbGrantsStale{File: rel, Missing: true}, nil
		case err != nil:
			return nil, err
		case checksums.BodyHash(got) != checksums.BodyHash(want[rel]):
			return &dbGrantsStale{File: rel}, nil
		}
	}
	entries, _ := os.ReadDir(filepath.Join(root, filepath.FromSlash(dbgrants.DashboardGrantsRowsDir)))
	for _, e := range entries {
		rel := dbgrants.DashboardGrantsRowsDir + "/" + e.Name()
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_gen.sql") && want[rel] == nil {
			return &dbGrantsStale{File: rel}, nil
		}
	}
	return nil, nil
}

func dirHasUpSQL(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			return true
		}
	}
	return false
}

// migrationSource is one migration file's text.
type migrationSource struct{ path, text string }

type migrationSources []migrationSource

func readMigrationSources(dir string) (migrationSources, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out migrationSources
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migrationSource{path: filepath.Join(dir, e.Name()), text: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// locate finds the first line, in a migration that names the table, that
// declares the column. Best effort: a column added by a statement this does not
// recognise is located at the table's file with no line.
func (ms migrationSources) locate(root, table, column string) (string, int) {
	colRE := regexp.MustCompile(`(?i)^\s*(?:add\s+column\s+(?:if\s+not\s+exists\s+)?)?"?` + regexp.QuoteMeta(column) + `"?\s+[A-Za-z]`)
	tableRE := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(table) + `\b`)
	for _, m := range ms {
		if !tableRE.MatchString(m.text) {
			continue
		}
		for i, line := range strings.Split(m.text, "\n") {
			if colRE.MatchString(line) {
				rel, err := filepath.Rel(root, m.path)
				if err != nil {
					rel = m.path
				}
				return filepath.ToSlash(rel), i + 1
			}
		}
	}
	return "", 0
}

// collectDBGrantsJSON maps db-grants findings onto the JSON contract. Severity
// error: an undecided credential is the gap this lane exists to close.
func collectDBGrantsJSON(cwd string, cfg *config.ProjectConfig) ([]lintJSONFinding, error) {
	rep, err := collectDBGrantsFindings(cwd, migrationsDirFor(cfg))
	if err != nil {
		return nil, fmt.Errorf("db-grants lint failed: %w", err)
	}
	var out []lintJSONFinding
	if rep.Unverified != "" {
		out = append(out, lintJSONFinding{
			Severity: lintSevError,
			Rule:     dbGrantsUnverifiedRule,
			Message:  "db-grants did not run: " + rep.Unverified,
		})
	}
	for _, f := range rep.Findings {
		out = append(out, lintJSONFinding{
			File: f.File, Line: f.Line, Severity: lintSevError,
			Rule: dbGrantsRule, Message: f.message(), FixHint: f.fixHint(),
		})
	}
	for _, st := range rep.Stale {
		out = append(out, lintJSONFinding{
			File: st.File, Severity: lintSevError,
			Rule: dbGrantsStaleRule, Message: st.message(), FixHint: st.fixHint(),
		})
	}
	return out, nil
}
