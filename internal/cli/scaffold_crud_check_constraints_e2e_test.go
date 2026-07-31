//go:build e2e

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forgetScaffoldRecordE2E deletes a scaffold-once file AND its ledger entry,
// which is how an author asks forge to re-birth a file against the schema as
// it now stands. Deleting the file alone is deliberately not enough: the
// ledger records that forge already scaffolded it once, so a bare deletion
// stays deleted (that is the scaffold-once contract).
func forgetScaffoldRecordE2E(t *testing.T, projectDir, fullPath, relPath string) {
	t.Helper()
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove %s: %v", relPath, err)
	}
	ledgerPath := filepath.Join(projectDir, ".forge", "scaffolded.json")
	data, err := os.ReadFile(ledgerPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read scaffold ledger: %v", err)
	}
	var ledger struct {
		Files map[string]json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatalf("parse scaffold ledger: %v", err)
	}
	if _, ok := ledger.Files[relPath]; !ok {
		t.Fatalf("scaffold ledger has no record of %s — the re-birth this sets up would be vacuous", relPath)
	}
	delete(ledger.Files, relPath)
	out, err := json.MarshalIndent(map[string]any{"files": ledger.Files}, "", "  ")
	if err != nil {
		t.Fatalf("marshal scaffold ledger: %v", err)
	}
	if err := os.WriteFile(ledgerPath, out, 0o644); err != nil {
		t.Fatalf("write scaffold ledger: %v", err)
	}
}

// runCmdAllowFail runs a command and RETURNS its failure instead of aborting
// the test. Needed wherever the failure IS the behaviour under test.
func runCmdAllowFail(t *testing.T, dir string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOFLAGS=",
		"GONOSUMCHECK=*",
		"GOPROXY=https://proxy.golang.org,direct",
		"GONOSUMDB=*",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The defect this pins, stated as the run that found it:
//
//	FAIL: TestCRUD_Order_Lifecycle
//	  create #1: invalid_argument: create order: a field value violates a
//	  constraint (constraint orders_customer_id_check)
//
// An author wrote migrations with the CHECKs forge's own charter tells them to
// write — an email regex, a two-character country code, a bounded name — and
// the generated lifecycle test inserted values those CHECKs reject. The test is
// scaffold-once, so the author's only escape was deleting it, and it
// regenerated.
//
// The migration below carries the SAME constraints, copied from the run that
// failed: the email regex, the two-character country code, the bounded name,
// plus the numeric IN-list that probing showed the derivation also could not
// invert. It is a hand-written TIGHTENING of the born table — which is exactly
// what forge's charter tells authors to write ("add relationships, indexes,
// and constraints with hand-written migrations").
//
// The proof is not an assertion about the generated text — it is `go test` on
// the generated project. If the fixtures violate the schema, create #1 fails
// exactly as it did in the wild.
const checkConstraintMigration = `
ALTER TABLE orders ADD CONSTRAINT orders_customer_email_check
    CHECK (customer_email ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$');
ALTER TABLE orders ADD CONSTRAINT orders_shipping_country_check
    CHECK (char_length(shipping_country) = 2);
ALTER TABLE orders ADD CONSTRAINT orders_shipping_name_check
    CHECK (char_length(shipping_name) BETWEEN 1 AND 160);
ALTER TABLE orders ADD CONSTRAINT orders_priority_check
    CHECK (priority IN (10, 20, 30));
`

// TestE2ECRUDFixtureSatisfiesCheckConstraints scaffolds a project carrying the
// shopdemo CHECKs and runs its generated CRUD lifecycle test.
//
// The constraints are added BEFORE the lifecycle test is scaffolded, because
// that is the order that matters: fixtures are derived on the first-scaffold
// path, from the schema as it stands then. A schema that already carries tight
// CHECKs is the case the run hit.
func TestE2ECRUDFixtureSatisfiesCheckConstraints(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "shopchk", "--mod", "example.com/shopchk", "--service", "storefront")
	projectDir := filepath.Join(dir, "shopchk")
	addCorpusForgePkgReplace(t, projectDir)

	protoPath := filepath.Join(projectDir, "proto", "services", "storefront", "v1", "storefront.proto")
	proto := readFileE2E(t, protoPath)
	proto += "\n// forge:entity\n" +
		"message Order {\n" +
		"  string customer_id = 1;\n" +
		"  string customer_email = 2;\n" +
		"  string shipping_name = 3;\n" +
		"  string shipping_country = 4;\n" +
		"  int64 priority = 5;\n" +
		"}\n"
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("append Order entity: %v", err)
	}

	// Phase 1 births the table and injects the CRUD quintet; phase 2 would
	// normally scaffold the lifecycle test in the same run. Birth first, then
	// TIGHTEN the born migration by hand — which is exactly what the charter
	// tells authors to do ("add relationships, indexes, and constraints with
	// hand-written migrations") and what the failing run's author had done.
	runCmd(t, projectDir, forgeBin, "scaffold")

	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.WriteFile(filepath.Join(migDir, "00090_checks.up.sql"),
		[]byte(checkConstraintMigration), 0o644); err != nil {
		t.Fatalf("write checks migration: %v", err)
	}
	down := "ALTER TABLE orders DROP CONSTRAINT orders_priority_check;\n" +
		"ALTER TABLE orders DROP CONSTRAINT orders_shipping_name_check;\n" +
		"ALTER TABLE orders DROP CONSTRAINT orders_shipping_country_check;\n" +
		"ALTER TABLE orders DROP CONSTRAINT orders_customer_email_check;\n"
	if err := os.WriteFile(filepath.Join(migDir, "00090_checks.down.sql"), []byte(down), 0o644); err != nil {
		t.Fatalf("write checks down migration: %v", err)
	}

	// The lifecycle test is scaffold-once and was born against the loose
	// schema. Clearing its ledger record is how an author asks for a fresh
	// birth against the schema as it now stands.
	crudTestPath := filepath.Join(projectDir, "internal", "handlers", "storefront", "handlers_crud_test.go")
	forgetScaffoldRecordE2E(t, projectDir, crudTestPath,
		filepath.Join("internal", "handlers", "storefront", "handlers_crud_test.go"))
	runCmd(t, projectDir, forgeBin, "generate")

	crudTest := readFileE2E(t, crudTestPath)
	if !strings.Contains(crudTest, "TestCRUD_Order_Lifecycle") {
		t.Fatalf("lifecycle test was not scaffolded against the constrained schema:\n%s", crudTest)
	}

	// The fixtures must NOT be the type-blind placeholders the run shipped.
	// This is a property of the DERIVATION (a placeholder is the emitter's own
	// stamp for "I invented this"), not a hardcoded expected value.
	for _, dead := range []string{`"sample_customer_email_1"`, `"sample_shipping_country_1"`} {
		if strings.Contains(crudTest, dead) {
			t.Errorf("fixture %s violates its own CHECK and was emitted anyway", dead)
		}
	}

	// THE PROOF: the generated project's own CRUD lifecycle test passes
	// against the constrained schema. create #1 is the assertion.
	runCmd(t, projectDir, "go", "test", "-count=1", "./internal/handlers/storefront/")
}

// TestE2ECRUDFixtureGuardFailsLoudlyOnUninvertibleCheck pins the other half of
// the contract: when the derivation CANNOT satisfy a constraint, generation
// must fail loudly naming the column and the constraint — never emit a fixture
// its own schema rejects and leave it to surface as a mysterious test failure.
//
// The constraint is a lookahead: postgres's POSIX engine accepts it, Go's RE2
// cannot compile it, so there is nothing to invert. Before the guard, this
// silently produced `sample_passcode_1` and a create #1 failure much later.
func TestE2ECRUDFixtureGuardFailsLoudlyOnUninvertibleCheck(t *testing.T) {
	t.Parallel()
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "guardapp", "--mod", "example.com/guardapp", "--service", "account")
	projectDir := filepath.Join(dir, "guardapp")
	addCorpusForgePkgReplace(t, projectDir)

	protoPath := filepath.Join(projectDir, "proto", "services", "account", "v1", "account.proto")
	proto := readFileE2E(t, protoPath)
	proto += "\n// forge:entity\n" +
		"message Credential {\n" +
		"  string passcode = 1;\n" +
		"}\n"
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("append Credential entity: %v", err)
	}
	runCmd(t, projectDir, forgeBin, "scaffold")

	// Tighten the born table with a CHECK forge's derivation cannot invert.
	migDir := filepath.Join(projectDir, "db", "migrations")
	up := "ALTER TABLE credentials ADD CONSTRAINT credentials_passcode_check " +
		`CHECK (passcode ~ '^(?=.*[A-Z])[a-zA-Z]{8,}$');` + "\n"
	if err := os.WriteFile(filepath.Join(migDir, "00090_passcode.up.sql"), []byte(up), 0o644); err != nil {
		t.Fatalf("write migration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00090_passcode.down.sql"),
		[]byte("ALTER TABLE credentials DROP CONSTRAINT credentials_passcode_check;\n"), 0o644); err != nil {
		t.Fatalf("write down migration: %v", err)
	}

	crudTestPath := filepath.Join(projectDir, "internal", "handlers", "account", "handlers_crud_test.go")
	forgetScaffoldRecordE2E(t, projectDir, crudTestPath,
		filepath.Join("internal", "handlers", "account", "handlers_crud_test.go"))

	// Generation must FAIL, and the message must locate the problem.
	out, err := runCmdAllowFail(t, projectDir, forgeBin, "generate")
	if err == nil {
		t.Fatalf("generate succeeded against a constraint it cannot satisfy; output:\n%s", out)
	}
	for _, want := range []string{"passcode", "credentials_passcode_check", "vocab.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure message omits %q — the author cannot act on it:\n%s", want, out)
		}
	}

	// And the violating file must not have been written.
	if _, statErr := os.Stat(crudTestPath); statErr == nil {
		t.Errorf("a fixture file the schema rejects was written anyway: %s", crudTestPath)
	}
}
