package codegen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/naming"
)

// crud_convert_test.go — the SCHEMA-DEFAULT half of the create path.
//
// The rest of crud_convert.go is well covered by the tests named after
// the shapes they pin (crud_enum_test.go, crud_jsonb_test.go,
// scalar_shape_pairing_test.go, crud_managed_timestamp_test.go). What
// none of them reached is schemaDefaultAssigns and the literal projection
// under it: pgDefaultGoLiteral was 47% covered and goZeroLiteral was 0%,
// exercised only for the enum/TEXT case by crud_enum_unset_test.go. Every
// non-string arm — the integers, the floats, the booleans, the arrays,
// the `::type` cast postgres renders, and the refusal for a default no
// literal can express — ran in production and in no test.
//
// That is the sharpest place for a gap to be, because the function exists
// to fix a bug that ships silently. A `// forge:read-only` column is by
// construction absent from the create request, so the op leaves the Go
// field at its zero and Bun writes that zero. For a column whose DEFAULT
// is NOT the Go zero the row is invalid on arrival — an enum column born
// `NOT NULL DEFAULT 'TONE_WARM' CHECK (tone IN (...))` receives the empty
// string and violates the CHECK forge derived from the same proto. The
// generated code either writes the schema's literal or every create
// fails, out of the box, on a marker forge documents.
//
// The assertions below derive from bornScalarShapes() — the closed
// fifteen-kind vocabulary shared with scalar_shape_pairing_test.go — so a
// kind added there is checked here too rather than being covered by
// whichever types someone happened to list.

// defaultProjection is what the schema's DEFAULT for one canonical column
// type must project to on the entity struct.
type defaultProjection struct {
	// sqlDefault is the DEFAULT expression as postgres reports it.
	sqlDefault string
	// goLiteral is the Go literal the create op must assign.
	goLiteral string
	// zeroDefault is a DEFAULT whose value IS the Go zero — Bun writes
	// that anyway, so the op must emit NO assignment for it.
	zeroDefault string
}

// canonicalDefaults maps each canonical column type to a default worth
// projecting. Keyed by canonical type rather than by proto kind because
// that is what pgDefaultGoLiteral branches on: fifteen proto kinds
// collapse onto five canonical column types, and a table keyed by kind
// would asserts the same five arms three times over while claiming
// fifteen.
//
// `bytes` is deliberately absent: BYTEA has no Go literal spelling
// pgDefaultGoLiteral can produce, and its refusal is asserted separately
// (TestSchemaDefaults_UnprojectableDefaultIsAComment).
var canonicalDefaults = map[string]defaultProjection{
	"string":  {sqlDefault: "'DRAFT'", goLiteral: `"DRAFT"`, zeroDefault: "''"},
	"int64":   {sqlDefault: "42", goLiteral: "42", zeroDefault: "0"},
	"float64": {sqlDefault: "2.5", goLiteral: "2.5", zeroDefault: "0"},
	"bool":    {sqlDefault: "true", goLiteral: "true", zeroDefault: "false"},
}

// readOnlyEntity builds an entity whose create request carries NO
// fields, so every column reaches schemaDefaultAssigns — the
// `// forge:read-only` shape, which is the only way this code runs.
func readOnlyEntity(cols ...EntityColumn) (ServiceDef, Method, EntityDef) {
	svc := ServiceDef{
		Package: "orders.v1",
		Schemas: map[string][]SchemaFieldDef{
			"orders.v1.CreateOrderRequest": {},
		},
	}
	m := Method{Name: "CreateOrder", InputType: "CreateOrderRequest", InputTypeFQ: "orders.v1.CreateOrderRequest"}
	return svc, m, EntityDef{Name: "Order", TableName: "orders", Columns: cols}
}

// createAssigns runs the real create-assignment builder and fails on any
// unmapped pairing, so a test that meant to assert on emitted code can
// never quietly assert on an error path instead.
func createAssigns(t *testing.T, cols ...EntityColumn) []string {
	t.Helper()
	svc, m, entity := readOnlyEntity(cols...)
	assigns, unmapped := buildCreateAssigns(svc, m, entity)
	if err := UnmappedFieldsError(unmapped); err != nil {
		t.Fatalf("unexpected unmapped fields: %v", err)
	}
	return assigns
}

// TestSchemaDefaults_EveryScalarColumnDefaultIsWritten is the main pin:
// for every proto scalar kind in the closed vocabulary, a NOT NULL column
// carrying a non-zero literal DEFAULT that the create request does not
// supply must be assigned that literal in the generated create op.
//
// The kinds come from bornScalarShapes(), and the column each kind is
// born with comes from the same table (scalarCol), so this cannot drift
// from what entity birth actually emits.
func TestSchemaDefaults_EveryScalarColumnDefaultIsWritten(t *testing.T) {
	shapes := bornScalarShapes()
	if len(shapes) == 0 {
		t.Fatal("the born-scalar vocabulary is empty — this test would assert nothing and report green")
	}

	checked := 0
	for _, s := range shapes {
		want, ok := canonicalDefaults[s.canonical]
		if !ok {
			// bytes: no literal projection exists; see the refusal test.
			if s.canonical == "bytes" {
				continue
			}
			t.Fatalf("the scalar vocabulary grew canonical type %q (kind %s) and this test has no "+
				"default projection for it — add one to canonicalDefaults", s.canonical, s.kind)
		}
		checked++

		col := scalarCol("f_"+s.kind, s, false, true)
		col.Default = want.sqlDefault

		assigns := createAssigns(t, col)
		joined := strings.Join(assigns, "\n")
		// The Go field name is spelled by the SAME function the generator
		// uses, so this asserts the whole assignment rather than just the
		// presence of a literal that could belong to any field.
		wantAssign := fmt.Sprintf("e.%s = %s", naming.ToProtoPascalCase(col.Name), want.goLiteral)
		if !strings.Contains(joined, wantAssign) {
			t.Errorf("%s column (%s DEFAULT %s): create op does not write the schema default.\n"+
				"want an assignment %q\ngot:\n%s",
				s.kind, s.declType, want.sqlDefault, wantAssign, joined)
		}
	}
	if checked == 0 {
		t.Fatal("no scalar kind was actually checked — the projection table and the vocabulary do not intersect")
	}
}

// TestSchemaDefaults_ZeroValuedDefaultIsNotWritten pins the other half,
// and it is the half that keeps the generated code honest rather than
// merely correct: when the column's DEFAULT already IS the Go zero, Bun
// writes that value anyway, so emitting an assignment would be noise in
// every create op forge generates.
//
// This is goZeroLiteral's only caller, and it was 0% covered.
func TestSchemaDefaults_ZeroValuedDefaultIsNotWritten(t *testing.T) {
	for _, s := range bornScalarShapes() {
		want, ok := canonicalDefaults[s.canonical]
		if !ok {
			continue
		}
		col := scalarCol("f_"+s.kind, s, false, true)
		col.Default = want.zeroDefault

		assigns := createAssigns(t, col)
		for _, a := range assigns {
			if strings.HasPrefix(strings.TrimSpace(a), "e.") {
				t.Errorf("%s column (DEFAULT %s = the Go zero): create op emits %q, "+
					"but Bun writes the zero anyway — the assignment is noise",
					s.kind, want.zeroDefault, a)
			}
		}
	}
}

// TestSchemaDefaults_CastSuffixIsStripped pins that the `::type` cast
// postgres renders on a literal default is stripped before projection.
// Introspection reports `'DRAFT'::text`, not `'DRAFT'`; a projection that
// did not strip it would either emit the cast into Go source (which does
// not compile) or refuse the default and silently write the zero.
func TestSchemaDefaults_CastSuffixIsStripped(t *testing.T) {
	cases := []struct{ decl, canonical, sqlDefault, want string }{
		{"TEXT", "string", "'DRAFT'::text", `"DRAFT"`},
		{"TEXT[]", "string", "'{}'::text[]", "[]string{}"},
		{"JSONB", "json", "'{}'::jsonb", `"{}"`},
	}
	for _, tc := range cases {
		col := EntityColumn{
			Name: "f", Type: tc.canonical, DeclType: tc.decl,
			IsArray: strings.HasSuffix(tc.decl, "[]"), NotNull: true, Default: tc.sqlDefault,
		}
		assigns := createAssigns(t, col)
		joined := strings.Join(assigns, "\n")
		if !strings.Contains(joined, tc.want) {
			t.Errorf("%s DEFAULT %s: want the projection to carry %s; got:\n%s",
				tc.decl, tc.sqlDefault, tc.want, joined)
		}
		if strings.Contains(joined, "::") {
			t.Errorf("%s DEFAULT %s: the `::type` cast leaked into generated Go:\n%s",
				tc.decl, tc.sqlDefault, joined)
		}
	}
}

// TestSchemaDefaults_UnprojectableDefaultIsAComment pins the refusal.
// now() / gen_random_uuid() / nextval() must be produced by the DB, and a
// BYTEA literal has no Go spelling here — forge has nothing to write for
// any of them.
//
// What it must NOT do is invent a value: writing a Go zero for a column
// whose DEFAULT is gen_random_uuid() would store the empty string in
// every row while the schema says otherwise. The generated code says so
// in a comment instead, which is the same fail-loud-in-the-source
// discipline the rest of this file follows.
func TestSchemaDefaults_UnprojectableDefaultIsAComment(t *testing.T) {
	cases := []struct{ name, decl, canonical, sqlDefault string }{
		{"id", "TEXT", "string", "gen_random_uuid()"},
		{"seq", "BIGINT", "int64", "nextval('orders_seq')"},
		{"blob", "BYTEA", "bytes", `'\x00'`},
	}
	for _, tc := range cases {
		col := EntityColumn{
			Name: tc.name, Type: tc.canonical, DeclType: tc.decl,
			NotNull: true, Default: tc.sqlDefault,
		}
		assigns := createAssigns(t, col)
		joined := strings.Join(assigns, "\n")

		for _, a := range assigns {
			if strings.HasPrefix(strings.TrimSpace(a), "e.") {
				t.Errorf("%s DEFAULT %s: forge invented the value %q — the DB owns this default",
					tc.decl, tc.sqlDefault, a)
			}
		}
		if !strings.Contains(joined, tc.name) || !strings.Contains(joined, "//") {
			t.Errorf("%s DEFAULT %s: the unprojectable default must be named in a comment, "+
				"not dropped in silence; got:\n%s", tc.decl, tc.sqlDefault, joined)
		}
	}
}

// TestSchemaDefaults_SkipsColumnsTheDBOrRequestOwns pins the three
// exclusions. A primary key, a GENERATED ALWAYS column and a nullable
// column are none of the create op's business: the first two are the DB's
// to produce (writing them is an error, not a nicety), and a nullable
// column's absence is already meaningful.
func TestSchemaDefaults_SkipsColumnsTheDBOrRequestOwns(t *testing.T) {
	cases := []struct {
		what string
		col  EntityColumn
	}{
		{"primary key", EntityColumn{Name: "id", Type: "string", DeclType: "TEXT", IsPK: true, NotNull: true, Default: "'x'"}},
		{"generated", EntityColumn{Name: "total", Type: "int64", DeclType: "BIGINT", IsGenerated: true, NotNull: true, Default: "7"}},
		{"nullable", EntityColumn{Name: "note", Type: "string", DeclType: "TEXT", NotNull: false, Default: "'x'"}},
		{"managed timestamp", EntityColumn{Name: "created_at", Type: "time", DeclType: "TIMESTAMPTZ", NotNull: true, Default: "now()"}},
	}
	for _, tc := range cases {
		assigns := createAssigns(t, tc.col)
		if len(assigns) != 0 {
			t.Errorf("%s column: create op must emit nothing, got:\n%s",
				tc.what, strings.Join(assigns, "\n"))
		}
	}
}

// TestSchemaDefaults_RequestSuppliedColumnIsNotOverwritten pins the
// precedence, and it is the one failure here that would corrupt data
// rather than merely annoy: schemaDefaultAssigns runs over the columns
// the request did NOT carry. If it ran over all of them, the generated op
// would assign the caller's value and then clobber it with the schema
// default on the next line, so every create would silently discard user
// input for any column that has one.
func TestSchemaDefaults_RequestSuppliedColumnIsNotOverwritten(t *testing.T) {
	svc := ServiceDef{
		Package: "orders.v1",
		Schemas: map[string][]SchemaFieldDef{
			"orders.v1.CreateOrderRequest": {{Name: "status", Kind: "string"}},
		},
	}
	m := Method{Name: "CreateOrder", InputType: "CreateOrderRequest", InputTypeFQ: "orders.v1.CreateOrderRequest"}
	entity := EntityDef{
		Name: "Order", TableName: "orders",
		Columns: []EntityColumn{{
			Name: "status", Type: "string", DeclType: "TEXT",
			NotNull: true, Default: "'DRAFT'",
		}},
	}

	assigns, unmapped := buildCreateAssigns(svc, m, entity)
	if err := UnmappedFieldsError(unmapped); err != nil {
		t.Fatalf("unexpected unmapped fields: %v", err)
	}
	joined := strings.Join(assigns, "\n")
	if !strings.Contains(joined, "req.Status") {
		t.Fatalf("the request-supplied column must be assigned from the request; got:\n%s", joined)
	}
	if strings.Contains(joined, `"DRAFT"`) {
		t.Errorf("the schema default clobbers the caller's value — every create would discard "+
			"user input for this column; got:\n%s", joined)
	}
}
