package lint

import (
	"bytes"
	"strings"
	"testing"
)

// The FRICTION §N13 scenario, reduced to its two tables. org_id is born
// BARE — forge adds no foreign key for an owner/parent column by default —
// so the scaffold-once fixture fills it with a synthetic placeholder that
// was legal at the time. A later migration adds the constraint the column
// always semantically had, and every fixture row now violates it.
const ownerColumnBirthSQL = `
CREATE TABLE organizations (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE memberships (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    user_id TEXT NOT NULL
);
`

// The later migration. This is the change that broke six test packages in
// the dogfood run, and it surfaces only as a raw pq error in test setup.
const ownerColumnAddFKSQL = `
ALTER TABLE memberships ADD CONSTRAINT memberships_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations (id) ON DELETE CASCADE;
`

// The scaffolded fixture as forge wrote it before that migration: a literal
// org_id, and no organizations row anywhere in the file to match it.
const staleOwnerFixtureGo = "package handlers_test\n\n" +
	"func TestCRUD_Membership_Lifecycle(t *testing.T) {\n" +
	"\tif _, err := db.Exec(context.Background(), `\n" +
	"INSERT INTO \"memberships\" (\"id\", \"org_id\", \"user_id\") VALUES\n" +
	"    ('mem-1', 'sample_org_id_1', 'sample_user_id_1');\n" +
	"`); err != nil {\n" +
	"\t\tt.Fatalf(\"seed parent rows: %v\", err)\n" +
	"\t}\n" +
	"}\n"

// newOwnerColumnProject lays down the §N13 project: birth schema, the
// migration that adds the constraint, and the fixture that predates it.
func newOwnerColumnProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "db/migrations/00001_create.up.sql", ownerColumnBirthSQL)
	writeFile(t, root, "db/migrations/00007_add_owner_constraints.up.sql", ownerColumnAddFKSQL)
	writeFile(t, root, "internal/handlers/orgs/handlers_crud_test.go", staleOwnerFixtureGo)
	return root
}

// TestCrudFixturesRegisteredInPipeline is the reproduction for §N13.
//
// The crud-fixtures analyzer detects this drift correctly — it always has.
// It was reachable only through the explicit `forge lint --crud-fixtures`
// flag, and absent from the ordered pipeline that an unflagged `forge lint`
// runs. So the check existed, worked, and never ran: the reporter saw a
// green lint over five fixtures the schema rejects.
//
// Registration, not detection, is the defect this pins.
func TestCrudFixturesRegisteredInPipeline(t *testing.T) {
	step := findStep(t, "crud-fixtures lint")

	// Warning, never gating — matching fixture-drift and guarded-fields.
	// The remedy is an edit to a file forge does not own and may
	// legitimately be mid-edit.
	if step.gates {
		t.Error("crud-fixtures must not gate; the fix is an edit to a file forge does not own")
	}
	if step.runText == nil || step.collect == nil {
		t.Fatal("crud-fixtures step must supply both a text and a JSON path")
	}
	if !strings.Contains(step.errFormat, "⚠️") {
		t.Errorf("errFormat = %q, want the advisory ⚠️ prefix", step.errFormat)
	}
}

// TestCrudFixturesPipelineStepReportsOwnerColumnDrift drives the §N13
// project through the step's own collector — the path an unflagged
// `forge lint --json` takes — and asserts the finding reaches it.
//
// Asserting the analyzer directly would pass even with the step
// unregistered, which is precisely the bug. This goes through the step.
func TestCrudFixturesPipelineStepReportsOwnerColumnDrift(t *testing.T) {
	root := newOwnerColumnProject(t)
	step := findStep(t, "crud-fixtures lint")

	findings, gated, err := step.collect(&lintRunCtx{cwd: root})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if gated {
		t.Error("crud-fixtures findings must not gate the build")
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1\n%+v", len(findings), findings)
	}

	f := findings[0]
	if f.Severity != lintSevWarning {
		t.Errorf("Severity = %q, want %q", f.Severity, lintSevWarning)
	}
	// The four facts the pq error withheld: the constraint, the migration
	// that introduced it, the column, and that the file is the user's.
	for _, want := range []string{
		"memberships_org_id_fkey",
		"00007_add_owner_constraints",
		"org_id",
		"organizations",
		"yours",
	} {
		if !strings.Contains(f.FixHint, want) {
			t.Errorf("fix hint missing %q:\n%s", want, f.FixHint)
		}
	}
}

// TestCrudFixturesStepSilentWithoutTheMigration is the negative control for
// the two tests above, and the thing that proves they are not vacuous: the
// SAME fixture, with the constraint never added. A check that fired here
// would be flagging the spelling `sample_org_id_1` rather than a real
// schema conflict — and the placeholder is correct until the FK exists.
func TestCrudFixturesStepSilentWithoutTheMigration(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/migrations/00001_create.up.sql", ownerColumnBirthSQL)
	writeFile(t, root, "internal/handlers/orgs/handlers_crud_test.go", staleOwnerFixtureGo)

	step := findStep(t, "crud-fixtures lint")
	findings, _, err := step.collect(&lintRunCtx{cwd: root})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings before the constraint exists, want 0\n%+v", len(findings), findings)
	}
}

// TestFixtureDriftCleanLineDoesNotOverclaim pins the wording of the other
// half of §N13.
//
// The reporter ran `forge lint --fixture-drift`, read "no scaffolded seed
// block contradicts the current schema", and concluded the fixtures were
// sound. Five of them contradicted it — by a plain foreign key, which is a
// different lane's business. The sentence was a claim about the whole
// question while the lane answers two specific shapes of it, and a clean
// line that overstates its scope is worse than none: it converts "I did not
// check that" into "I checked, it is fine."
//
// So the clean line must name what it actually verified.
func TestFixtureDriftCleanLineDoesNotOverclaim(t *testing.T) {
	var buf bytes.Buffer
	formatFixtureDrift(&buf, nil)
	out := buf.String()

	if !strings.Contains(out, "fixture-drift clean") {
		t.Fatalf("clean report = %q, want a fixture-drift clean line", out)
	}
	// It must say which two shapes it cleared, so a reader cannot take it
	// for a verdict on foreign keys.
	for _, want := range []string{"GENERATED", "UNIQUE"} {
		if !strings.Contains(out, want) {
			t.Errorf("clean line must name the %s shape it actually checked:\n%s", want, out)
		}
	}
	// The overclaiming sentence itself must be gone.
	if strings.Contains(out, "no scaffolded seed block contradicts the current schema") {
		t.Errorf("clean line still claims to have cleared the whole schema question:\n%s", out)
	}
}
