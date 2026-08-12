package lint

import (
	"path/filepath"
	"strings"
	"testing"
)

// The birth migrations from the run that motivated this check, reduced to
// the cycle that produced it: crews.foreman_id -> crew_members.id and
// crew_members.crew_id -> crews.id. Neither table's own birth migration can
// declare its side, because the other table does not exist yet — which is
// exactly why the constraint arrives later and the fixtures predate it.
const crewsBirthSQL = `
CREATE TABLE crews (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    foreman_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The FOREIGN KEY for foreman_id cannot be applied here: crew_members does
-- not exist yet. It is added by that table's own birth migration.
-- ALTER TABLE crews ADD CONSTRAINT crews_foreman_id_fkey FOREIGN KEY (foreman_id) REFERENCES crew_members (id);
CREATE INDEX crews_foreman_id_idx ON crews (foreman_id);
`

const crewMembersBirthSQL = `
CREATE TABLE crew_members (
    id TEXT PRIMARY KEY,
    crew_id TEXT NOT NULL REFERENCES crews (id),
    full_name TEXT NOT NULL
);
`

// The later migration that closes the cycle — the one change that turned
// nine passing tests into nine pq failures.
const foremanFKSQL = `
ALTER TABLE crews ADD CONSTRAINT crews_foreman_id_fkey FOREIGN KEY (foreman_id) REFERENCES crew_members (id);
`

// The scaffolded lifecycle test as forge wrote it BEFORE the FK existed:
// crews.foreman_id had no constraint, so the generator had no parent to
// point at and emitted the synthetic placeholder.
const staleCrudTestGo = "package handlers_test\n\n" +
	"func TestCRUD_Crew_Lifecycle(t *testing.T) {\n" +
	"\tdb := crudTestDB(t)\n\n" +
	"\tif _, err := db.Exec(context.Background(), `\n" +
	"INSERT INTO \"crew_members\" (\"id\", \"crew_id\", \"full_name\") VALUES\n" +
	"    ('11111111-1111-4111-8111-111111111111', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'sample_full_name_1'),\n" +
	"    ('22222222-2222-4222-8222-222222222222', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'sample_full_name_2')\n" +
	"ON CONFLICT (id) DO NOTHING;\n" +
	"INSERT INTO \"crews\" (\"id\", \"name\", \"foreman_id\") VALUES\n" +
	"    ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'sample_name_1', 'sample_foreman_id_1'),\n" +
	"    ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'sample_name_2', 'sample_foreman_id_2')\n" +
	"ON CONFLICT (id) DO NOTHING;\n" +
	"`); err != nil {\n" +
	"\t\tt.Fatalf(\"seed parent rows: %v\", err)\n" +
	"\t}\n" +
	"}\n"

// newCrewProject lays down the project shape both directions of the test
// share: migrations, and the stale lifecycle test.
func newCrewProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_create_crews.up.sql", crewsBirthSQL)
	writeFile(t, root, "db/migrations/20240102000000_create_crew_members.up.sql", crewMembersBirthSQL)
	writeFile(t, root, "internal/handlers/crews/handlers_crud_test.go", staleCrudTestGo)
	return root
}

// TestCrudFixtures_LaterFKMakesSeedValueDangling is the reproduction. With
// the foreman FK present, the placeholder in crews.foreman_id references no
// crew_members row and the check must say so — naming the column, the
// constraint, the migration that introduced it, and the line to edit.
//
// The NEGATIVE CONTROL is TestCrudFixtures_NoFKNoFinding below: the same
// file, the same placeholder, without the later migration. That pair is
// what proves the check keys on the constraint rather than on the spelling
// of the value.
func TestCrudFixtures_LaterFKMakesSeedValueDangling(t *testing.T) {
	root := newCrewProject(t)
	writeFile(t, root, "db/migrations/20240301000000_add_crew_foreman_fk.up.sql", foremanFKSQL)

	findings, err := collectCrudFixtureFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectCrudFixtureFindings: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2 (one per seeded crews row)\n%+v", len(findings), findings)
	}

	f := findings[0]
	if f.Table != "crews" || f.Column != "foreman_id" {
		t.Errorf("finding targets %s.%s, want crews.foreman_id", f.Table, f.Column)
	}
	if f.RefTable != "crew_members" || f.RefColumn != "id" {
		t.Errorf("finding references %s.%s, want crew_members.id", f.RefTable, f.RefColumn)
	}
	if f.Constraint != "crews_foreman_id_fkey" {
		t.Errorf("constraint = %q, want crews_foreman_id_fkey", f.Constraint)
	}
	// Attribution to the migration that ADDED the constraint is the fact
	// that turns "wrong fixture" into "fixture older than this constraint".
	if !strings.Contains(f.DeclaredIn, "20240301000000_add_crew_foreman_fk") {
		t.Errorf("DeclaredIn = %q, want the later FK migration", f.DeclaredIn)
	}
	if f.File != "internal/handlers/crews/handlers_crud_test.go" {
		t.Errorf("File = %q, want the handler test path", f.File)
	}
	// The line must point at the offending row, not at the statement.
	wantLine := lineOf(staleCrudTestGo, strings.Index(staleCrudTestGo, "'sample_foreman_id_1'"))
	if f.Line != wantLine {
		t.Errorf("Line = %d, want %d (the row carrying the stale value)", f.Line, wantLine)
	}
	if !strings.Contains(f.Value, "sample_foreman_id_1") {
		t.Errorf("Value = %q, want the stale literal", f.Value)
	}

	hint := crudFixtureFixHint(f)
	for _, want := range []string{"crews_foreman_id_fkey", "crew_members", "NULL", "yours"} {
		if !strings.Contains(hint, want) {
			t.Errorf("fix hint missing %q:\n%s", want, hint)
		}
	}
}

// TestCrudFixtures_NoFKNoFinding is the negative control for the case above.
// Same project, same `sample_foreman_id_1` literal, WITHOUT the migration
// that adds the constraint. A column with no foreign key accepts any value,
// so there is nothing to report — and a check that fired here would be
// flagging the `sample_` spelling rather than a real dangling reference.
func TestCrudFixtures_NoFKNoFinding(t *testing.T) {
	root := newCrewProject(t)

	findings, err := collectCrudFixtureFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectCrudFixtureFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings with no FK on crews.foreman_id, want 0\n%+v", len(findings), findings)
	}
}

// TestCrudFixtures_SeededParentIsClean pins the other half of the contract:
// a value that DOES name a seeded parent row is correct and must stay
// silent, even though the parent's own id is a synthesized `sample_` string.
// This is the case a stamp-based check would have false-flagged.
func TestCrudFixtures_SeededParentIsClean(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_init.up.sql", `
CREATE TABLE estimates (
    id TEXT PRIMARY KEY,
    label TEXT NOT NULL
);
CREATE TABLE invoices (
    id TEXT PRIMARY KEY,
    estimate_id TEXT REFERENCES estimates (id)
);
`)
	writeFile(t, root, "internal/handlers/billing/handlers_crud_test.go", "package handlers_test\n\n"+
		"func TestCRUD_Invoice_Lifecycle(t *testing.T) {\n"+
		"\tif _, err := db.Exec(context.Background(), `\n"+
		"INSERT INTO \"estimates\" (\"id\", \"label\") VALUES\n"+
		"    ('sample_id_1', 'sample_label_1'),\n"+
		"    ('sample_id_2', 'sample_label_2')\n"+
		"ON CONFLICT (id) DO NOTHING;\n"+
		"INSERT INTO \"invoices\" (\"id\", \"estimate_id\") VALUES\n"+
		"    ('inv-1', 'sample_id_1'),\n"+
		"    ('inv-2', NULL)\n"+
		"ON CONFLICT (id) DO NOTHING;\n"+
		"`); err != nil {\n"+
		"\t\tt.Fatalf(\"seed parent rows: %v\", err)\n"+
		"\t}\n"+
		"}\n")

	findings, err := collectCrudFixtureFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectCrudFixtureFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings, want 0 — a seeded parent and a NULL both satisfy the FK\n%+v", len(findings), findings)
	}
}

// TestCrudFixtures_CommentedFKIsNotApplied guards the specific false
// positive forge's own scaffold would otherwise cause: a birth migration
// writes the FK it cannot yet apply as a COMMENTED-OUT suggestion. Reading
// it as applied would flag every project that has not uncommented it.
func TestCrudFixtures_CommentedFKIsNotApplied(t *testing.T) {
	root := newCrewProject(t)
	// crewsBirthSQL carries the commented-out foreman FK and nothing else
	// declares it, so the constraint does not exist.
	fks, err := foreignKeysFromMigrations(root, filepath.Join(root, "db", "migrations"))
	if err != nil {
		t.Fatalf("foreignKeysFromMigrations: %v", err)
	}
	if _, found := fks["crews"]["foreman_id"]; found {
		t.Error("commented-out ALTER TABLE was read as an applied foreign key")
	}
	// The inline REFERENCES on crew_members.crew_id is real and must be read.
	fk, found := fks["crew_members"]["crew_id"]
	if !found {
		t.Fatal("inline REFERENCES on crew_members.crew_id was not detected")
	}
	if fk.RefTable != "crews" || fk.RefColumn != "id" {
		t.Errorf("crew_members.crew_id references %s.%s, want crews.id", fk.RefTable, fk.RefColumn)
	}
}

// TestCrudFixtures_DroppedConstraintIsRetracted pins that the replay is a
// replay: a constraint added and later dropped is not part of the schema
// the fixtures must satisfy.
func TestCrudFixtures_DroppedConstraintIsRetracted(t *testing.T) {
	root := newCrewProject(t)
	writeFile(t, root, "db/migrations/20240301000000_add_crew_foreman_fk.up.sql", foremanFKSQL)
	writeFile(t, root, "db/migrations/20240401000000_drop_crew_foreman_fk.up.sql",
		"ALTER TABLE crews DROP CONSTRAINT crews_foreman_id_fkey;\n")

	findings, err := collectCrudFixtureFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectCrudFixtureFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings after the constraint was dropped, want 0\n%+v", len(findings), findings)
	}
}

// TestCrudFixtures_NoProjectIsClean pins graceful degradation: a directory
// with no migrations and no handlers is not an error, it is a project this
// lane does not apply to.
func TestCrudFixtures_NoProjectIsClean(t *testing.T) {
	findings, err := collectCrudFixtureFindings(t.TempDir(), "db/migrations")
	if err != nil {
		t.Fatalf("collectCrudFixtureFindings on an empty dir: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings on an empty project, want 0", len(findings))
	}
}

// TestCrudFixtures_CastAndQuotingNormalize pins that the comparison is over
// VALUES, not source spellings: a parent seeded as a bare string and a child
// referencing it through a ::uuid cast are the same row.
func TestCrudFixtures_CastAndQuotingNormalize(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/migrations/20240101000000_init.up.sql", `
CREATE TABLE owners (id UUID PRIMARY KEY);
CREATE TABLE pets (
    id UUID PRIMARY KEY,
    owner_id UUID,
    CONSTRAINT pets_owner_id_fkey FOREIGN KEY (owner_id) REFERENCES owners (id)
);
`)
	writeFile(t, root, "internal/handlers/pets/handlers_crud_test.go", "package handlers_test\n\n"+
		"var _ = `\n"+
		"INSERT INTO \"owners\" (\"id\") VALUES\n"+
		"    ('33333333-3333-4333-8333-333333333333')\n"+
		"ON CONFLICT (id) DO NOTHING;\n"+
		"INSERT INTO \"pets\" (\"id\", \"owner_id\") VALUES\n"+
		"    ('44444444-4444-4444-8444-444444444444'::uuid, '33333333-3333-4333-8333-333333333333'::uuid)\n"+
		"ON CONFLICT (id) DO NOTHING;\n"+
		"`\n")

	findings, err := collectCrudFixtureFindings(root, "db/migrations")
	if err != nil {
		t.Fatalf("collectCrudFixtureFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings, want 0 — a ::uuid cast does not change which row is referenced\n%+v", len(findings), findings)
	}
}

// TestCrudFixtures_FormatReportsClean pins the human output shape against
// its advisory siblings: a clean run says so on one line, and a run with
// findings states plainly that it is not failing the build.
func TestCrudFixtures_FormatReportsClean(t *testing.T) {
	var b strings.Builder
	formatCrudFixtures(&b, nil)
	if !strings.Contains(b.String(), "crud-fixtures clean") {
		t.Errorf("clean report = %q, want a crud-fixtures clean line", b.String())
	}

	b.Reset()
	formatCrudFixtures(&b, []crudFixtureFinding{{
		File: "internal/handlers/crews/handlers_crud_test.go", Line: 12,
		Table: "crews", Column: "foreman_id", Value: "'sample_foreman_id_1'",
		RefTable: "crew_members", RefColumn: "id", Constraint: "crews_foreman_id_fkey",
	}})
	out := b.String()
	for _, want := range []string{"forge-crud-fixtures", "handlers_crud_test.go:12", "warnings only"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}
