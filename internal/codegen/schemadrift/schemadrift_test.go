package schemadrift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/scaffold"
	"github.com/reliant-labs/forge/internal/schemadef"
)

// bornMigration renders forge's real birth-time projection for an entity and
// returns the up.sql, so the "applied" schema under test is authentic forge
// output — exactly what a `--from-proto` birth would have frozen.
func bornMigration(t *testing.T, fields []codegen.SchemaFieldDef, enums map[string][]string) string {
	t.Helper()
	mig := scaffold.RenderEntityMigrationFromProto(scaffold.EntityFromProtoSpec{
		Table:      "orders",
		MessageFQ:  "orders.v1.Order",
		ProtoPkg:   "orders.v1",
		Fields:     fields,
		Enums:      enums,
		Timestamps: true,
	})
	return mig.UpSQL
}

// ordersServices builds a synthetic descriptor with the CRUD create method
// (enough for entity discovery) plus the deep schema + enum vocabulary the
// desired projection reads.
func ordersServices(fields []codegen.SchemaFieldDef, enums map[string][]string) []codegen.ServiceDef {
	return []codegen.ServiceDef{{
		Name:    "OrdersService",
		Package: "orders.v1",
		Methods: []codegen.Method{{Name: "CreateOrder"}},
		Schemas: map[string][]codegen.SchemaFieldDef{"orders.v1.Order": fields},
		Enums:   enums,
	}}
}

// TestDetect_EnumAndValidateDrift is the RED→GREEN acceptance test at the
// detection layer, against real postgres shadows on BOTH sides:
//
//	born from proto v1 (enum {A,B}; amount_cents >= 0) + a developer-added
//	column forge never projected → then the proto tightens to v2 (enum
//	{A,B,C}; amount_cents >= 1).
//
// GREEN control: v1 proto vs its own born migration reports nothing (and the
// developer column is not a false positive). RED→drift: v2 proto reports
// exactly the enum-vocabulary and protovalidate CHECK drifts, each with a
// correct DROP+ADD suggestion, and still nothing for the developer column or
// the unchanged field.
func TestDetect_EnumAndValidateDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("Detect boots real postgres (shadow apply); skipped under -short")
	}

	fieldsV1 := []codegen.SchemaFieldDef{
		{Name: "name", Kind: "string"},
		{Name: "status", Kind: "enum", TypeName: "orders.v1.OrderStatus"},
		{Name: "amount_cents", Kind: "int64", Validate: &codegen.FieldConstraints{Gte: "0"}},
	}
	enumsV1 := map[string][]string{"orders.v1.OrderStatus": {"ORDER_STATUS_A", "ORDER_STATUS_B"}}

	// The applied schema: forge's own birth output, plus a hand-added column
	// in a later migration that forge never projected.
	projectDir := t.TempDir()
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(migDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("00001_create_orders.up.sql", bornMigration(t, fieldsV1, enumsV1))
	write("00002_add_internal_note.up.sql", "ALTER TABLE orders ADD COLUMN internal_note TEXT;\n")

	// ── GREEN control: current proto still matches the born migration ──
	report, err := Detect(projectDir, ordersServices(fieldsV1, enumsV1))
	if err != nil {
		t.Fatalf("Detect (v1 control): %v", err)
	}
	if !report.Empty() {
		t.Fatalf("expected NO drift when the proto is unchanged, got:\n%s", report.String())
	}

	// ── RED→drift: enum gains a value, the numeric bound tightens ──
	fieldsV2 := []codegen.SchemaFieldDef{
		{Name: "name", Kind: "string"},
		{Name: "status", Kind: "enum", TypeName: "orders.v1.OrderStatus"},
		{Name: "amount_cents", Kind: "int64", Validate: &codegen.FieldConstraints{Gte: "1"}},
	}
	enumsV2 := map[string][]string{"orders.v1.OrderStatus": {"ORDER_STATUS_A", "ORDER_STATUS_B", "ORDER_STATUS_C"}}

	report, err = Detect(projectDir, ordersServices(fieldsV2, enumsV2))
	if err != nil {
		t.Fatalf("Detect (v2 drift): %v", err)
	}
	if report.Empty() {
		t.Fatal("expected drift after tightening the proto, got none")
	}

	byCol := map[string]Drift{}
	for _, d := range report.Drifts {
		if _, dup := byCol[d.Column]; dup {
			t.Fatalf("duplicate drift for column %q", d.Column)
		}
		byCol[d.Column] = d
	}

	// Enum vocabulary drift on status.
	status, ok := byCol["status"]
	if !ok {
		t.Fatalf("expected enum drift on status; drifts: %+v", report.Drifts)
	}
	if !strings.Contains(status.Kind, "enum CHECK vocabulary") {
		t.Errorf("status drift kind = %q, want enum-vocabulary label", status.Kind)
	}
	statusSQL := strings.Join(status.SuggestedSQL, "\n")
	if !strings.Contains(statusSQL, "DROP CONSTRAINT orders_status_check") {
		t.Errorf("status suggestion should DROP the born check; got:\n%s", statusSQL)
	}
	if !strings.Contains(statusSQL, "ADD CHECK (status IN (") || !strings.Contains(statusSQL, "ORDER_STATUS_C") {
		t.Errorf("status suggestion should ADD the new vocabulary incl. ORDER_STATUS_C; got:\n%s", statusSQL)
	}

	// protovalidate CHECK drift on amount_cents.
	amt, ok := byCol["amount_cents"]
	if !ok {
		t.Fatalf("expected protovalidate drift on amount_cents; drifts: %+v", report.Drifts)
	}
	if !strings.Contains(amt.Kind, "protovalidate CHECK") {
		t.Errorf("amount_cents drift kind = %q, want protovalidate label", amt.Kind)
	}
	amtSQL := strings.Join(amt.SuggestedSQL, "\n")
	if !strings.Contains(amtSQL, "DROP CONSTRAINT orders_amount_cents_check") {
		t.Errorf("amount_cents suggestion should DROP the born check; got:\n%s", amtSQL)
	}
	if !strings.Contains(amtSQL, "ADD CHECK (amount_cents >= 1)") {
		t.Errorf("amount_cents suggestion should ADD the tightened bound; got:\n%s", amtSQL)
	}

	// No false positives: the developer-added column and the unchanged field
	// are never reported.
	if _, bad := byCol["internal_note"]; bad {
		t.Error("developer-added column internal_note must NOT be flagged as drift")
	}
	if _, bad := byCol["name"]; bad {
		t.Error("unchanged field name must NOT be flagged as drift")
	}
	if len(report.Drifts) != 2 {
		t.Errorf("expected exactly 2 drifts (status, amount_cents), got %d:\n%s", len(report.Drifts), report.String())
	}
}

// TestCanonicalCheckAtoms pins the SEMANTIC (not structural) CHECK comparator:
// a combined AND-joined bound predicate must fold into the SAME atom set as the
// split single-bound CHECKs forge's protovalidate projection emits, so a birth
// migration that froze the combined form is NOT flagged as drift. A genuinely
// different bound still yields a different atom (still drift), and enum / regex
// CHECKs stay opaque (matched by whole definition). Hermetic — no postgres, so
// it runs under -short. The CHECK definitions are the exact pg_get_constraintdef
// spellings verified against real postgres.
func TestCanonicalCheckAtoms(t *testing.T) {
	atomsOf := func(defs ...string) map[string]bool {
		checks := make([]schemadef.CheckConstraint, len(defs))
		for i, d := range defs {
			checks[i] = schemadef.CheckConstraint{Def: d}
		}
		return canonicalCheckAtomSet(checks)
	}
	eq := func(a, b map[string]bool) bool {
		if len(a) != len(b) {
			return false
		}
		for k := range a {
			if !b[k] {
				return false
			}
		}
		return true
	}

	var (
		combined = "CHECK (((exp_month >= 1) AND (exp_month <= 12)))"
		splitLo  = "CHECK ((exp_month >= 1))"
		splitHi  = "CHECK ((exp_month <= 12))"
		flipped  = "CHECK (((1 <= exp_month) AND (exp_month <= 12)))"
	)
	split := atomsOf(splitLo, splitHi)

	// The whole point: combined ≡ split (both directions, via one canonical set).
	if got := atomsOf(combined); !eq(got, split) {
		t.Fatalf("combined must equal split:\n combined=%v\n split=%v", got, split)
	}
	// Operand order is canonicalized (postgres keeps `1 <= exp_month` literal).
	if got := atomsOf(flipped); !eq(got, split) {
		t.Errorf("flipped operand order must equal split:\n flipped=%v\n split=%v", got, split)
	}
	// BETWEEN folds to the same >= / <= pair (defensive: postgres pre-expands it).
	if got := atomsOf("CHECK (exp_month BETWEEN 1 AND 12)"); !eq(got, split) {
		t.Errorf("BETWEEN must equal split: %v", got)
	}

	// Exact-value / exact-length equality folds to a >= N / <= N pair, so a
	// fixed-length field's `= N` projection and a legacy `BETWEEN N AND N` /
	// `>= N AND <= N` migration compare equal instead of spuriously drifting.
	exact := atomsOf("CHECK (char_length(code) = 3)")
	if got := atomsOf("CHECK (char_length(code) BETWEEN 3 AND 3)"); !eq(got, exact) {
		t.Errorf("`= 3` must equal `BETWEEN 3 AND 3`:\n =%v\n between=%v", exact, got)
	}
	if got := atomsOf("CHECK ((char_length(code) >= 3) AND (char_length(code) <= 3))"); !eq(got, exact) {
		t.Errorf("`= 3` must equal the split `>= 3 AND <= 3`:\n =%v\n split=%v", exact, got)
	}
	// A changed exact length is a real drift.
	if got := atomsOf("CHECK (char_length(code) = 4)"); eq(got, exact) {
		t.Error("`= 4` must differ from `= 3`")
	}
	// A `=` against a NON-numeric operand (default/enum equality) stays opaque.
	if _, ok := boundAtoms("CHECK ((status = 'active'::text))"); ok {
		t.Error("`= 'active'` (string literal) must NOT decompose into bound atoms")
	}

	// A real bound change must NOT collapse to the same set → still drift.
	if got := atomsOf("CHECK (((exp_month >= 1) AND (exp_month <= 13)))"); eq(got, split) {
		t.Error("a changed upper bound (<= 13) must differ from the <= 12 set")
	}
	// A different operator (> vs >=) is a real change too.
	if got := atomsOf("CHECK ((exp_month > 1))", splitHi); eq(got, split) {
		t.Error("`> 1` must differ from `>= 1`")
	}
	// Dropping a bound entirely is a change.
	if got := atomsOf(splitLo); eq(got, split) {
		t.Error("a single lower bound must differ from the two-bound set")
	}

	// Enum / regex CHECKs stay opaque and are matched by whole definition.
	enumAB := "CHECK ((status = ANY (ARRAY['A'::text, 'B'::text])))"
	if _, ok := boundAtoms(enumAB); ok {
		t.Error("enum `= ANY(ARRAY[...])` must NOT decompose into bound atoms")
	}
	if _, ok := boundAtoms("CHECK ((email ~ '^[^@]+@[^@]+$'::text))"); ok {
		t.Error("regex `~` must NOT decompose into bound atoms")
	}
	enumABC := "CHECK ((status = ANY (ARRAY['A'::text, 'B'::text, 'C'::text])))"
	if eq(atomsOf(enumAB), atomsOf(enumABC)) {
		t.Error("a changed enum vocabulary must still differ")
	}
}

// TestDetect_CombinedCheckEquivalentToSplit is the acceptance test for the
// false positive: a birth migration that froze the bounds as ONE combined
// predicate must NOT drift against forge's split-form projection — while a
// genuinely different bound still does. Real postgres shadows on both sides.
func TestDetect_CombinedCheckEquivalentToSplit(t *testing.T) {
	if testing.Short() {
		t.Skip("Detect boots real postgres (shadow apply); skipped under -short")
	}

	fields := []codegen.SchemaFieldDef{
		{Name: "name", Kind: "string"},
		{Name: "exp_month", Kind: "int32", Validate: &codegen.FieldConstraints{Gte: "1", Lte: "12"}},
	}
	// forge's own split-form birth output …
	split := bornMigration(t, fields, nil)
	// … rewritten so the birth migration froze ONE combined predicate, exactly
	// the hand-written spelling that previously demanded a re-split.
	combined := strings.Replace(split,
		"CHECK (exp_month >= 1) CHECK (exp_month <= 12)",
		"CHECK (exp_month >= 1 AND exp_month <= 12)", 1)
	if combined == split {
		t.Fatalf("test setup: expected split CHECK clauses in born migration:\n%s", split)
	}

	projectDir := t.TempDir()
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00001_create_orders.up.sql"), []byte(combined), 0o644); err != nil {
		t.Fatal(err)
	}

	// combined ≡ split ⇒ NO drift (the fix).
	report, err := Detect(projectDir, ordersServices(fields, nil))
	if err != nil {
		t.Fatalf("Detect (combined): %v", err)
	}
	if !report.Empty() {
		t.Fatalf("a combined-predicate CHECK must not drift against the split projection, got:\n%s", report.String())
	}

	// A genuinely different bound (proto now allows <= 13) STILL drifts.
	changed := []codegen.SchemaFieldDef{
		{Name: "name", Kind: "string"},
		{Name: "exp_month", Kind: "int32", Validate: &codegen.FieldConstraints{Gte: "1", Lte: "13"}},
	}
	report, err = Detect(projectDir, ordersServices(changed, nil))
	if err != nil {
		t.Fatalf("Detect (changed bound): %v", err)
	}
	if report.Empty() {
		t.Fatal("a changed upper bound (<= 13) must still report drift")
	}
	if len(report.Drifts) != 1 || report.Drifts[0].Column != "exp_month" {
		t.Fatalf("expected exactly one exp_month drift, got:\n%s", report.String())
	}
}

// TestDetect_AddedField covers the added-proto-field drift: a field the proto
// declares today with no matching column in the born migration.
func TestDetect_AddedField(t *testing.T) {
	if testing.Short() {
		t.Skip("Detect boots real postgres (shadow apply); skipped under -short")
	}

	born := []codegen.SchemaFieldDef{{Name: "name", Kind: "string"}}
	projectDir := t.TempDir()
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00001_create_orders.up.sql"),
		[]byte(bornMigration(t, born, nil)), 0o644); err != nil {
		t.Fatal(err)
	}

	// Proto now also declares discount_cents.
	now := []codegen.SchemaFieldDef{
		{Name: "name", Kind: "string"},
		{Name: "discount_cents", Kind: "int64"},
	}
	report, err := Detect(projectDir, ordersServices(now, nil))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(report.Drifts) != 1 || report.Drifts[0].Column != "discount_cents" {
		t.Fatalf("expected one added-field drift on discount_cents, got:\n%s", report.String())
	}
	sql := strings.Join(report.Drifts[0].SuggestedSQL, "\n")
	if !strings.Contains(sql, "ADD COLUMN discount_cents BIGINT") {
		t.Errorf("added-field suggestion should ADD COLUMN discount_cents; got:\n%s", sql)
	}
}
