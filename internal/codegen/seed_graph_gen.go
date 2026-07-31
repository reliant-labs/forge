package codegen

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/schemadef"
	"github.com/reliant-labs/forge/internal/seeddata"
	"github.com/reliant-labs/forge/internal/shadowdb"
)

// This is the generate-time FOLLOW-UP to the reverted runtime testkit.SeedGraph
// (see forge commit "revert runtime SeedGraph primitive"). A pkg helper could
// not seed an arbitrary FK spine at runtime — pkg/ is a separate module and a
// shipped-binary gate keeps the seed applier out of every binary. So instead of
// reusing the planner at runtime, forge BAKES the FK-closure INSERT SQL at
// generate time into a project-owned pkg/app/seedgraph_gen.go: a flow test calls
// app.SeedGraph(t, db, "cart_items") and gets the whole parent spine seeded from
// pre-rendered SQL, with NO runtime internal/seeddata dependency.
//
// It generalizes the CRUD-test-fixture machinery (crud_test_fixtures.go): the
// same schema introspection + seed planner, but keyed by an ARBITRARY root table
// and INCLUDING the root row (a flow test operates on the seeded graph), rather
// than one entity's parent closure with the root excluded.

// seedGraphSpec is one root table's baked seed: the rendered INSERTs for the
// root plus its transitive FK ancestors (parents first), and the row-0
// primary-key + column values so a test can reference the seeded rows.
type seedGraphSpec struct {
	root   string
	sql    string
	tables []seedGraphTableVals
}

type seedGraphTableVals struct {
	table string
	pk    string // row-0 primary-key value ("" when composite or none)
	cols  []seedGraphColVal
}

type seedGraphColVal struct{ col, val string }

// buildSeedGraphSpecs introspects the applied schema and builds a baked seed
// spec for every table's FK closure (root INCLUDED). Returns nil when the
// project has no migrations or introspection fails — the caller then skips
// emitting the helper (same graceful degradation as buildCRUDTestFixtures).
func buildSeedGraphSpecs(projectDir string) []seedGraphSpec {
	migDir := filepath.Join(projectDir, "db", "migrations")
	tables, err := schemadef.ApplyAndIntrospectAt(migDir, shadowdb.Resolve(projectDir))
	if err != nil || len(tables) == 0 {
		return nil
	}
	byName := make(map[string]schemadef.Table, len(tables))
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		byName[t.Name] = t
		names = append(names, t.Name)
	}
	sort.Strings(names)

	pools := seeddata.PoolsFromTables(tables)
	bounds := seeddata.BoundsFromTables(tables)
	vocab, _ := seeddata.LoadVocab(seeddata.VocabPath(migDir))

	var specs []seedGraphSpec
	for _, root := range names {
		closure := seedGraphClosure(byName, root)
		closureTables := make([]schemadef.Table, 0, len(closure))
		for _, n := range closure {
			closureTables = append(closureTables, byName[n])
		}
		// One connected happy-path spine: row 0 of every table references row 0
		// of its parents. Rows:1 keeps the seeded graph minimal and
		// deterministic (a flow test drives its own additional rows via RPCs).
		plan, perr := seeddata.BuildPlan(closureTables, pools, seeddata.Config{Rows: 1, Salt: 0})
		if perr != nil {
			// Unsatisfiable closure (a NOT NULL FK cycle) — equally unsatisfiable
			// for the app; skip this root, the rest still generate.
			continue
		}
		plan.SetBounds(bounds)
		plan.ApplyVocab(vocab)
		sql := strings.TrimSpace(strings.Join(plan.Statements(), "\n"))
		if sql == "" {
			continue
		}
		spec := seedGraphSpec{root: root, sql: sql}
		for _, n := range closure {
			tv := seedGraphTableVals{table: n}
			tbl := byName[n]
			if len(tbl.PKCols) >= 1 {
				if v, ok := plan.SeedValue(n, tbl.PKCols[0], 0); ok {
					tv.pk = v
				}
			}
			for _, c := range tbl.Columns {
				if v, ok := plan.SeedValue(n, c.Name, 0); ok {
					tv.cols = append(tv.cols, seedGraphColVal{col: c.Name, val: v})
				}
			}
			spec.tables = append(spec.tables, tv)
		}
		specs = append(specs, spec)
	}
	return specs
}

// seedGraphClosure returns root plus every table reachable from it over
// declared foreign keys (its transitive ancestors), root INCLUDED, restricted
// to tables present in byName. Self-references and edges back into the closure
// resolve per-row inside BuildPlan, so they are not walked. The result is
// sorted for deterministic emission.
func seedGraphClosure(byName map[string]schemadef.Table, root string) []string {
	seen := map[string]bool{root: true}
	var walk func(string)
	walk = func(name string) {
		for _, fk := range byName[name].ForeignKeys {
			if fk.RefTable == name || seen[fk.RefTable] {
				continue
			}
			if _, ok := byName[fk.RefTable]; !ok {
				continue
			}
			seen[fk.RefTable] = true
			walk(fk.RefTable)
		}
	}
	walk(root)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// renderSeedGraphFile renders the forge-owned pkg/app/seedgraph_gen.go from the
// baked specs. All strings go through strconv.Quote so seeded SQL / values with
// any character (newlines, quotes, backslashes) stay a valid Go literal.
func renderSeedGraphFile(specs []seedGraphSpec) []byte {
	var b strings.Builder
	b.WriteString("// Code generated by forge. DO NOT EDIT.\n")
	b.WriteString("// forge-owned: regenerated every run — do not edit (forge project disown to take ownership)\n")
	b.WriteString("// Source: the APPLIED schema (db/migrations), baked through forge's seed planner.\n")
	b.WriteString("//\n")
	b.WriteString("// SeedGraph seeds an FK spine for a flow/e2e test WITHOUT a runtime seed\n")
	b.WriteString("// dependency: the INSERT SQL is pre-rendered here at generate time. See the\n")
	b.WriteString("// forge `testing/flow` skill.\n")
	b.WriteString("package app\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"sort\"\n")
	b.WriteString("\t\"testing\"\n\n")
	b.WriteString("\t\"github.com/reliant-labs/forge/pkg/orm\"\n")
	b.WriteString(")\n\n")

	b.WriteString("type seedGraphSpec struct {\n")
	b.WriteString("\tsql    string\n")
	b.WriteString("\tpk     map[string]string\n")
	b.WriteString("\tvalues map[string]map[string]string\n")
	b.WriteString("}\n\n")

	b.WriteString("// seedGraphSpecs is the baked per-root seed data. Key: root table name.\n")
	b.WriteString("var seedGraphSpecs = map[string]seedGraphSpec{\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "\t%s: {\n", strconv.Quote(s.root))
		fmt.Fprintf(&b, "\t\tsql: %s,\n", strconv.Quote(s.sql))
		b.WriteString("\t\tpk: map[string]string{\n")
		for _, tv := range s.tables {
			if tv.pk != "" {
				fmt.Fprintf(&b, "\t\t\t%s: %s,\n", strconv.Quote(tv.table), strconv.Quote(tv.pk))
			}
		}
		b.WriteString("\t\t},\n")
		b.WriteString("\t\tvalues: map[string]map[string]string{\n")
		for _, tv := range s.tables {
			if len(tv.cols) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\t\t\t%s: {\n", strconv.Quote(tv.table))
			for _, cv := range tv.cols {
				fmt.Fprintf(&b, "\t\t\t\t%s: %s,\n", strconv.Quote(cv.col), strconv.Quote(cv.val))
			}
			b.WriteString("\t\t\t},\n")
		}
		b.WriteString("\t\t},\n")
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")

	b.WriteString(seedGraphAPISource)
	return []byte(b.String())
}

// seedGraphAPISource is the static (schema-independent) API over the baked
// specs. It carries no data, so it is a plain constant appended after the
// generated maps.
const seedGraphAPISource = `// SeededGraph is the handle SeedGraph returns: the seeded row-0 ids and column
// values of the root table and its FK ancestors.
type SeededGraph struct{ spec seedGraphSpec }

// SeedGraph seeds rootTable and its transitive foreign-key ancestors as one
// connected happy-path spine (row 0 -> parent row 0), and returns a handle to
// the seeded rows. Every row satisfies the
// migrations' CHECK / enum / char_length / range constraints (the SQL was baked
// from forge's seed planner at generate time), so a flow test gets a valid FK
// spine in one call instead of a hand-chained CreateX sequence — and with no
// runtime seed dependency.
//
// To set up a CREATE test, root at the PARENT table (its spine is seeded; you
// create the child via its RPC referencing g.PK("<parent>")). To set up a
// read/derived/update test, root at the leaf itself (the whole graph is seeded).
func SeedGraph(t testing.TB, db orm.Context, rootTable string) *SeededGraph {
	t.Helper()
	spec, ok := seedGraphSpecs[rootTable]
	if !ok {
		t.Fatalf("SeedGraph: no seed graph for table %q (known roots: %s)", rootTable, seedGraphRoots())
	}
	if _, err := db.Exec(context.Background(), spec.sql); err != nil {
		t.Fatalf("SeedGraph(%q): seeding failed: %v", rootTable, err)
	}
	return &SeededGraph{spec: spec}
}

// PK returns the seeded row-0 primary-key value of table (a table in the seeded
// closure) — hand it to an RPC to reference the seeded row. Panics if table was
// not part of this graph (a test-authoring error naming the wrong table).
func (g *SeededGraph) PK(table string) string {
	v, ok := g.spec.pk[table]
	if !ok {
		panic(fmt.Sprintf("SeededGraph.PK: table %q not in the seeded graph", table))
	}
	return v
}

// Value returns the seeded row-0 value of table.column, so an assertion reads
// the REAL seeded input instead of hard-coding it. Panics if the table/column
// was not seeded.
func (g *SeededGraph) Value(table, column string) string {
	cols, ok := g.spec.values[table]
	if !ok {
		panic(fmt.Sprintf("SeededGraph.Value: table %q not in the seeded graph", table))
	}
	v, ok := cols[column]
	if !ok {
		panic(fmt.Sprintf("SeededGraph.Value: column %q not seeded on table %q", column, table))
	}
	return v
}

// seedGraphRoots lists the table names SeedGraph can seed, for the not-found error.
func seedGraphRoots() string {
	roots := make([]string, 0, len(seedGraphSpecs))
	for r := range seedGraphSpecs {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	return fmt.Sprint(roots)
}
`

// GenerateSeedGraph writes the forge-owned pkg/app/seedgraph_gen.go with the
// baked FK-closure seed helper. It is forge-owned + checksum-tracked so it stays
// fresh as migrations evolve. It is a NO-OP (returns nil, writes nothing) when
// the project has no migrations or the schema can't be introspected — the same
// graceful degradation the CRUD-test fixtures use, so it never fails a generate
// run on a machine without a reachable shadow database.
func GenerateSeedGraph(projectDir, modulePath string, cs *checksums.FileChecksums) error {
	_ = modulePath // reserved: the current emission needs only pkg/orm, not the project module
	specs := buildSeedGraphSpecs(projectDir)
	if len(specs) == 0 {
		return nil
	}
	content := renderSeedGraphFile(specs)
	return writeForgeOwned(projectDir, filepath.Join("pkg", "app", "seedgraph_gen.go"), content, cs)
}
