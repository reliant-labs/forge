//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EScaffoldReadOnlyColumnTakesItsSchemaDefault pins the create path
// for a column the client cannot set.
//
// A `// forge:read-only` field is absent from the born create request, so
// the generated op left the Go field at its zero value and Bun wrote that
// zero. For an enum column — born as
//
//	tone TEXT NOT NULL DEFAULT 'TONE_WARM' CHECK (tone IN (...))
//
// — the zero is the empty string, which violates the CHECK forge derived from the same
// proto. Every create failed, out of the box, on a marker forge documents:
//
//	--- FAIL: TestCRUD_Gadget_Lifecycle
//	    create #1: invalid_argument: create gadget: a field value violates a constraint
//
// The op now writes the column's own DEFAULT for anything the request does
// not carry. The second half of the test is the reason that lives in the op
// and NOT in a `nullzero`/`default:` bun tag: Bun cannot tell "unset" from
// "zero", so a tag would silently store true for an explicit false on a
// hand-written `DEFAULT true` column. The op can tell, because it knows
// which columns the request carried.
func TestE2EScaffoldReadOnlyColumnTakesItsSchemaDefault(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "ssdapp", "--mod", "example.com/ssdapp", "--service", "widget")
	projectDir := filepath.Join(dir, "ssdapp")
	addCorpusForgePkgReplace(t, projectDir)

	protoPath := filepath.Join(projectDir, "proto", "services", "widget", "v1", "widget.proto")
	proto := readFileE2E(t, protoPath)
	proto += "\nenum Tone {\n" +
		"  TONE_UNSPECIFIED = 0;\n" +
		"  TONE_WARM = 1;\n" +
		"  TONE_COOL = 2;\n" +
		"}\n" +
		"\n// forge:entity\n" +
		"message Gadget {\n" +
		"  string name = 1;\n" +
		"  Tone tone = 2; // forge:read-only\n" +
		"  int64 priority = 3; // forge:read-only\n" +
		"  bool active = 4;\n" +
		"}\n"
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("append Gadget entity to widget proto: %v", err)
	}

	runCmd(t, projectDir, forgeBin, "scaffold")

	// The born CRUD lifecycle test runs the real stack against the real
	// migrations. This is the reproduction, verbatim.
	runCmd(t, projectDir, "go", "test", "-count=1", "./internal/handlers/widget/")

	opsPath := filepath.Join(projectDir, "internal", "handlers", "widget", "handlers_crud_ops_gen.go")
	createOp := sliceBetweenCLI(readFileE2E(t, opsPath), "func (s *Service) crudCreateGadgetOp()", "Persist:")
	// TONE_WARM, not TONE_UNSPECIFIED: the born migration defaults an enum
	// column to the first REAL member, so what the server sets for a
	// read-only field is a state the domain actually has. The two halves
	// compose — the column DEFAULT is what the create op pastes.
	if !strings.Contains(createOp, `e.Tone = "TONE_WARM"`) {
		t.Errorf("the create op must write the enum column's DEFAULT for a read-only field:\n%s", createOp)
	}
	if strings.Contains(createOp, "UNSPECIFIED") {
		t.Errorf("a read-only enum must not be born at the proto zero:\n%s", createOp)
	}

	// Evolution the charter tells authors to write by hand: a non-zero
	// default plus a range CHECK on the read-only column, and a
	// `DEFAULT true` on a column the client DOES set.
	migDir := filepath.Join(projectDir, "db", "migrations")
	up := "ALTER TABLE gadgets ALTER COLUMN priority SET DEFAULT 5;\n" +
		"ALTER TABLE gadgets ADD CONSTRAINT gadgets_priority_range CHECK (priority BETWEEN 1 AND 10);\n" +
		"ALTER TABLE gadgets ALTER COLUMN active SET DEFAULT true;\n"
	down := "ALTER TABLE gadgets DROP CONSTRAINT gadgets_priority_range;\n" +
		"ALTER TABLE gadgets ALTER COLUMN priority SET DEFAULT 0;\n" +
		"ALTER TABLE gadgets ALTER COLUMN active SET DEFAULT false;\n"
	if err := os.WriteFile(filepath.Join(migDir, "00002_tighten_gadgets.up.sql"), []byte(up), 0o644); err != nil {
		t.Fatalf("write evolution migration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00002_tighten_gadgets.down.sql"), []byte(down), 0o644); err != nil {
		t.Fatalf("write evolution down migration: %v", err)
	}
	runCmd(t, projectDir, forgeBin, "generate")

	createOp = sliceBetweenCLI(readFileE2E(t, opsPath), "func (s *Service) crudCreateGadgetOp()", "Persist:")
	if !strings.Contains(createOp, "e.Priority = 5") {
		t.Errorf("the create op must write the hand-written DEFAULT 5 for a read-only column:\n%s", createOp)
	}
	// `active` IS on the create request: its value comes from the caller and
	// nothing may substitute the column default for an explicit false.
	if !strings.Contains(createOp, "e.Active = req.Active") {
		t.Errorf("a client-settable column must come from the request:\n%s", createOp)
	}
	if strings.Contains(createOp, "e.Active = true") {
		t.Errorf("the column DEFAULT was written over a client-settable field — an explicit false would be lost:\n%s", createOp)
	}
	if strings.Contains(readFileE2E(t, filepath.Join(projectDir, "internal", "db", "gadget_orm_gen.go")), "nullzero") {
		t.Errorf("the ORM must not mark a NOT NULL non-timestamp column nullzero: Bun cannot tell unset from zero")
	}

	runCmd(t, projectDir, "go", "test", "-count=1", "./internal/handlers/widget/")
	runCmd(t, projectDir, "go", "vet", "./...")
}
