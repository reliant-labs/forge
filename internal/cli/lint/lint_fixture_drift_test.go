package lint

import (
	"bytes"
	"strings"
	"testing"
)

// The birth schema from the dogfood run that motivated this check, reduced
// to the three tables whose fixtures broke. At birth, total_cents is an
// ordinary column and nothing is unique beyond the primary keys — which is
// exactly the schema the lifecycle test's seed block was written from.
const estimatesBirthSQL = `
CREATE TABLE estimates (
    id TEXT PRIMARY KEY,
    subtotal_cents BIGINT NOT NULL DEFAULT 0,
    tax_cents BIGINT NOT NULL DEFAULT 0,
    total_cents BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    estimate_id TEXT NOT NULL REFERENCES estimates (id),
    title TEXT NOT NULL
);
`

// The later migration that made total_cents derived — the one change that
// turned every CRUD lifecycle test in two packages into a pq 428C9.
const totalCentsGeneratedSQL = `
ALTER TABLE estimates DROP COLUMN total_cents;
ALTER TABLE estimates ADD COLUMN total_cents BIGINT
    GENERATED ALWAYS AS (subtotal_cents + tax_cents) STORED NOT NULL;
`

// The later migration that made the estimate -> job edge one-to-one.
const jobEstimateUniqueSQL = `
ALTER TABLE jobs ADD CONSTRAINT jobs_estimate_id_key UNIQUE (estimate_id);
`

// staleFixtureGo is the scaffolded lifecycle test as forge wrote it BEFORE
// either migration: the estimates column list names total_cents, and the two
// jobs rows share an estimate_id.
//
// Written with explicit escapes rather than a raw literal because the file
// itself contains backticks — this is Go source embedding a SQL string.
const staleFixtureGo = "package handlers_test\n\n" +
	"// The estimates seed writes total_cents directly; see the factory for\n" +
	"// how the value is derived elsewhere.\n" +
	"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n" +
	"\tdb := crudTestDB(t)\n\n" +
	"\tif _, err := db.Exec(context.Background(), `\n" +
	"INSERT INTO \"estimates\" (\"id\", \"subtotal_cents\", \"tax_cents\", \"total_cents\") VALUES\n" +
	"    ('est-1', 1000, 80, 1080),\n" +
	"    ('est-2', 2000, 160, 2160);\n" +
	"INSERT INTO \"jobs\" (\"id\", \"estimate_id\", \"title\") VALUES\n" +
	"    ('job-1', 'est-1', 'sample_title_1'),\n" +
	"    ('job-2', 'est-1', 'sample_title_2');\n" +
	"`); err != nil {\n" +
	"\t\tt.Fatalf(\"seed parent rows: %v\", err)\n" +
	"\t}\n" +
	"}\n"

// newEstimateProject lays down the project shape every direction of these
// tests shares: the birth migrations, and the stale lifecycle test.
func newEstimateProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_create_estimates.up.sql", estimatesBirthSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go", staleFixtureGo)
	return root
}

// findingsOfKind filters to one check's findings, so each test speaks about
// the check it is pinning even when the fixture trips both.
func findingsOfKind(findings []fixtureDriftFinding, kind fixtureDriftKind) []fixtureDriftFinding {
	var out []fixtureDriftFinding
	for _, f := range findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// ── Check 1: an INSERT naming a now-GENERATED column ──────────────────────

// TestFixtureDrift_GeneratedColumnInInsert is the reproduction. With
// total_cents now GENERATED ALWAYS, the scaffolded column list names a
// column postgres refuses to accept, and the check must say so — naming the
// file, the line, the column, and the migration that changed it.
//
// The NEGATIVE CONTROL is TestFixtureDrift_PlainColumnNoFinding below: the
// same file, the same column list, without the later migration.
func TestFixtureDrift_GeneratedColumnInInsert(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240301000000_total_cents_generated.up.sql", totalCentsGeneratedSQL)

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	findings := findingsOfKind(all, driftGeneratedColumn)
	if len(findings) != 1 {
		t.Fatalf("got %d generated-column findings, want 1\n%+v", len(findings), findings)
	}

	f := findings[0]
	if f.Table != "estimates" || f.Column != "total_cents" {
		t.Errorf("finding targets %s.%s, want estimates.total_cents", f.Table, f.Column)
	}
	if f.File != "internal/handlers/estimates/handlers_crud_test.go" {
		t.Errorf("File = %q, want the handler test path", f.File)
	}
	// The line must point at the column list, which is the text to edit.
	wantLine := lineOf(staleFixtureGo, strings.Index(staleFixtureGo, `"total_cents"`))
	if f.Line != wantLine {
		t.Errorf("Line = %d, want %d (the INSERT column list)", f.Line, wantLine)
	}
	// Attribution to the migration that made the column generated is the
	// fact that turns "wrong fixture" into "fixture older than this change".
	if !strings.Contains(f.DeclaredIn, "20240301000000_total_cents_generated") {
		t.Errorf("DeclaredIn = %q, want the later migration", f.DeclaredIn)
	}
	if !strings.Contains(f.Expression, "subtotal_cents") {
		t.Errorf("Expression = %q, want the GENERATED ALWAYS AS body", f.Expression)
	}

	if f.ruleID() != fixtureDriftRuleGeneratedColumn {
		t.Errorf("ruleID = %q, want %q", f.ruleID(), fixtureDriftRuleGeneratedColumn)
	}
	hint := fixtureDriftFixHint(f)
	for _, want := range []string{
		"GENERATED ALWAYS",
		"428C9",
		"total_cents",
		"yours",
		"factories_gen_test.go",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("fix hint missing %q:\n%s", want, hint)
		}
	}
}

// TestFixtureDrift_PlainColumnNoFinding is the negative control for the case
// above. Same file, same column list, WITHOUT the migration that makes the
// column generated. An ordinary column accepts an explicit value, so there
// is nothing to report — and a check that fired here would be flagging the
// NAME `total_cents` rather than a real schema conflict.
func TestFixtureDrift_PlainColumnNoFinding(t *testing.T) {
	root := newEstimateProject(t)

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftGeneratedColumn); len(fs) != 0 {
		t.Fatalf("got %d generated-column findings on an unchanged schema, want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_FixtureOmittingGeneratedColumnIsClean pins the CORRECT
// state — the one a user reaches after acting on a finding, and the one the
// regenerated factory already emits. The column is generated and the fixture
// does not name it, which is precisely right; firing here would tell the
// author to undo the fix.
func TestFixtureDrift_FixtureOmittingGeneratedColumnIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240301000000_total_cents_generated.up.sql", totalCentsGeneratedSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"estimates\" (\"id\", \"subtotal_cents\", \"tax_cents\") VALUES\n"+
			"    ('est-1', 1000, 80);\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatalf(\"seed parent rows: %v\", err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings on a correctly-updated fixture, want 0\n%+v", len(all), all)
	}
}

// TestFixtureDrift_ColumnNameInCommentOrStringIsClean pins the exclusion that
// makes the check structural rather than textual. The generated column's name
// appears in a Go comment, in a t.Fatalf message, and in a WHERE clause —
// but never in an INSERT column list, which is the only place this rule
// looks. A grep-shaped check would report all three.
func TestFixtureDrift_ColumnNameInCommentOrStringIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240301000000_total_cents_generated.up.sql", totalCentsGeneratedSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"// total_cents is derived by the database; do not seed it here.\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"estimates\" (\"id\", \"subtotal_cents\") VALUES ('est-1', 1000);\n"+
			"SELECT total_cents FROM \"estimates\" WHERE total_cents > 0;\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatalf(\"seed failed, check total_cents: %v\", err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings from a comment/string/WHERE mention, want 0\n%+v", len(all), all)
	}
}

// TestFixtureDrift_UntouchedTableIsClean pins the table-scoping exclusion. A
// generated column on a table no fixture inserts into produces nothing, and a
// same-named column on the table the fixture DOES touch is a different
// column — so the check must key on table AND column, not on column alone.
func TestFixtureDrift_UntouchedTableIsClean(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_create.up.sql", `
CREATE TABLE invoices (
    id TEXT PRIMARY KEY,
    total_cents BIGINT GENERATED ALWAYS AS (1) STORED
);
CREATE TABLE estimates (
    id TEXT PRIMARY KEY,
    total_cents BIGINT NOT NULL DEFAULT 0
);
`)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"estimates\" (\"id\", \"total_cents\") VALUES ('est-1', 1080);\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings for a generated column on an untouched table, want 0\n%+v", len(all), all)
	}
}

// TestFixtureDrift_NoScaffoldedTestsIsClean pins the lane's applicability. A
// project with migrations but none of forge's scaffold-once lifecycle tests
// has nothing for this rule to speak about, and a lane that does not apply is
// not a gap — it must be silent, not an error.
func TestFixtureDrift_NoScaffoldedTestsIsClean(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_create.up.sql", estimatesBirthSQL)
	writeFile(t, root, "db/migrations/20240301000000_gen.up.sql", totalCentsGeneratedSQL)

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings in a project with no lifecycle tests, want 0\n%+v", len(all), all)
	}
}

// TestFixtureDrift_NoMigrationsIsClean pins the other half of applicability:
// a project whose migrations directory does not exist yet yields silence
// rather than an error.
func TestFixtureDrift_NoMigrationsIsClean(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go", staleFixtureGo)

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings with no migrations, want 0\n%+v", len(all), all)
	}
}

// TestFixtureDrift_CommentedOutInsertIsClean pins the comment-blanking that
// the sibling rules depend on. A commented-out INSERT is not a statement, and
// reporting one would flag a fixture that never runs.
func TestFixtureDrift_CommentedOutInsertIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240301000000_total_cents_generated.up.sql", totalCentsGeneratedSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"-- INSERT INTO \"estimates\" (\"id\", \"total_cents\") VALUES ('est-1', 1080);\n"+
			"INSERT INTO \"estimates\" (\"id\", \"subtotal_cents\") VALUES ('est-1', 1000);\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings from a commented-out INSERT, want 0\n%+v", len(all), all)
	}
}

// ── Check 2: repeated literals in a now-UNIQUE column ─────────────────────

// TestFixtureDrift_DuplicateValueInNowUniqueColumn is the second
// reproduction: two scaffolded job rows share an estimate_id, which was legal
// until the edge became one-to-one.
func TestFixtureDrift_DuplicateValueInNowUniqueColumn(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	findings := findingsOfKind(all, driftDuplicateUnique)
	// One finding, not two: the FIRST write is legal, so only the colliding
	// row is reported.
	if len(findings) != 1 {
		t.Fatalf("got %d duplicate-unique findings, want 1 (the second row only)\n%+v", len(findings), findings)
	}

	f := findings[0]
	if f.Table != "jobs" || f.Column != "estimate_id" {
		t.Errorf("finding targets %s.%s, want jobs.estimate_id", f.Table, f.Column)
	}
	if f.Constraint != "jobs_estimate_id_key" {
		t.Errorf("Constraint = %q, want jobs_estimate_id_key", f.Constraint)
	}
	if !strings.Contains(f.DeclaredIn, "20240302000000_job_estimate_unique") {
		t.Errorf("DeclaredIn = %q, want the later unique migration", f.DeclaredIn)
	}
	// The line must point at the SECOND row, the one postgres rejects.
	wantLine := lineOf(staleFixtureGo, strings.Index(staleFixtureGo, "'job-2'"))
	if f.Line != wantLine {
		t.Errorf("Line = %d, want %d (the colliding row)", f.Line, wantLine)
	}
	if f.ruleID() != fixtureDriftRuleDuplicateUnique {
		t.Errorf("ruleID = %q, want %q", f.ruleID(), fixtureDriftRuleDuplicateUnique)
	}
	hint := fixtureDriftFixHint(f)
	for _, want := range []string{"jobs_estimate_id_key", "duplicate key value", "yours", "ON CONFLICT"} {
		if !strings.Contains(hint, want) {
			t.Errorf("fix hint missing %q:\n%s", want, hint)
		}
	}
}

// TestFixtureDrift_DuplicateWithoutUniqueIsClean is the negative control for
// the case above: the same two rows sharing an estimate_id, with no UNIQUE
// constraint. A non-unique column accepts repeats, and a check that fired
// here would be flagging ordinary one-to-many seed data.
func TestFixtureDrift_DuplicateWithoutUniqueIsClean(t *testing.T) {
	root := newEstimateProject(t)

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d duplicate findings with no UNIQUE constraint, want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_DistinctValuesInUniqueColumnIsClean pins the other correct
// state: the constraint exists and the fixture already honours it.
func TestFixtureDrift_DistinctValuesInUniqueColumnIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"jobs\" (\"id\", \"estimate_id\") VALUES\n"+
			"    ('job-1', 'est-1'),\n"+
			"    ('job-2', 'est-2');\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d findings on distinct unique values, want 0\n%+v", len(all), all)
	}
}

// TestFixtureDrift_CompositeUniqueIsClean pins the exclusion that keeps
// check 2 sound. `UNIQUE (estimate_id, title)` forbids repeated PAIRS, not
// repeated estimate_ids — the fixture below is legal, and a per-column
// reading of the constraint would call it broken.
func TestFixtureDrift_CompositeUniqueIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_composite.up.sql",
		"ALTER TABLE jobs ADD CONSTRAINT jobs_estimate_title_key UNIQUE (estimate_id, title);\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings on a COMPOSITE unique, want 0 (only the pair must be distinct)\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_PartialUniqueIndexIsClean pins the partial-index
// exclusion: the predicate decides whether the duplicate is legal, and this
// parser does not evaluate predicates.
func TestFixtureDrift_PartialUniqueIndexIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_partial.up.sql",
		"CREATE UNIQUE INDEX jobs_active_estimate_idx ON jobs (estimate_id) WHERE deleted_at IS NULL;\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings on a PARTIAL unique index, want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_OnConflictSuppresses pins the suppression: a statement
// that tells postgres to swallow this exact collision does not fail, so
// reporting it would be reporting working code.
func TestFixtureDrift_OnConflictSuppresses(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"jobs\" (\"id\", \"estimate_id\") VALUES\n"+
			"    ('job-1', 'est-1'),\n"+
			"    ('job-2', 'est-1')\n"+
			"ON CONFLICT (estimate_id) DO NOTHING;\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings despite ON CONFLICT (estimate_id), want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_BareOnConflictSuppressesStatement pins the conservative
// arm: a bare `ON CONFLICT DO NOTHING` names no target this parser can
// resolve, so the whole statement is left alone rather than guessed at.
func TestFixtureDrift_BareOnConflictSuppressesStatement(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"jobs\" (\"id\", \"estimate_id\") VALUES\n"+
			"    ('job-1', 'est-1'),\n"+
			"    ('job-2', 'est-1')\n"+
			"ON CONFLICT DO NOTHING;\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings despite a bare ON CONFLICT DO NOTHING, want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_RepeatedNullsAndOpaqueValuesAreClean pins the two literal
// kinds that cannot evidence a collision: postgres permits many NULLs in a
// unique column, and a value this parser cannot decode has no known identity.
func TestFixtureDrift_RepeatedNullsAndOpaqueValuesAreClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"jobs\" (\"id\", \"estimate_id\") VALUES\n"+
			"    ('job-1', NULL),\n"+
			"    ('job-2', NULL),\n"+
			"    ('job-3', gen_random_uuid()),\n"+
			"    ('job-4', gen_random_uuid());\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings on repeated NULLs / opaque calls, want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_DuplicateAcrossStatementsIsClean pins the within-statement
// restriction. Two separate INSERTs writing the same value may be perfectly
// legal — the block between them can delete, truncate or upsert — and
// reasoning across statements would mean modelling all of that.
func TestFixtureDrift_DuplicateAcrossStatementsIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)
	writeFile(t, root, "internal/handlers/estimates/handlers_crud_test.go",
		"package handlers_test\n\n"+
			"func TestCRUD_Estimate_Lifecycle(t *testing.T) {\n"+
			"\tif _, err := db.Exec(context.Background(), `\n"+
			"INSERT INTO \"jobs\" (\"id\", \"estimate_id\") VALUES ('job-1', 'est-1');\n"+
			"DELETE FROM \"jobs\";\n"+
			"INSERT INTO \"jobs\" (\"id\", \"estimate_id\") VALUES ('job-2', 'est-1');\n"+
			"`); err != nil {\n"+
			"\t\tt.Fatal(err)\n"+
			"\t}\n"+
			"}\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings across separate statements, want 0\n%+v", len(fs), fs)
	}
}

// TestFixtureDrift_DroppedUniqueIsClean pins the replay. A constraint added
// and then dropped is not part of the live schema, and reporting against it
// would flag a fixture the current database accepts.
func TestFixtureDrift_DroppedUniqueIsClean(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)
	writeFile(t, root, "db/migrations/20240303000000_drop_unique.up.sql",
		"ALTER TABLE jobs DROP CONSTRAINT jobs_estimate_id_key;\n")

	all, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}
	if fs := findingsOfKind(all, driftDuplicateUnique); len(fs) != 0 {
		t.Fatalf("got %d findings for a DROPPED unique constraint, want 0\n%+v", len(fs), fs)
	}
}

// TestMigrationColumns_DropThenRecreateInOneFile pins the statement-ORDER
// replay in applyMigrationColumns, a shared helper the read-only-fields rule
// also depends on.
//
// Postgres has no ALTER that makes an existing column generated, so
// drop-then-recreate in one migration is the ordinary way to do it — and it
// is exactly the migration that motivated this whole lane. Grouping every
// ADD before every DROP (the previous behaviour) deleted the column
// outright, so the replay claimed a column the database really has does not
// exist. Every caller reads that as "unresolved" and stays silent, which is
// indistinguishable from a clean project.
func TestMigrationColumns_DropThenRecreateInOneFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_create.up.sql", estimatesBirthSQL)
	writeFile(t, root, "db/migrations/20240301000000_generated.up.sql", totalCentsGeneratedSQL)

	columns, err := readOnlyColumnsFromMigrations(root + "/db/migrations")
	if err != nil {
		t.Fatalf("readOnlyColumnsFromMigrations: %v", err)
	}
	col, ok := columns["estimates"]["total_cents"]
	if !ok {
		t.Fatal("estimates.total_cents missing after drop-then-recreate — " +
			"the ADD was applied before the DROP in the same file")
	}
	if !col.Generated {
		t.Errorf("estimates.total_cents Generated = false, want true\n%+v", col)
	}
}

// ── Report shape ──────────────────────────────────────────────────────────

// TestFormatFixtureDrift_Clean pins the success line, so a user who runs the
// lane on a healthy project sees that it ran.
func TestFormatFixtureDrift_Clean(t *testing.T) {
	var buf bytes.Buffer
	formatFixtureDrift(&buf, nil)
	if !strings.Contains(buf.String(), "fixture-drift clean") {
		t.Errorf("clean report = %q, want a fixture-drift clean line", buf.String())
	}
}

// TestFormatFixtureDrift_WarnsWithoutGating pins the severity contract: the
// report must mark findings as warnings and say plainly that the build is not
// failing, matching its crud-fixtures and guarded-fields neighbours.
func TestFormatFixtureDrift_WarnsWithoutGating(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240301000000_total_cents_generated.up.sql", totalCentsGeneratedSQL)
	findings, err := collectFixtureDriftFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftFindings: %v", err)
	}

	var buf bytes.Buffer
	formatFixtureDrift(&buf, findings)
	out := buf.String()
	for _, want := range []string{"⚠", fixtureDriftRuleGeneratedColumn, "warnings only"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "❌") {
		t.Errorf("advisory lane must not render a gating ❌:\n%s", out)
	}
}

// TestFixtureDriftJSON_SeverityIsWarning pins the JSON contract, which is
// what CI and other tools read. Severity warning, and the rule id present on
// every finding.
func TestFixtureDriftJSON_SeverityIsWarning(t *testing.T) {
	root := newEstimateProject(t)
	writeFile(t, root, "db/migrations/20240301000000_total_cents_generated.up.sql", totalCentsGeneratedSQL)
	writeFile(t, root, "db/migrations/20240302000000_job_estimate_unique.up.sql", jobEstimateUniqueSQL)

	findings, err := collectFixtureDriftJSONAt(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectFixtureDriftJSONAt: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d JSON findings, want 2 (one per check)\n%+v", len(findings), findings)
	}
	rules := map[string]bool{}
	for _, f := range findings {
		if f.Severity != lintSevWarning {
			t.Errorf("finding %s severity = %q, want %q", f.Rule, f.Severity, lintSevWarning)
		}
		if f.FixHint == "" {
			t.Errorf("finding %s has no fix hint", f.Rule)
		}
		rules[f.Rule] = true
	}
	for _, want := range []string{fixtureDriftRuleGeneratedColumn, fixtureDriftRuleDuplicateUnique} {
		if !rules[want] {
			t.Errorf("JSON findings missing rule %q, got %v", want, rules)
		}
	}
}
