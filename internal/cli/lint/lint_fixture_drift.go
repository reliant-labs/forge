// File: internal/cli/lint/lint_fixture_drift.go
//
// fixture-drift — `forge lint --fixture-drift`.
//
// The sibling of crud-fixtures, for the two ways a scaffold-once fixture
// ages out of its schema that a foreign-key check cannot see.
//
// internal/handlers/<svc>/handlers_crud_test.go is scaffold-once: forge
// writes it exactly once, from the schema as it stands at that moment, and
// never touches it again. The embedded seed block therefore carries a
// LITERAL column list and literal rows:
//
//	INSERT INTO "estimates" ("id", "subtotal_cents", "tax_cents", "total_cents") VALUES ...
//
// The schema keeps moving. Two later changes break that fixture outright,
// and both were measured in a dogfood run:
//
//  1. A column becomes `GENERATED ALWAYS AS (...) STORED`. Postgres then
//     REFUSES any INSERT that names it at all:
//
//     pq: cannot insert a non-DEFAULT value into column "total_cents" (428C9)
//
//  2. A UNIQUE constraint is added to a column the fixture seeds with the
//     same literal on more than one row — the ordinary shape for a
//     one-to-one edge the generator scaffolded as one-to-many:
//
//     pq: duplicate key value violates unique constraint "jobs_estimate_id_key"
//
// Both surface as a pq error inside test SETUP, which names postgres's
// complaint and nothing else. Nothing connects either to the migration that
// caused it, and nothing points at the fixture — so the failure reads like a
// broken harness. Worse, they queue: each one only becomes visible after the
// previous is fixed, so one schema change cost six sequential debugging
// rounds to unwind.
//
// ── Why a lint and not a generator change ────────────────────────────────
//
// Forge demonstrably KNOWS the answer. The regenerated sibling
// factories_gen_test.go derives its columns from the live schema and
// correctly omits the generated column. The knowledge simply never reaches
// the scaffold-once file, because forge does not write that file again.
//
// That is not a bug in scaffold-once; it is the model working as designed.
// The file is the user's from line one, and a generator that reached back in
// and rewrote a user's test would be a far worse defect than the one it
// repaired. So this check does not fix anything. It detects the drift and
// names the four facts the pq error withheld: the file, the line, the
// column, and the migration that changed it.
//
// It is also why internal/codegen/crud_fixture_guard.go cannot cover this.
// That guard verifies fixtures against a shadow database at BIRTH; the
// scaffold-once ledger check returns before it runs on every later generate,
// including the one right after the offending migration. Birth checks cannot
// see aging problems.
//
// ── What it reads ────────────────────────────────────────────────────────
//
// Purely textual, no database — the same grade as crud-fixtures and
// read-only-fields, and for the same reason: this defect lives in a
// half-migrated project, which is exactly where a lint that needs a live
// postgres cannot run. The final schema is replayed out of db/migrations in
// lexical order so a later ALTER wins, and the fixtures are read out of each
// handlers_crud_test.go.
//
// ── Check 1: an INSERT naming a GENERATED ALWAYS column ──────────────────
//
// A finding is a column name inside an INSERT's parenthesized COLUMN LIST
// whose live definition is `GENERATED ALWAYS AS (...) STORED`. Postgres
// rejects this unconditionally, so the check needs no reasoning about
// values — the presence of the name is the defect.
//
// Only the column list is examined, never the file at large. That is what
// makes a mention of the column in a Go comment, in a t.Fatalf string, or in
// a WHERE clause structurally incapable of producing a finding.
//
// ── Check 2: repeated values in a now-UNIQUE column ──────────────────────
//
// A finding is a literal value written into a single-column-UNIQUE column on
// two or more rows OF ONE INSERT STATEMENT. The restrictions are what make
// it sound rather than merely plausible, and each is pinned by a test:
//
//   - WITHIN one statement only. Reasoning across statements would need to
//     model what else the block deleted, truncated or upserted first; a
//     single statement that writes one value twice into a unique column
//     fails on its own, with no context required.
//   - SINGLE-column uniques only. A composite `UNIQUE (a, b)` forbids
//     repeated PAIRS, not repeated values in either column, so reporting per
//     column would flag correct fixtures.
//   - PRIMARY KEY is deliberately NOT treated as a unique for this purpose.
//     It is one, but the generator emits distinct ids by construction, so
//     including it buys nothing and widens the surface.
//   - Partial unique indexes (`CREATE UNIQUE INDEX ... WHERE ...`) are
//     skipped: the predicate decides whether the duplicate is legal, and
//     this parser does not evaluate predicates.
//   - `ON CONFLICT` suppresses. A conflict target naming the column means
//     postgres swallows exactly this collision; a bare `ON CONFLICT DO
//     NOTHING`, or one naming a constraint rather than columns, suppresses
//     the whole statement because the target cannot be resolved from text.
//   - NULL never collides — postgres permits many NULLs in a unique column —
//     and a value this parser cannot fully decode (a function call, an
//     expression) is not evidence of anything.
//
// ── Severity: warning, never gating ──────────────────────────────────────
//
// Matching crud-fixtures and guarded-fields. The fixture is genuinely broken
// and its test genuinely fails, but the remedy is an edit to a file forge
// does not own and may legitimately be mid-edit. Failing the whole lint run
// over it would be a generator holding a user's file hostage — and a noisy
// gating rule is how a check earns a blanket disable, which costs every
// future finding it would have reported.

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

// fixtureDriftKind distinguishes the two checks. They share a lane, a file
// and a report because they are one question — "has this scaffold-once
// fixture aged out of the schema?" — and a reader who hits one is about to
// hit the other.
type fixtureDriftKind int

const (
	// driftGeneratedColumn is an INSERT column list naming a column that is
	// now GENERATED ALWAYS. Postgres rejects it outright (SQLSTATE 428C9).
	driftGeneratedColumn fixtureDriftKind = iota
	// driftDuplicateUnique is one INSERT writing the same literal twice into
	// a column that is now UNIQUE.
	driftDuplicateUnique
)

// fixtureDriftRuleGeneratedColumn and fixtureDriftRuleDuplicateUnique are the
// stable rule ids, reported in text mode and in `forge lint --json`.
const (
	fixtureDriftRuleGeneratedColumn = "forge-fixture-generated-column"
	fixtureDriftRuleDuplicateUnique = "forge-fixture-duplicate-unique"
)

// fixtureDriftFinding is one stale fixture. File is root-relative and Line
// is 1-indexed, pointing at the offending column name (check 1) or at the
// repeated value (check 2) rather than at the statement — the statement is
// what the pq error already effectively named.
type fixtureDriftFinding struct {
	Kind   fixtureDriftKind
	File   string
	Line   int
	Table  string
	Column string

	// DeclaredIn is the root-relative migration that made the change, "" when
	// it could not be attributed. It is the fact that turns "this fixture is
	// wrong" into "this fixture predates this migration".
	DeclaredIn string

	// Expression is the generated column's `GENERATED ALWAYS AS (...)` body,
	// carried so the message can show what postgres computes instead.
	// Check 1 only.
	Expression string

	// Value is the repeated literal, and Constraint the unique constraint it
	// violates. Check 2 only.
	Value      string
	Constraint string
}

// ruleID returns the rule id for this finding's check.
func (f fixtureDriftFinding) ruleID() string {
	if f.Kind == driftDuplicateUnique {
		return fixtureDriftRuleDuplicateUnique
	}
	return fixtureDriftRuleGeneratedColumn
}

// message is the one-line summary, shared by text mode and JSON.
func (f fixtureDriftFinding) message() string {
	if f.Kind == driftDuplicateUnique {
		return fmt.Sprintf("this INSERT writes %s into %s.%s on more than one row, and that column is UNIQUE",
			f.Value, f.Table, f.Column)
	}
	return fmt.Sprintf("this INSERT names %s.%s, which is GENERATED ALWAYS — postgres rejects the statement",
		f.Table, f.Column)
}

// fixtureDriftFixHint renders the remediation.
//
// Both arms state the ownership fact explicitly. The author was told forge
// would never touch this file again, so a finding about it has to say
// plainly that editing it is theirs to do and will not be reverted —
// otherwise the obvious next move is to re-run `forge generate` and conclude
// the lint is wrong when nothing changes.
func fixtureDriftFixHint(f fixtureDriftFinding) string {
	origin := ""
	if f.DeclaredIn != "" {
		origin = fmt.Sprintf(" (declared in %s)", f.DeclaredIn)
	}
	if f.Kind == driftDuplicateUnique {
		return fmt.Sprintf(
			"%s.%s is UNIQUE — constraint %s%s — but this INSERT writes %s in that column on more "+
				"than one row, so postgres rejects the whole statement with `duplicate key value "+
				"violates unique constraint %q`, in test setup, naming no fixture. These rows were "+
				"scaffolded when the column still allowed duplicates, and handlers_crud_test.go is "+
				"yours — written once, never regenerated — so `forge generate` cannot repair it. "+
				"Give each row a distinct %s, or drop the extra row if the edge is genuinely "+
				"one-to-one. If the collision is deliberate, add `ON CONFLICT (%s) DO NOTHING` to "+
				"the statement.",
			f.Table, f.Column, f.Constraint, origin, f.Value, f.Constraint, f.Column, f.Column)
	}
	expr := ""
	if e := strings.TrimSpace(f.Expression); e != "" {
		expr = fmt.Sprintf(" AS (%s)", e)
	}
	return fmt.Sprintf(
		"%s.%s is GENERATED ALWAYS%s STORED%s, so postgres computes it on every write and REFUSES "+
			"any INSERT that names it: `cannot insert a non-DEFAULT value into column %q` "+
			"(SQLSTATE 428C9). This column list was written from the schema as it stood when the "+
			"test was scaffolded, and handlers_crud_test.go is yours — written once, never "+
			"regenerated — so `forge generate` cannot repair it. Remove %q from the column list "+
			"and its value from every VALUES row in this statement. The regenerated sibling "+
			"factories_gen_test.go already derives this correctly and omits the column; it is the "+
			"reference for what this block should look like.",
		f.Table, f.Column, expr, origin, f.Column, f.Column)
}

// runFixtureDriftLint is the text-mode entry point.
func runFixtureDriftLint(cwd string, cfg *config.ProjectConfig) error {
	fmt.Println("Running fixture-drift lint...")
	findings, err := collectFixtureDriftFindings(cwd, migrationsDirFor(cfg))
	if err != nil {
		return err
	}
	formatFixtureDrift(os.Stdout, findings)
	return nil
}

// formatFixtureDrift writes the human report, matching the sibling advisory
// lanes: one success line when clean, one ⚠ block per finding otherwise.
func formatFixtureDrift(w io.Writer, findings []fixtureDriftFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  fixture-drift clean — no scaffolded seed block contradicts the current schema")
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ⚠ [%s] %s:%d\n", f.ruleID(), f.File, f.Line)
		_, _ = fmt.Fprintf(w, "      → %s\n", fixtureDriftFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d scaffolded fixture(s) that the current schema rejects.\n", len(findings))
	_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
}

// collectFixtureDriftFindings is the shared engine behind text mode and
// `forge lint --json`.
//
// A project with no migrations, or none of forge's scaffolded lifecycle
// tests, yields no findings rather than an error: both are ordinary states
// for a project this lane does not apply to, and a lane that does not apply
// is not a gap.
func collectFixtureDriftFindings(root, migrationsDir string) ([]fixtureDriftFinding, error) {
	if !filepath.IsAbs(migrationsDir) {
		migrationsDir = filepath.Join(root, migrationsDir)
	}
	testFiles, err := crudTestFiles(root)
	if err != nil {
		return nil, err
	}
	if len(testFiles) == 0 {
		return nil, nil
	}

	// The live column set answers check 1 (is this column GENERATED now?),
	// re-using the same replay the read-only rule depends on. The origins
	// pass is separate and additive because that replay carries no
	// attribution — see generatedColumnOrigins.
	columns, err := readOnlyColumnsFromMigrations(migrationsDir)
	if err != nil {
		return nil, err
	}
	origins, err := generatedColumnOrigins(root, migrationsDir)
	if err != nil {
		return nil, err
	}
	uniques, err := uniqueColumnsFromMigrations(root, migrationsDir)
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 && len(uniques) == 0 {
		return nil, nil
	}

	var findings []fixtureDriftFinding
	for _, path := range testFiles {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", path, rerr)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		findings = append(findings,
			driftedFixtures(filepath.ToSlash(rel), string(data), columns, origins, uniques)...)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// driftedFixtures runs both checks over one lifecycle test.
func driftedFixtures(
	relPath, content string,
	columns map[string]map[string]sqlColumn,
	origins map[string]map[string]generatedOrigin,
	uniques map[string]map[string]uniqueDecl,
) []fixtureDriftFinding {
	// Comments are blanked for the same reason crud-fixtures blanks them: a
	// commented-out INSERT is not a statement, and reading one would report a
	// fixture that does not run. Offsets are preserved in place so line
	// numbers still address the original text.
	inserts := parseFixtureInserts(blankSQLComments(content))

	var findings []fixtureDriftFinding
	for _, ins := range inserts {
		findings = append(findings, generatedColumnWrites(relPath, content, ins, columns, origins)...)
		findings = append(findings, duplicateUniqueWrites(relPath, content, ins, uniques)...)
	}
	return findings
}

// generatedColumnWrites reports every column in one INSERT's column list
// that is GENERATED ALWAYS in the live schema.
//
// Only the parenthesized column list is consulted. A column named anywhere
// else in the file — a Go comment, a t.Fatalf string, a WHERE clause — is
// structurally unreachable from here, which is what makes that whole class
// of false positive impossible rather than merely filtered.
func generatedColumnWrites(
	relPath, content string,
	ins fixtureInsert,
	columns map[string]map[string]sqlColumn,
	origins map[string]map[string]generatedOrigin,
) []fixtureDriftFinding {
	byName := columns[ins.table]
	if len(byName) == 0 {
		// A table this replay did not resolve is a gap in the parser, not
		// evidence about the fixture. Silence is the only sound answer.
		return nil
	}
	var findings []fixtureDriftFinding
	for _, col := range ins.cols {
		if !byName[col.name].Generated {
			continue
		}
		origin := origins[ins.table][col.name]
		findings = append(findings, fixtureDriftFinding{
			Kind:       driftGeneratedColumn,
			File:       relPath,
			Line:       lineOf(content, col.offset),
			Table:      ins.table,
			Column:     col.name,
			DeclaredIn: origin.declaredIn,
			Expression: origin.expression,
		})
	}
	return findings
}

// duplicateUniqueWrites reports every literal one INSERT writes more than
// once into a single-column-UNIQUE column.
//
// Only the SECOND and later occurrences are reported, one per extra row: the
// first write is legal, and flagging it too would make a two-row collision
// read as two independent defects.
func duplicateUniqueWrites(
	relPath, content string,
	ins fixtureInsert,
	uniques map[string]map[string]uniqueDecl,
) []fixtureDriftFinding {
	byName := uniques[ins.table]
	if len(byName) == 0 || ins.suppressesAllConflicts {
		return nil
	}
	var findings []fixtureDriftFinding
	for i, col := range ins.cols {
		decl, isUnique := byName[col.name]
		if !isUnique || ins.conflictTargets[col.name] {
			// An ON CONFLICT naming this column means postgres swallows
			// exactly this collision.
			continue
		}
		seen := map[string]bool{}
		for _, row := range ins.rows {
			if i >= len(row) {
				break
			}
			text, kind := decodeSQLLiteral(row[i].raw)
			// NULL never collides — postgres permits many NULLs in a unique
			// column — and an opaque value's identity is unknown here.
			if kind != literalPlain {
				continue
			}
			if !seen[text] {
				seen[text] = true
				continue
			}
			findings = append(findings, fixtureDriftFinding{
				Kind:       driftDuplicateUnique,
				File:       relPath,
				Line:       lineOf(content, row[i].offset),
				Table:      ins.table,
				Column:     col.name,
				Value:      strings.TrimSpace(row[i].raw),
				Constraint: decl.constraint,
				DeclaredIn: decl.declaredIn,
			})
		}
	}
	return findings
}

// ── Fixture INSERT parsing ────────────────────────────────────────────────

// fixtureColumn is one name in an INSERT's column list, with its absolute
// offset so a finding can point at the name rather than the statement.
type fixtureColumn struct {
	name   string
	offset int
}

// fixtureInsert is one INSERT ... VALUES statement in a lifecycle test.
//
// It carries what parseSeedInserts deliberately drops — per-column offsets
// and the ON CONFLICT clause — because this rule reports on the column list
// itself and must not speak about a collision postgres is told to swallow.
type fixtureInsert struct {
	table string
	cols  []fixtureColumn
	rows  [][]seedValue

	// conflictTargets is the column set named by `ON CONFLICT (a, b)`.
	conflictTargets map[string]bool
	// suppressesAllConflicts is true for a bare `ON CONFLICT DO NOTHING`, and
	// for `ON CONFLICT ON CONSTRAINT <name>` — a constraint this text parse
	// cannot resolve to columns. Both mean the statement's collisions cannot
	// be reasoned about from here, so the whole statement is left alone.
	suppressesAllConflicts bool
}

// onConflictRe captures the clause that follows the value tuples. Group 1 is
// the parenthesized conflict target when there is one.
var onConflictRe = regexp.MustCompile(`(?is)^\s*ON\s+CONFLICT\b\s*(?:\(([^)]*)\))?`)

// onConflictConstraintRe matches the constraint-named form, whose columns
// this parser cannot resolve.
var onConflictConstraintRe = regexp.MustCompile(`(?is)^\s*ON\s+CONFLICT\s+ON\s+CONSTRAINT\b`)

// parseFixtureInserts extracts every INSERT ... VALUES statement, its column
// list with offsets, its value tuples, and its conflict handling.
//
// content must already have its comments blanked IN PLACE, so offsets still
// address the original file.
func parseFixtureInserts(content string) []fixtureInsert {
	var out []fixtureInsert
	for _, m := range insertHeadRe.FindAllStringSubmatchIndex(content, -1) {
		ins := fixtureInsert{
			table:           normIdent(content[m[2]:m[3]]),
			cols:            splitColumnList(content[m[4]:m[5]], m[4]),
			conflictTargets: map[string]bool{},
		}
		rows, end := parseValueTuples(content, m[1])
		if len(rows) == 0 {
			continue
		}
		ins.rows = rows
		applyConflictClause(&ins, content[end:])
		out = append(out, ins)
	}
	return out
}

// splitColumnList splits an INSERT column list on commas, carrying each
// name's absolute offset. base is the offset of the list's first character.
func splitColumnList(list string, base int) []fixtureColumn {
	var out []fixtureColumn
	start := 0
	emit := func(end int) {
		part := list[start:end]
		name := normIdent(part)
		if name == "" {
			return
		}
		// Point at the identifier itself, not at the leading whitespace or
		// the opening quote the scaffold writes.
		off := base + start + len(part) - len(strings.TrimLeft(part, " \t\n\r\"`"))
		out = append(out, fixtureColumn{name: name, offset: off})
	}
	for i := 0; i < len(list); i++ {
		if list[i] == ',' {
			emit(i)
			start = i + 1
		}
	}
	emit(len(list))
	return out
}

// applyConflictClause reads the text immediately after the value tuples and
// records what ON CONFLICT suppresses. tail is everything from the end of the
// last tuple onward; only the statement's own clause (up to the terminating
// semicolon) is considered.
func applyConflictClause(ins *fixtureInsert, tail string) {
	if i := strings.IndexByte(tail, ';'); i >= 0 {
		tail = tail[:i]
	}
	if onConflictConstraintRe.MatchString(tail) {
		ins.suppressesAllConflicts = true
		return
	}
	m := onConflictRe.FindStringSubmatch(tail)
	if m == nil {
		return
	}
	targets := strings.TrimSpace(m[1])
	if targets == "" {
		// A bare `ON CONFLICT DO NOTHING` swallows every collision.
		ins.suppressesAllConflicts = true
		return
	}
	for _, c := range strings.Split(targets, ",") {
		if name := normIdent(c); name != "" {
			ins.conflictTargets[name] = true
		}
	}
}

// ── Schema facts out of migration text ────────────────────────────────────

// generatedOrigin attributes a GENERATED ALWAYS column to the migration that
// declared it, and carries the expression postgres computes.
type generatedOrigin struct {
	declaredIn string
	expression string
}

// generatedExprRe captures the body of `GENERATED ALWAYS AS (...) STORED`.
var generatedExprRe = regexp.MustCompile(`(?is)\bGENERATED\s+ALWAYS\s+AS\s*\((.*)\)\s*STORED\b`)

// generatedColumnOrigins records which migration last declared each column
// GENERATED, and with what expression.
//
// This is a separate, additive pass rather than a field on sqlColumn: the
// shared column replay answers "what is the schema now", which is the
// question check 1 actually asks, and it carries no per-column provenance.
// Attribution is a message-quality concern — a finding whose DeclaredIn is
// empty is still a correct finding — so it is kept out of the shared type
// where a wrong answer would reach two other rules.
func generatedColumnOrigins(root, migrationsDir string) (map[string]map[string]generatedOrigin, error) {
	out := map[string]map[string]generatedOrigin{}
	err := eachMigration(root, migrationsDir, func(relPath, content string) {
		record := func(table string, def string) {
			m := generatedExprRe.FindStringSubmatch(def)
			if m == nil {
				return
			}
			col, ok := parseColumnDef(def)
			if !ok {
				return
			}
			if out[table] == nil {
				out[table] = map[string]generatedOrigin{}
			}
			out[table][col.Name] = generatedOrigin{
				declaredIn: relPath,
				expression: strings.TrimSpace(m[1]),
			}
		}
		for _, m := range createTableRe.FindAllStringSubmatchIndex(content, -1) {
			table := normIdent(content[m[2]:m[3]])
			open := m[1] - 1 // the '(' the pattern ends on
			end, ok := matchParen(content, open)
			if !ok {
				continue
			}
			for _, part := range splitTopLevel(content[open+1 : end]) {
				record(table, part)
			}
		}
		for _, m := range alterAddColumnRe.FindAllStringSubmatch(content, -1) {
			record(normIdent(m[1]), m[2])
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// uniqueDecl is one single-column UNIQUE constraint, with the migration that
// declared it.
type uniqueDecl struct {
	table      string
	column     string
	constraint string
	declaredIn string
}

var (
	// alterAddUniqueRe matches `ALTER TABLE t ADD CONSTRAINT n UNIQUE (col)`.
	// The column group deliberately forbids a comma, so a composite UNIQUE
	// simply does not match — see the file header for why per-column
	// reporting of a composite constraint would be a false positive.
	alterAddUniqueRe = regexp.MustCompile(
		`(?is)\bALTER\s+TABLE\s+(?:ONLY\s+)?("?[\w.]+"?)\s+ADD\s+CONSTRAINT\s+("?\w+"?)\s+UNIQUE\s*\(\s*("?\w+"?)\s*\)`)
	// createUniqueIndexRe matches a single-column unique index. The trailing
	// group captures whatever follows, so a partial index (`... WHERE ...`)
	// can be excluded.
	createUniqueIndexRe = regexp.MustCompile(
		`(?is)\bCREATE\s+UNIQUE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?("?\w+"?)?\s*ON\s+(?:ONLY\s+)?("?[\w.]+"?)\s*(?:USING\s+\w+\s*)?\(\s*("?\w+"?)\s*\)([^;]*)`)
	// dropIndexRe retracts a unique index.
	dropIndexRe = regexp.MustCompile(`(?is)\bDROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?("?[\w.]+"?)`)
	// tableUniqueRe matches a table-level `[CONSTRAINT n] UNIQUE (col)` inside
	// a CREATE TABLE body. Single column only, for the same reason.
	tableUniqueRe = regexp.MustCompile(
		`(?is)^\s*(?:CONSTRAINT\s+("?\w+"?)\s+)?UNIQUE\s*\(\s*("?\w+"?)\s*\)\s*$`)
	// inlineUniqueRe matches a UNIQUE keyword inside a column definition.
	inlineUniqueRe = regexp.MustCompile(`(?is)\bUNIQUE\b`)
	// partialIndexRe detects the predicate that makes a unique index partial.
	partialIndexRe = regexp.MustCompile(`(?is)\bWHERE\b`)
)

// uniqueColumnsFromMigrations reads the live single-column UNIQUE
// constraints out of the project's migrations, indexed as uniques[table][column].
//
// Migrations are replayed in lexical order and DROP CONSTRAINT / DROP INDEX /
// DROP TABLE retract what earlier files added, so the result describes the
// schema as it stands after the last migration. Keyed by constraint name
// internally so a DROP retracts exactly what its ADD introduced — the same
// shape foreignKeysFromMigrations uses.
//
// PRIMARY KEY is deliberately absent: it is a unique constraint, but the
// generator emits distinct ids by construction, so including it widens the
// surface for no measured benefit.
func uniqueColumnsFromMigrations(root, migrationsDir string) (map[string]map[string]uniqueDecl, error) {
	live := map[string]uniqueDecl{}
	err := eachMigration(root, migrationsDir, func(relPath, content string) {
		for _, m := range createTableRe.FindAllStringSubmatchIndex(content, -1) {
			table := normIdent(content[m[2]:m[3]])
			open := m[1] - 1
			end, ok := matchParen(content, open)
			if !ok {
				continue
			}
			for _, part := range splitTopLevel(content[open+1 : end]) {
				if decl, found := createTableUnique(table, part); found {
					decl.declaredIn = relPath
					live[decl.constraint] = decl
				}
			}
		}
		for _, m := range alterAddUniqueRe.FindAllStringSubmatch(content, -1) {
			decl := uniqueDecl{
				table:      normIdent(m[1]),
				constraint: normIdent(m[2]),
				column:     normIdent(m[3]),
				declaredIn: relPath,
			}
			live[decl.constraint] = decl
		}
		for _, m := range createUniqueIndexRe.FindAllStringSubmatch(content, -1) {
			if partialIndexRe.MatchString(m[4]) {
				// A partial unique index forbids duplicates only where its
				// predicate holds, and this parser does not evaluate
				// predicates. Silence is the only sound answer.
				continue
			}
			decl := uniqueDecl{
				table:      normIdent(m[2]),
				constraint: normIdent(m[1]),
				column:     normIdent(m[3]),
				declaredIn: relPath,
			}
			if decl.constraint == "" {
				decl.constraint = fmt.Sprintf("%s_%s_key", decl.table, decl.column)
			}
			live[decl.constraint] = decl
		}
		for _, m := range alterDropConstraintRe.FindAllStringSubmatch(content, -1) {
			delete(live, normIdent(m[2]))
		}
		for _, m := range dropIndexRe.FindAllStringSubmatch(content, -1) {
			delete(live, normIdent(m[1]))
		}
		for _, m := range alterDropColumnRe.FindAllStringSubmatch(content, -1) {
			table, column := normIdent(m[1]), normIdent(m[2])
			for name, decl := range live {
				if decl.table == table && decl.column == column {
					delete(live, name)
				}
			}
		}
		for _, m := range dropTableRe.FindAllStringSubmatch(content, -1) {
			dropped := normIdent(m[1])
			for name, decl := range live {
				if decl.table == dropped {
					delete(live, name)
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}

	out := map[string]map[string]uniqueDecl{}
	for _, decl := range live {
		if out[decl.table] == nil {
			out[decl.table] = map[string]uniqueDecl{}
		}
		out[decl.table][decl.column] = decl
	}
	return out, nil
}

// createTableUnique reads one CREATE TABLE body part as a single-column
// UNIQUE declaration, either as a table constraint or inline on the column.
//
// An unnamed constraint is given the name postgres derives for it,
// `<table>_<column>_key`, so a later DROP CONSTRAINT naming it retracts the
// right entry.
func createTableUnique(table, part string) (uniqueDecl, bool) {
	if m := tableUniqueRe.FindStringSubmatch(part); m != nil {
		decl := uniqueDecl{table: table, constraint: normIdent(m[1]), column: normIdent(m[2])}
		if decl.constraint == "" {
			decl.constraint = fmt.Sprintf("%s_%s_key", decl.table, decl.column)
		}
		return decl, true
	}
	col, ok := parseColumnDef(part)
	if !ok || !inlineUniqueRe.MatchString(part) {
		return uniqueDecl{}, false
	}
	return uniqueDecl{
		table:      table,
		column:     col.Name,
		constraint: fmt.Sprintf("%s_%s_key", table, col.Name),
	}, true
}

// eachMigration walks the project's .up.sql files in lexical order — the
// order the migrator applies them — and hands each one's root-relative path
// and comment-blanked content to fn.
//
// Comments are blanked because forge's own birth migration writes the
// constraints it CANNOT yet apply as commented-out suggestions. Reading
// those would build a schema that does not exist and report fixtures against
// constraints nobody has applied.
func eachMigration(root, migrationsDir string, fn func(relPath, content string)) error {
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return nil
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
		return fmt.Errorf("walk %s: %w", migrationsDir, err)
	}
	sort.Strings(files)

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		fn(filepath.ToSlash(rel), blankSQLComments(string(data)))
	}
	return nil
}
