package schemadrift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// TestDetect_SentinelFreeEnumCheckIsNotDrift is the guard against the worst
// thing a drift detector can do: tell an author to UNDO a correct schema.
//
// The detector's expectation is forge's own birth projection, so it inherits
// whatever that projection decides about the proto3 zero sentinel. It used to
// inherit a projection that admitted the sentinel while the comment beside it
// and the scaffolded create form both refused it — so an author who followed
// the schema-review step and dropped the sentinel from the CHECK got a
// `forge generate` notice instructing them to add it back, complete with
// copy-pasteable SQL.
//
// The applied migration here IS forge's birth output with the sentinel
// stripped from the CHECK's value list — the exact edit the review step
// produces. It must read as a schema that matches, not as drift.
func TestDetect_SentinelFreeEnumCheckIsNotDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("Detect boots real postgres (shadow apply); skipped under -short")
	}

	fields := []codegen.SchemaFieldDef{
		{Name: "name", Kind: "string"},
		{Name: "status", Kind: "enum", TypeName: "orders.v1.OrderStatus"},
	}
	enums := map[string][]string{"orders.v1.OrderStatus": {
		"ORDER_STATUS_UNSPECIFIED", "ORDER_STATUS_OPEN", "ORDER_STATUS_CLOSED",
	}}

	// The reviewed schema: birth output minus the sentinel in the CHECK.
	// Once the projection itself refuses the sentinel this replacement is a
	// no-op, which is the point — the two sides converge on one answer.
	applied := strings.ReplaceAll(bornMigration(t, fields, enums), "'ORDER_STATUS_UNSPECIFIED', ", "")
	if strings.Contains(applied, "'ORDER_STATUS_UNSPECIFIED'") {
		t.Fatalf("the reviewed schema still admits the sentinel — the fixture no longer models the author's edit:\n%s", applied)
	}

	projectDir := t.TempDir()
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00001_create_orders.up.sql"), []byte(applied), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Detect(projectDir, ordersServices(fields, enums))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	// A zero-drift report proves nothing unless something was compared.
	if report.Inconclusive != "" {
		t.Fatalf("comparison was inconclusive, so the assertion below is vacuous: %s", report.Inconclusive)
	}
	if report.Compared != 1 {
		t.Fatalf("Compared = %d, want 1 — nothing was diffed, so this test proves nothing", report.Compared)
	}
	if !report.Empty() {
		t.Fatalf("the reviewed schema is the one forge projects; reporting drift here tells the author to undo it:\n%s", report.String())
	}
}
