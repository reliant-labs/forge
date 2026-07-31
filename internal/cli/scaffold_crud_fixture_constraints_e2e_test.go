//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2ECRUDFixtureSurvivesAddedConstraints pins the born CRUD lifecycle
// test against the constraints forge itself tells the author to add.
//
// handlers_crud_test.go is scaffold-once — "yours: scaffolded once, never
// touched again" — but the schema is not. It used to create two rows with
// IDENTICAL field values and no parent row for a `<x>_id` column. Both are
// fine until the constraints arrive:
//
//   - the FOREIGN KEY on `<x>_id`, which birth now WRITES LIVE (a resolved
//     reference is a real constraint from the first apply — see "scaffold:
//     birth migrations apply the foreign keys they resolve") → create #1
//     fails with failed_precondition, referenced record missing, unless the
//     born fixture seeds the parent;
//   - "add relationships, indexes, and constraints with hand-written
//     migrations", forge's own advice for everything birth cannot derive →
//     the first UNIQUE index fails create #2 with already_exists.
//
// Identical rows bought nothing (the test needs two DISTINCT records to
// prove list/get/update), so the fixtures now differ everywhere the type
// admits a second value, and the parent is seeded because the FK is real.
func TestE2ECRUDFixtureSurvivesAddedConstraints(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "cfcapp", "--mod", "example.com/cfcapp", "--service", "widget")
	projectDir := filepath.Join(dir, "cfcapp")
	addCorpusForgePkgReplace(t, projectDir)

	protoPath := filepath.Join(projectDir, "proto", "services", "widget", "v1", "widget.proto")
	proto := readFileE2E(t, protoPath)
	proto += "\n// forge:entity\n" +
		"message Brand {\n" +
		"  string name = 1;\n" +
		"}\n" +
		"\n// forge:entity\n" +
		"message Gadget {\n" +
		"  string name = 1;\n" +
		"  string sku = 2;\n" +
		"  int64 seq = 3;\n" +
		"  string brand_id = 4;\n" +
		"}\n"
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("append entities to widget proto: %v", err)
	}

	runCmd(t, projectDir, forgeBin, "scaffold")

	// Birth writes the resolved FOREIGN KEY LIVE — not commented out. This
	// is what makes the parent-row seeding below load-bearing rather than
	// decorative: without the constraint, create #1 would pass on a
	// dangling brand_id and the fixture would prove nothing.
	gadgetsUp := readBornMigrationE2E(t, projectDir, "gadgets")
	if !strings.Contains(gadgetsUp, "REFERENCES brands") {
		t.Errorf("gadgets birth migration must apply the resolved brand_id FOREIGN KEY, not suggest it:\n%s", gadgetsUp)
	}
	for _, line := range strings.Split(gadgetsUp, "\n") {
		if strings.Contains(line, "REFERENCES") && strings.HasPrefix(strings.TrimSpace(line), "--") {
			t.Errorf("birth must not emit a COMMENTED-OUT foreign key (dangling-by-default): %q", strings.TrimSpace(line))
		}
	}

	// Every create field differs between the two creates.
	crudTestPath := filepath.Join(projectDir, "internal", "handlers", "widget", "handlers_crud_test.go")
	crudTest := readFileE2E(t, crudTestPath)
	if strings.Count(crudTest, `"test-value"`) != strings.Count(crudTest, `"test-value-2"`) {
		t.Errorf("create #1 and create #2 do not carry matching distinct literals:\n%s", crudTest)
	}
	if !strings.Contains(crudTest, `INSERT INTO "brands"`) {
		t.Errorf("the born test seeds no parent row for brand_id, the FK forge suggests in its own migration:\n%s", crudTest)
	}
	runCmd(t, projectDir, "go", "test", "-count=1", "./internal/handlers/widget/")

	// Now take forge's own advice, by hand, after birth: UNIQUE indexes on
	// three differently-typed columns. (The FOREIGN KEY is NOT here — birth
	// already applied it, asserted above. Re-adding it by hand would fail
	// with "constraint already exists", which is the schema saying the same
	// thing.)
	migDir := filepath.Join(projectDir, "db", "migrations")
	up := "CREATE UNIQUE INDEX gadgets_sku_uniq ON gadgets (sku);\n" +
		"CREATE UNIQUE INDEX gadgets_seq_uniq ON gadgets (seq);\n" +
		"CREATE UNIQUE INDEX gadgets_name_uniq ON gadgets (name);\n"
	down := "DROP INDEX gadgets_name_uniq;\nDROP INDEX gadgets_seq_uniq;\nDROP INDEX gadgets_sku_uniq;\n"
	if err := os.WriteFile(filepath.Join(migDir, "00090_constraints.up.sql"), []byte(up), 0o644); err != nil {
		t.Fatalf("write constraints migration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00090_constraints.down.sql"), []byte(down), 0o644); err != nil {
		t.Fatalf("write constraints down migration: %v", err)
	}

	// The scaffold-once test must survive UNCHANGED — that is the contract.
	runCmd(t, projectDir, "go", "test", "-count=1", "./internal/handlers/widget/")
	if readFileE2E(t, crudTestPath) != crudTest {
		t.Errorf("handlers_crud_test.go changed; it is scaffold-once and must survive as written")
	}
}
