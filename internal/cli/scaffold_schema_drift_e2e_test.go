//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2ESchemaDriftNotice is the acceptance gate for the post-birth
// schema-drift NOTICE. forge freezes proto→schema truth into the born
// migration and never edits it, so a proto that changes AFTER birth can
// leave the DB's CHECK/columns behind what the proto now declares. forge
// does NOT auto-write a migration — it PRINTS the divergence with a
// suggested ALTER, non-fatally, on every `forge generate` (and on demand
// via `forge db check`).
//
// The scenario, driven through the REAL pipeline (buf compile + embedded-
// postgres shadow apply):
//
//	Born: `// forge:entity Order` with an enum status (values A,B) and an
//	int64 amount_cents gte 0. Then (a) a developer adds a column forge never
//	projected, and (b) the proto's enum gains a value + the validate bound
//	tightens (gte 0 → gte 1).
//
// Asserts:
//   - a clean generate (nothing changed) prints NO drift notice;
//   - the developer-added column never trips a false positive;
//   - after the proto tightens, generate STILL SUCCEEDS (non-fatal), prints
//     the notice naming BOTH drifts with a correct suggested ALTER, and
//     writes NO new migration file.
func TestE2ESchemaDriftNotice(t *testing.T) {
	t.Parallel()
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "shopapp", "--mod", "example.com/shopapp", "--service", "orders")
	projectDir := filepath.Join(dir, "shopapp")
	addCorpusForgePkgReplace(t, projectDir)

	protoPath := filepath.Join(projectDir, "proto", "services", "orders", "v1", "orders.proto")
	proto := readFileE2E(t, protoPath)
	if !strings.Contains(proto, `import "buf/validate/validate.proto";`) {
		proto = strings.Replace(proto, `syntax = "proto3";`,
			`syntax = "proto3";`+"\n"+`import "buf/validate/validate.proto";`, 1)
	}
	proto += `
enum OrderStatus {
  ORDER_STATUS_A = 0;
  ORDER_STATUS_B = 1;
}

// forge:entity
message Order {
  string id = 1;
  string name = 2;
  OrderStatus status = 3;
  int64 amount_cents = 4 [(buf.validate.field).int64.gte = 0];
}
`
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("author orders proto: %v", err)
	}

	// Birth the orders table + run the full pipeline.
	runCmd(t, projectDir, forgeBin, "scaffold")

	// ── clean generate: no drift, no false positive ──
	if out := runCmdOutput(t, projectDir, forgeBin, "generate"); strings.Contains(out, "Schema drift") {
		t.Fatalf("clean generate must not report schema drift; got:\n%s", out)
	}

	// A developer adds a column forge never projected — its own migration.
	devMig := filepath.Join(projectDir, "db", "migrations", "00002_add_internal_note.up.sql")
	if err := os.WriteFile(devMig, []byte("ALTER TABLE orders ADD COLUMN internal_note TEXT;\n"), 0o644); err != nil {
		t.Fatalf("write dev migration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "db", "migrations", "00002_add_internal_note.down.sql"),
		[]byte("ALTER TABLE orders DROP COLUMN internal_note;\n"), 0o644); err != nil {
		t.Fatalf("write dev down migration: %v", err)
	}
	if out := runCmdOutput(t, projectDir, forgeBin, "generate"); strings.Contains(out, "Schema drift") {
		t.Fatalf("developer-added column must not trip a drift false positive; got:\n%s", out)
	}

	// ── the proto tightens after birth ──
	//
	// Re-read from DISK. `forge scaffold` completes the entity's CRUD
	// quintet into the user's proto file, so the in-memory `proto` above is
	// the pre-scaffold text: writing it back would silently delete
	// Create/Get/Update/Delete/ListOrdersRequest and the next generate would
	// fail on `undefined: pb.CreateOrderRequest` instead of reporting drift.
	// The scenario is "the author edits the proto", and the author edits what
	// is on disk.
	proto = readFileE2E(t, protoPath)
	tightened := strings.Replace(proto, "  ORDER_STATUS_B = 1;\n", "  ORDER_STATUS_B = 1;\n  ORDER_STATUS_C = 2;\n", 1)
	tightened = strings.Replace(tightened, "(buf.validate.field).int64.gte = 0", "(buf.validate.field).int64.gte = 1", 1)
	if tightened == proto {
		t.Fatalf("neither the enum nor the validate bound was edited — the drift scenario is vacuous; proto:\n%s", proto)
	}
	if !strings.Contains(tightened, "ORDER_STATUS_C") || !strings.Contains(tightened, "int64.gte = 1") {
		t.Fatalf("proto tightening did not apply BOTH edits; proto:\n%s", tightened)
	}
	if err := os.WriteFile(protoPath, []byte(tightened), 0o644); err != nil {
		t.Fatalf("tighten orders proto: %v", err)
	}

	migGlob := filepath.Join(projectDir, "db", "migrations", "*.up.sql")
	before, _ := filepath.Glob(migGlob)

	// generate must SUCCEED (non-fatal) and print the drift notice.
	out := runCmdOutput(t, projectDir, forgeBin, "generate")

	after, _ := filepath.Glob(migGlob)
	if len(after) != len(before) {
		t.Fatalf("drift must NOT write a migration: had %d up migrations, now %d", len(before), len(after))
	}

	for _, want := range []string{
		"Schema drift",
		"orders.status",
		"enum CHECK vocabulary",
		"ORDER_STATUS_C",
		"orders.amount_cents",
		"protovalidate CHECK",
		"ADD CHECK (amount_cents >= 1)",
		"DROP CONSTRAINT orders_status_check",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drift notice missing %q; full generate output:\n%s", want, out)
		}
	}
	// The dev column is still not flagged even amid real drift.
	if strings.Contains(out, "internal_note") {
		t.Errorf("developer-added column must never appear in the drift notice; got:\n%s", out)
	}

	// generate produced valid code despite the drift (non-fatal contract).
	runCmd(t, projectDir, "go", "build", "./...")

	// `forge db check` reports the same drift on demand and, with --strict,
	// exits non-zero.
	check := runCmdOutput(t, projectDir, forgeBin, "db", "check")
	if !strings.Contains(check, "orders.status") || !strings.Contains(check, "orders.amount_cents") {
		t.Errorf("`forge db check` should report both drifts; got:\n%s", check)
	}
}
