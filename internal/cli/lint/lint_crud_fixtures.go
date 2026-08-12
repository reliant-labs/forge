// File: internal/cli/lint/lint_crud_fixtures.go
//
// crud-fixtures — `forge lint --crud-fixtures`.
//
// A generated CRUD lifecycle test seeds its entity's foreign-key parents as
// SQL literals, written into internal/handlers/<svc>/handlers_crud_test.go at
// scaffold time. Those literals are derived from the schema AS IT WAS THEN,
// and the file is scaffold-once: forge writes it exactly once and never
// touches it again. The schema, meanwhile, keeps moving — forge's own charter
// tells authors to "add relationships, indexes, and constraints with
// hand-written migrations".
//
// So a foreign key added in a LATER migration silently invalidates fixtures
// that were correct when they were written. For a column with no FK at birth
// the generator has nothing to point at and emits the synthetic placeholder
// (`sample_<column>_1`); the moment the constraint exists, that value
// references nothing:
//
//	seed parent rows: pq: insert or update on table "crews" violates
//	foreign key constraint "crews_foreman_id_fkey"
//
// One stale literal fails the WHOLE lifecycle test, and a schema-wide change
// hits every entity whose seed block touches the column — the run that
// motivated this check turned one new constraint into nine failures across
// four packages, all in setup, all reading like a broken test harness rather
// than a fixture that has aged out of its schema.
//
// ── Why lint, and not the generate-time guard ─────────────────────────────
//
// internal/codegen/crud_fixture_guard.go already verifies fixtures against
// the applied schema and REFUSES to generate when they violate a CHECK. It
// cannot see this defect, and no extension of it could: GenerateCRUDTests
// returns at the scaffold-once ledger check BEFORE it builds the fixture
// model or opens a shadow database. That early return is the correct
// behaviour — the file is the user's, and re-deriving fixtures for a file
// forge will not write is wasted work — but it means the only run that
// introspects the schema is the run that BIRTHS the file. Every later
// generate, including the one right after the migration that adds the
// constraint, skips the whole path. The guard is a birth check; this is an
// aging problem, and aging problems belong to a checker that runs
// continuously over what is on disk.
//
// ── What it checks ────────────────────────────────────────────────────────
//
// Purely textual, no database. Foreign keys are read out of db/migrations
// (ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY, plus inline and
// table-level REFERENCES in CREATE TABLE), and the seed INSERTs are read out
// of each handlers_crud_test.go. A finding is one seeded value in an
// FK-constrained column that is a plain literal, is not NULL, and matches no
// value seeded for the referenced column ANYWHERE in the same file.
//
// The test starts against a freshly migrated database, so the seed block is
// the entire population the INSERT can reference — which is what makes a
// static answer sound rather than a guess.
//
// It deliberately does NOT key on the `sample_` stamp. A parent whose own
// primary key is a synthesized string is seeded with `sample_id_1` and the
// child referencing it is CORRECT; keying on the spelling would flag it. The
// question asked is the one postgres will ask: does a row with this value
// exist? Conversely a hand-written literal that never matched anything is
// caught too, which a stamp check would have missed entirely.
//
// Severity is warning, never gating. The fixture is genuinely wrong and the
// test genuinely fails, but the remedy is an edit to a file forge does not
// own, and refusing to lint the rest of the project over it would be a
// generator holding a user's file hostage. The finding names the column, the
// constraint, the migration that introduced it, and the line to edit — the
// four facts the pq error withheld.

package lint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// crudFixtureFinding is one seeded FK value that references no seeded row.
// File is root-relative, Line is 1-indexed and points at the value itself.
type crudFixtureFinding struct {
	File       string
	Line       int
	Table      string
	Column     string
	Value      string // the literal as written in the file
	RefTable   string
	RefColumn  string
	Constraint string
	// DeclaredIn is the root-relative migration that declared the foreign
	// key, "" when it could not be attributed. It is the fact that turns
	// "this fixture is wrong" into "this fixture predates this constraint".
	DeclaredIn string
}

// crudFixtureFixHint renders the remediation. It states the ownership fact
// first — the author was told forge would never touch this file, so a
// finding about it must say plainly that editing it is theirs to do and
// will not be reverted.
func crudFixtureFixHint(f crudFixtureFinding) string {
	origin := ""
	if f.DeclaredIn != "" {
		origin = fmt.Sprintf(" (declared in %s)", f.DeclaredIn)
	}
	return fmt.Sprintf(
		"foreign key %s%s requires %s.%s to name an existing %s.%s row; %s matches none seeded in this file. "+
			"These fixtures were scaffolded from the schema as it was then, and handlers_crud_test.go is yours — "+
			"scaffolded once, never regenerated — so edit it in place: use NULL if the column is nullable, "+
			"or the id of a %s row seeded earlier in the same block.",
		f.Constraint, origin, f.Table, f.Column, f.RefTable, f.RefColumn, f.Value, f.RefTable)
}

// runCrudFixturesLint is the text-mode entry point.
func runCrudFixturesLint(cwd string, cfg *config.ProjectConfig) error {
	fmt.Println("Running crud-fixtures lint...")
	findings, err := collectCrudFixtureFindings(cwd, migrationsDirFor(cfg))
	if err != nil {
		return err
	}
	formatCrudFixtures(os.Stdout, findings)
	return nil
}

// formatCrudFixtures writes the human report, matching the sibling advisory
// lanes: a single success line when clean, one ⚠ block per finding otherwise.
func formatCrudFixtures(w io.Writer, findings []crudFixtureFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  crud-fixtures clean — every seeded foreign-key value names a row the same file seeds")
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ⚠ [forge-crud-fixtures] %s:%d\n", f.File, f.Line)
		_, _ = fmt.Fprintf(w, "      → %s\n", crudFixtureFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d stale CRUD fixture value(s).\n", len(findings))
	_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
}

// collectCrudFixtureFindings is the shared engine behind text mode and
// `forge lint --json`.
//
// A missing migrations directory or a project with no scaffolded lifecycle
// tests yields no findings rather than an error: both are ordinary states for
// a project this lane does not apply to, and a lane that does not apply is
// not a gap.
func collectCrudFixtureFindings(root, migrationsDir string) ([]crudFixtureFinding, error) {
	if !filepath.IsAbs(migrationsDir) {
		migrationsDir = filepath.Join(root, migrationsDir)
	}
	fks, err := foreignKeysFromMigrations(root, migrationsDir)
	if err != nil {
		return nil, err
	}
	if len(fks) == 0 {
		return nil, nil
	}

	testFiles, err := crudTestFiles(root)
	if err != nil {
		return nil, err
	}

	var findings []crudFixtureFinding
	for _, path := range testFiles {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", path, rerr)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		findings = append(findings, danglingSeedValues(filepath.ToSlash(rel), string(data), fks)...)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// crudTestFiles returns every scaffolded lifecycle test under
// internal/handlers, sorted for deterministic output.
func crudTestFiles(root string) ([]string, error) {
	handlersDir := filepath.Join(root, "internal", "handlers")
	if _, err := os.Stat(handlersDir); os.IsNotExist(err) {
		return nil, nil
	}
	var files []string
	if err := filepath.WalkDir(handlersDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "handlers_crud_test.go" {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk %s: %w", handlersDir, err)
	}
	sort.Strings(files)
	return files, nil
}

// danglingSeedValues reports every seeded FK value in one lifecycle test that
// names no row the same file seeds.
//
// The seeded population is indexed across the WHOLE file, not per statement:
// one file holds a lifecycle test per entity, each with its own seed block,
// and a parent seeded for one entity is present in the database for the
// others too. Indexing per block would report a dangling reference for a row
// that exists.
func danglingSeedValues(relPath, content string, fks map[string]map[string]migrationFK) []crudFixtureFinding {
	blanked := blankSQLComments(content)
	inserts := parseSeedInserts(blanked)
	if len(inserts) == 0 {
		return nil
	}

	// seeded[table][column] = the set of values that table's INSERTs write.
	seeded := map[string]map[string]map[string]bool{}
	for _, ins := range inserts {
		for _, row := range ins.rows {
			for i, val := range row {
				if i >= len(ins.cols) {
					break
				}
				text, kind := decodeSQLLiteral(val.raw)
				if kind != literalPlain {
					continue
				}
				if seeded[ins.table] == nil {
					seeded[ins.table] = map[string]map[string]bool{}
				}
				if seeded[ins.table][ins.cols[i]] == nil {
					seeded[ins.table][ins.cols[i]] = map[string]bool{}
				}
				seeded[ins.table][ins.cols[i]][text] = true
			}
		}
	}

	var findings []crudFixtureFinding
	for _, ins := range inserts {
		byColumn := fks[ins.table]
		if len(byColumn) == 0 {
			continue
		}
		for _, row := range ins.rows {
			for i, val := range row {
				if i >= len(ins.cols) {
					break
				}
				fk, constrained := byColumn[ins.cols[i]]
				if !constrained {
					continue
				}
				text, kind := decodeSQLLiteral(val.raw)
				// NULL satisfies any foreign key, and a value forge cannot
				// read statically (a function call, an expression) is not
				// evidence of a dangling reference — the check only ever
				// speaks about a literal it fully understands.
				if kind != literalPlain {
					continue
				}
				if seeded[fk.RefTable][fk.RefColumn][text] {
					continue
				}
				findings = append(findings, crudFixtureFinding{
					File:       relPath,
					Line:       lineOf(content, val.offset),
					Table:      ins.table,
					Column:     ins.cols[i],
					Value:      strings.TrimSpace(val.raw),
					RefTable:   fk.RefTable,
					RefColumn:  fk.RefColumn,
					Constraint: fk.Constraint,
					DeclaredIn: fk.DeclaredIn,
				})
			}
		}
	}
	return findings
}

// ── SQL literals ──────────────────────────────────────────────────────────

type literalKind int

const (
	// literalPlain is a value whose identity is fully determined by the
	// text: a quoted string, a number, a boolean.
	literalPlain literalKind = iota
	// literalNull is NULL, which no foreign key rejects.
	literalNull
	// literalOpaque is anything else — a function call, a cast of an
	// expression, a parameter. Its runtime value is unknown here.
	literalOpaque
)

// castSuffixRe matches a trailing `::type` cast, including array and
// qualified spellings (`::uuid`, `::text[]`, `::public.citext`).
var castSuffixRe = regexp.MustCompile(`(?i)::\s*[\w.]+(\s*\[\s*\])*\s*$`)

// decodeSQLLiteral reduces a SQL literal to the text that identifies it, so
// a child's reference and its parent's key compare as VALUES rather than as
// source spellings. Casts are stripped and string quoting is undone, which
// is what lets `'8a4f…'::uuid` in one statement match `'8a4f…'` in another.
func decodeSQLLiteral(raw string) (string, literalKind) {
	s := strings.TrimSpace(raw)
	for {
		trimmed := castSuffixRe.ReplaceAllString(s, "")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == s {
			break
		}
		s = trimmed
	}
	if s == "" {
		return "", literalOpaque
	}
	if strings.EqualFold(s, "null") {
		return "", literalNull
	}
	if s[0] == '\'' && len(s) >= 2 && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), literalPlain
	}
	// A bare token that is a number or boolean identifies itself; anything
	// else (a call, an operator expression) does not.
	if isNumericLiteral(s) || strings.EqualFold(s, "true") || strings.EqualFold(s, "false") {
		return strings.ToLower(s), literalPlain
	}
	return "", literalOpaque
}

// isNumericLiteral reports whether s is a plain decimal number.
func isNumericLiteral(s string) bool {
	seenDigit := false
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			seenDigit = true
		case r == '-' || r == '+':
			if i != 0 {
				return false
			}
		case r == '.':
		default:
			return false
		}
	}
	return seenDigit
}

// ── Seed INSERT parsing ───────────────────────────────────────────────────

type seedValue struct {
	raw    string
	offset int
}

type seedInsert struct {
	table string
	cols  []string
	rows  [][]seedValue
}

// insertHeadRe matches `INSERT INTO <table> (<columns>) VALUES`.
var insertHeadRe = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+("?[\w.]+"?)\s*\(([^()]*)\)\s*VALUES`)

// parseSeedInserts extracts every INSERT ... VALUES statement and its value
// tuples. Offsets are relative to the input, which the caller keeps aligned
// with the original file by blanking comments in place rather than removing
// them.
func parseSeedInserts(content string) []seedInsert {
	var out []seedInsert
	for _, m := range insertHeadRe.FindAllStringSubmatchIndex(content, -1) {
		table := normIdent(content[m[2]:m[3]])
		var cols []string
		for _, c := range strings.Split(content[m[4]:m[5]], ",") {
			cols = append(cols, normIdent(c))
		}
		rows, _ := parseValueTuples(content, m[1])
		if len(rows) == 0 {
			continue
		}
		out = append(out, seedInsert{table: table, cols: cols, rows: rows})
	}
	return out
}

// parseValueTuples reads the comma-separated `( ... )` tuples that follow
// VALUES, stopping at the first token that is not another tuple (ON CONFLICT,
// RETURNING, the terminating semicolon). It returns the tuples and the offset
// just past the last one.
func parseValueTuples(content string, start int) ([][]seedValue, int) {
	var rows [][]seedValue
	i := start
	for {
		i = skipSpace(content, i)
		if i >= len(content) || content[i] != '(' {
			return rows, i
		}
		end, ok := matchParen(content, i)
		if !ok {
			return rows, i
		}
		rows = append(rows, splitTupleValues(content[i+1:end], i+1))
		i = skipSpace(content, end+1)
		if i < len(content) && content[i] == ',' {
			i++
			continue
		}
		return rows, i
	}
}

// splitTupleValues splits one tuple body on top-level commas, carrying each
// value's absolute offset so a finding can point at the value rather than at
// the statement.
func splitTupleValues(body string, base int) []seedValue {
	var out []seedValue
	depth := 0
	inString := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					i++ // an escaped quote, still inside the string
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, seedValue{raw: body[start:i], offset: base + start})
			start = i + 1
		}
	}
	out = append(out, seedValue{raw: body[start:], offset: base + start})
	return out
}

// matchParen returns the offset of the ')' closing the '(' at open, ignoring
// parens inside string literals.
func matchParen(s string, open int) (int, bool) {
	depth := 0
	inString := false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// normIdent strips quoting and schema qualification from an identifier and
// folds it to lower case, matching how postgres resolves the unquoted names
// forge generates.
func normIdent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = strings.Trim(s[i+1:], `"`)
	}
	return strings.ToLower(s)
}

// blankSQLComments replaces SQL comments with spaces, preserving both the
// total length and every newline so byte offsets and line numbers still
// address the original text.
//
// This is load-bearing rather than tidy: forge's own birth migration writes
// the foreign key it CANNOT yet apply as a commented-out suggestion for the
// author to uncomment later. Reading comments would make the check treat
// every such suggestion as an applied constraint and flag fixtures against a
// schema that does not exist.
func blankSQLComments(s string) string {
	out := []byte(s)
	inString := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(out) && out[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '-' && i+1 < len(out) && out[i+1] == '-':
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			for i < len(out) {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i] = ' '
					out[i+1] = ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		}
	}
	return string(out)
}

// ── Foreign keys out of migration text ────────────────────────────────────

// migrationFK is one declared foreign key, with the migration that declared
// it so a finding can attribute the constraint to the change that added it.
type migrationFK struct {
	Table      string
	Column     string
	RefTable   string
	RefColumn  string
	Constraint string
	DeclaredIn string
}

var (
	alterAddFKRe = regexp.MustCompile(
		`(?is)\bALTER\s+TABLE\s+(?:ONLY\s+)?("?[\w.]+"?)\s+ADD\s+CONSTRAINT\s+("?[\w]+"?)\s+FOREIGN\s+KEY\s*\(\s*("?[\w]+"?)\s*\)\s*REFERENCES\s+("?[\w.]+"?)\s*(?:\(\s*("?[\w]+"?)\s*\))?`)
	alterDropConstraintRe = regexp.MustCompile(
		`(?is)\bALTER\s+TABLE\s+(?:ONLY\s+)?("?[\w.]+"?)\s+DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?("?[\w]+"?)`)
	dropTableRe = regexp.MustCompile(
		`(?is)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?("?[\w.]+"?)`)
	createTableRe = regexp.MustCompile(
		`(?is)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?("?[\w.]+"?)\s*\(`)
	tableFKRe = regexp.MustCompile(
		`(?is)^\s*(?:CONSTRAINT\s+("?[\w]+"?)\s+)?FOREIGN\s+KEY\s*\(\s*("?[\w]+"?)\s*\)\s*REFERENCES\s+("?[\w.]+"?)\s*(?:\(\s*("?[\w]+"?)\s*\))?`)
	columnFKRe = regexp.MustCompile(
		`(?is)^\s*("?[\w]+"?)\s+.*?\bREFERENCES\s+("?[\w.]+"?)\s*(?:\(\s*("?[\w]+"?)\s*\))?`)
)

// foreignKeysFromMigrations reads the declared foreign keys out of the
// project's migrations, indexed as fks[table][column].
//
// Migrations are replayed in lexical order — the order the migrator applies
// them — and DROP CONSTRAINT / DROP TABLE remove what earlier files added, so
// the result describes the schema as it stands after the last migration
// rather than every constraint the history ever mentioned.
func foreignKeysFromMigrations(root, migrationsDir string) (map[string]map[string]migrationFK, error) {
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return nil, nil
	}
	var files []string
	if err := filepath.WalkDir(migrationsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".up.sql") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk %s: %w", migrationsDir, err)
	}
	sort.Strings(files)

	// Keyed by constraint name so DROP CONSTRAINT can retract exactly what
	// ADD CONSTRAINT introduced.
	live := map[string]migrationFK{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		applyMigrationFKs(live, blankSQLComments(string(data)), filepath.ToSlash(rel))
	}

	out := map[string]map[string]migrationFK{}
	for _, fk := range live {
		if out[fk.Table] == nil {
			out[fk.Table] = map[string]migrationFK{}
		}
		out[fk.Table][fk.Column] = fk
	}
	return out, nil
}

// applyMigrationFKs folds one migration's foreign-key additions and removals
// into the running set. content must already have its comments blanked.
func applyMigrationFKs(live map[string]migrationFK, content, relPath string) {
	for _, m := range createTableRe.FindAllStringSubmatchIndex(content, -1) {
		table := normIdent(content[m[2]:m[3]])
		open := m[1] - 1 // the '(' the pattern ends on
		end, ok := matchParen(content, open)
		if !ok {
			continue
		}
		for _, fk := range createTableFKs(table, content[open+1:end]) {
			fk.DeclaredIn = relPath
			live[fk.Constraint] = fk
		}
	}

	for _, m := range alterAddFKRe.FindAllStringSubmatch(content, -1) {
		fk := migrationFK{
			Table:      normIdent(m[1]),
			Constraint: normIdent(m[2]),
			Column:     normIdent(m[3]),
			RefTable:   normIdent(m[4]),
			RefColumn:  normIdent(m[5]),
			DeclaredIn: relPath,
		}
		if fk.RefColumn == "" {
			fk.RefColumn = "id"
		}
		live[fk.Constraint] = fk
	}

	for _, m := range alterDropConstraintRe.FindAllStringSubmatch(content, -1) {
		delete(live, normIdent(m[2]))
	}
	for _, m := range dropTableRe.FindAllStringSubmatch(content, -1) {
		dropped := normIdent(m[1])
		for name, fk := range live {
			if fk.Table == dropped {
				delete(live, name)
			}
		}
	}
}

// createTableFKs extracts the foreign keys declared inside a CREATE TABLE
// body, both as table constraints (`FOREIGN KEY (x) REFERENCES ...`) and
// inline on the column (`x TEXT REFERENCES ...`).
//
// An unnamed constraint is given the name postgres would derive for it,
// `<table>_<column>_fkey`, so a later DROP CONSTRAINT naming it retracts the
// right entry.
func createTableFKs(table, body string) []migrationFK {
	var out []migrationFK
	for _, part := range splitTopLevel(body) {
		var fk migrationFK
		switch m := tableFKRe.FindStringSubmatch(part); {
		case m != nil:
			fk = migrationFK{
				Table:      table,
				Constraint: normIdent(m[1]),
				Column:     normIdent(m[2]),
				RefTable:   normIdent(m[3]),
				RefColumn:  normIdent(m[4]),
			}
		default:
			cm := columnFKRe.FindStringSubmatch(part)
			if cm == nil {
				continue
			}
			// A table-level constraint clause is not a column definition;
			// anything whose first token is a constraint keyword is skipped
			// so `PRIMARY KEY (a)` is never read as a column named PRIMARY.
			if isConstraintKeyword(normIdent(cm[1])) {
				continue
			}
			fk = migrationFK{
				Table:     table,
				Column:    normIdent(cm[1]),
				RefTable:  normIdent(cm[2]),
				RefColumn: normIdent(cm[3]),
			}
		}
		if fk.RefColumn == "" {
			fk.RefColumn = "id"
		}
		if fk.Constraint == "" {
			fk.Constraint = fmt.Sprintf("%s_%s_fkey", fk.Table, fk.Column)
		}
		out = append(out, fk)
	}
	return out
}

// isConstraintKeyword reports whether tok opens a table-level constraint
// clause rather than naming a column.
func isConstraintKeyword(tok string) bool {
	switch tok {
	case "primary", "unique", "check", "foreign", "constraint", "exclude", "like":
		return true
	}
	return false
}

// splitTopLevel splits a CREATE TABLE body on commas at paren depth zero,
// keeping each column definition and table constraint whole.
func splitTopLevel(body string) []string {
	var out []string
	depth := 0
	inString := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	return append(out, body[start:])
}
