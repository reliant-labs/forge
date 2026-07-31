package codegen

import (
	"go/format"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/schemadef"
)

// TestRenderSeedGraphFile_ValidGo renders the seed-graph helper from
// hand-built specs (avoiding a DB) and asserts the output is valid, gofmt-clean
// Go carrying the baked SQL, pk, and values maps plus the static API.
func TestRenderSeedGraphFile_ValidGo(t *testing.T) {
	specs := []seedGraphSpec{
		{
			root: "cart_items",
			sql:  "INSERT INTO brands (id, name) VALUES ('b0', 'Acme');\nINSERT INTO carts (id, brand_id) VALUES ('c0', 'b0');\nINSERT INTO cart_items (id, cart_id) VALUES ('ci0', 'c0');",
			tables: []seedGraphTableVals{
				{table: "brands", pk: "b0", cols: []seedGraphColVal{{"id", "b0"}, {"name", "Acme"}}},
				{table: "carts", pk: "c0", cols: []seedGraphColVal{{"id", "c0"}, {"brand_id", "b0"}}},
				{table: "cart_items", pk: "ci0", cols: []seedGraphColVal{{"id", "ci0"}, {"cart_id", "c0"}}},
			},
		},
		{
			// A quote/newline-bearing value must survive as a valid Go literal.
			root:   "notes",
			sql:    "INSERT INTO notes (id, body) VALUES ('n0', 'line1\nline2 \"q\"');",
			tables: []seedGraphTableVals{{table: "notes", pk: "n0", cols: []seedGraphColVal{{"body", "line1\nline2 \"q\""}}}},
		},
	}

	out := renderSeedGraphFile(specs)
	if _, err := format.Source(out); err != nil {
		t.Fatalf("rendered seedgraph_gen.go is not valid Go: %v\n---\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{
		"package app",
		`"github.com/reliant-labs/forge/pkg/orm"`,
		"func SeedGraph(t testing.TB, db orm.Context, rootTable string) *SeededGraph",
		"func (g *SeededGraph) PK(table string) string",
		"func (g *SeededGraph) Value(table, column string) string",
		`"cart_items": {`,
		`"brand_id": "b0"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered seedgraph_gen.go missing %q", want)
		}
	}
}

func TestSeedGraphClosure_IncludesRootAndAncestors(t *testing.T) {
	// cart_items -> carts -> brands ; "unrelated" is off the graph.
	fk := func(col, refTable string) schemadef.ForeignKey {
		return schemadef.ForeignKey{Column: col, RefTable: refTable, RefColumn: "id"}
	}
	byName := map[string]schemadef.Table{
		"brands":     {Name: "brands"},
		"carts":      {Name: "carts", ForeignKeys: []schemadef.ForeignKey{fk("brand_id", "brands")}},
		"cart_items": {Name: "cart_items", ForeignKeys: []schemadef.ForeignKey{fk("cart_id", "carts")}},
		"unrelated":  {Name: "unrelated"},
	}
	got := seedGraphClosure(byName, "cart_items")
	want := map[string]bool{"cart_items": true, "carts": true, "brands": true}
	if len(got) != len(want) {
		t.Fatalf("closure = %v, want keys %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected table %q in closure %v", n, got)
		}
	}
}
