package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/schemadef"
	"github.com/reliant-labs/forge/pkg/seedplan"
)

// Typed entity factories: the TYPED counterpart to seedplan.SeedGraph.
//
// SeedGraph seeds an FK spine keyed by a raw TABLE name and hands back a
// string-keyed handle — the right primitive for a flow test that drives its
// own RPCs. But a handler/unit test that just needs "one valid <Entity> row to
// read/derive from" was left reverse-engineering the entity's `_orm.go` — which
// columns are NOT NULL, which are foreign keys, what a CHECK vocabulary allows —
// to hand-build a `db.Order{...}` literal and INSERT it. That reverse-engineering
// (nine `_orm.go` views for one dashboard test in the dogfood run) is exactly
// what a typed factory removes.
//
// New<Entity>(t, db, overrides…) inserts ONE valid row — every NOT-NULL column
// and FK parent satisfied from forge's seed planner — and returns the typed
// *db.<Entity>. Each call gets a fresh primary key, so it is repeatable where
// a SeedGraph root is a single fixed spine; the FK parent spine is seeded
// idempotently (ON CONFLICT DO NOTHING), and column overrides let a test set
// just the fields it asserts on:
//
//	o := NewOrder(t, database, func(o *db.Order) { o.Status = "shipped" })
//
// WHERE THEY LAND, AND WHY IT IS A `_test.go` FILE. Each entity's factory is
// emitted into the handler package that owns its CRUD RPCs, as
// internal/handlers/<svc>/factories_gen_test.go — beside that service's
// helpers_gen_test.go and in the same package clause. The `_test.go` suffix is
// load-bearing: the toolchain compiles such a file only into that package's
// test binary, so `testing` is never linked into cmd/. The factories used to
// live in their own non-test package (internal/testfactory) for importability,
// which meant a non-test .go file importing `testing` sat in the tree — safe
// only for as long as nothing in the production import graph reached it, a
// property guarded by a `go list -deps` test rather than by structure. Landing
// them beside their consumer makes the leak unrepresentable instead.
//
// THE VISIBILITY THIS BUYS AND COSTS. Go compiles a package's in-package
// _test.go files INTO the package under test, and the external `<svc>_test`
// package then imports that augmented package — so these exported factories are
// visible to BOTH `package <svc>` and `package <svc>_test` files in this
// directory, and to nothing else. That is the correct scope for a test factory
// and it is what the scaffolded CRUD lifecycle test (package <svc>_test, same
// directory) needs. A test in a DIFFERENT package cannot call them; such a test
// drives the service through its RPCs, which is the seam it should be using.
//
// Emission degrades gracefully: no migrations, an
// unreachable shadow DB, or an unsatisfiable FK closure just means the factory
// for that entity (or the whole file) isn't emitted — never a failed generate.

// entityFactorySpec is one entity's baked factory: the parent-closure seed SQL
// (idempotent), the single-row root INSERT with the PK column bound to $1 (so
// each call inserts a fresh row), and the identifiers the emitted Go references.
type entityFactorySpec struct {
	goName    string // "Order" — the db.<GoName> struct / db.Get<GoName>ByID / db.Update<GoName>
	lower     string // "order" — const/var name stem (camelCase for multi-word)
	parentSQL string // FK-ancestor INSERTs (root excluded), each ON CONFLICT DO NOTHING
	rootSQL   string // single root INSERT with the PK literal replaced by $1
}

// dbEntity is the table↔Go-name mapping parsed from the generated ORM package.
// The seed planner works in SQL (table names); the emitted factory works in Go
// (db.<GoName>, db.Get<GoName>ByID). The `_orm.go` structs are the authoritative
// join between the two, so we read them rather than re-deriving a Go name from a
// table name (pluralization isn't reliably invertible).
type dbEntity struct {
	goName   string
	table    string
	pkColumn string
	pkString bool // single string PK → factory-eligible (Get/Create take an id string)
}

// buildEntityFactorySpecs introspects the applied schema, joins it with the ORM
// structs' table↔Go-name mapping, and bakes a factory spec for every
// single-string-PK entity whose FK closure the seed planner can satisfy.
func buildEntityFactorySpecs(projectDir string) []entityFactorySpec {
	migDir := filepath.Join(projectDir, "db", "migrations")
	tables, err := schemadef.ApplyAndIntrospectAt(migDir, shadowdb.Resolve(projectDir))
	if err != nil || len(tables) == 0 {
		return nil
	}
	byName := make(map[string]schemadef.Table, len(tables))
	for _, t := range tables {
		byName[t.Name] = t
	}

	dbEntities := parseDBEntities(filepath.Join(projectDir, "internal", "db"))
	if len(dbEntities) == 0 {
		return nil
	}

	pools := seedplan.PoolsFromTables(tables)
	bounds := seedplan.BoundsFromTables(tables)
	vocab, _ := seedplan.LoadVocab(seedplan.VocabPath(migDir))

	var specs []entityFactorySpec
	roots := make([]string, 0, len(dbEntities))
	for table := range dbEntities {
		roots = append(roots, table)
	}
	sort.Strings(roots)

	for _, root := range roots {
		ent := dbEntities[root]
		if !ent.pkString {
			continue // Get/Create take a string id — non-string PKs are out of scope
		}
		tbl, ok := byName[root]
		if !ok || len(tbl.PKCols) != 1 {
			continue
		}
		spec, ok := bakeEntityFactory(byName, root, ent, pools, bounds, vocab)
		if ok {
			specs = append(specs, spec)
		}
	}
	return specs
}

// bakeEntityFactory renders one entity's parent + root SQL from a Rows:1 plan
// over its FK closure (root included). Returns ok=false when the closure is
// unsatisfiable or the root statement/PK can't be resolved — the caller then
// skips that entity, exactly as the seed-graph builder skips an unseedable root.
func bakeEntityFactory(byName map[string]schemadef.Table, root string, ent dbEntity, pools seedplan.EnumPools, bounds seedplan.CheckBounds, vocab *seedplan.Vocab) (entityFactorySpec, bool) {
	all := make([]schemadef.Table, 0, len(byName))
	for _, t := range byName {
		all = append(all, t)
	}
	closure := seedplan.FKClosure(all, root)
	closureTables := make([]schemadef.Table, 0, len(closure))
	for _, n := range closure {
		closureTables = append(closureTables, byName[n])
	}
	plan, err := seedplan.BuildPlan(closureTables, pools, seedplan.Config{Rows: 1, Salt: 0})
	if err != nil {
		return entityFactorySpec{}, false
	}
	plan.SetBounds(bounds)
	plan.ApplyVocab(vocab)

	rootStmt := ""
	var parentStmts []string
	rootPrefix := "INSERT INTO " + pgQuoteIdent(root) + " ("
	for _, stmt := range plan.Statements() {
		if strings.HasPrefix(stmt, rootPrefix) {
			rootStmt = stmt
		} else {
			parentStmts = append(parentStmts, strings.TrimSpace(stmt))
		}
	}
	if rootStmt == "" {
		return entityFactorySpec{}, false
	}

	// Bind the seeded PK literal to $1 so each factory call inserts a fresh row
	// (the Go side passes a new ULID). The PK is a synthesized unique value, so
	// its quoted literal occurs exactly once in the root statement.
	pkRaw, ok := plan.SeedValue(root, ent.pkColumn, 0)
	if !ok {
		return entityFactorySpec{}, false
	}
	pkLit := "'" + strings.ReplaceAll(pkRaw, "'", "''") + "'"
	if !strings.Contains(rootStmt, pkLit) {
		return entityFactorySpec{}, false
	}
	rootParamSQL := strings.Replace(strings.TrimSpace(rootStmt), pkLit, "$1", 1)

	return entityFactorySpec{
		goName:    ent.goName,
		lower:     lowerFirst(ent.goName),
		parentSQL: strings.Join(parentStmts, "\n"),
		rootSQL:   rootParamSQL,
	}, true
}

// parseDBEntities reads the generated internal/db/*.go ORM structs and returns
// the table↔Go-name mapping (plus the PK column and whether it is a single
// string PK). A struct is an entity when its embedded bun.BaseModel field
// carries a `bun:"table:<name>,…"` tag; the PK column is the field tagged `,pk`.
func parseDBEntities(dbDir string) map[string]dbEntity {
	out := map[string]dbEntity{}
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return out
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dbDir, name), nil, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				if ent, ok := entityFromStruct(ts.Name.Name, st); ok {
					out[ent.table] = ent
				}
			}
		}
	}
	return out
}

// entityFromStruct extracts the table name and PK metadata from one struct's
// bun tags, or ok=false when the struct isn't a bun entity (no table tag).
func entityFromStruct(goName string, st *ast.StructType) (dbEntity, bool) {
	ent := dbEntity{goName: goName}
	found := false
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag, terr := strconv.Unquote(f.Tag.Value)
		if terr != nil {
			continue
		}
		bunTag := reflect.StructTag(tag).Get("bun")
		if bunTag == "" {
			continue
		}
		parts := strings.Split(bunTag, ",")
		for _, p := range parts {
			if strings.HasPrefix(p, "table:") {
				ent.table = strings.TrimPrefix(p, "table:")
				found = true
			}
		}
		if hasTagOption(parts, "pk") {
			ent.pkColumn = parts[0]
			ent.pkString = isStringFieldType(f.Type)
		}
	}
	if !found || ent.table == "" || ent.pkColumn == "" {
		return dbEntity{}, false
	}
	return ent, true
}

func hasTagOption(parts []string, opt string) bool {
	for _, p := range parts {
		if p == opt {
			return true
		}
	}
	return false
}

// isStringFieldType reports whether a struct field's type is `string` (the PK
// shape db.Get<Entity>ByID / a ULID insert require). Pointer/other types are
// out of scope for the factory.
func isStringFieldType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "string"
}

// pgQuoteIdent mirrors seeddata's identifier quoting (postgres double-quotes)
// so the emitted root-statement prefix matches what the seed planner rendered.
func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// renderEntityFactoryFile renders one handler package's forge-owned
// factories_gen_test.go. All baked SQL goes through strconv.Quote so any
// character stays a valid Go string.
//
// pkgName is the handler package's own clause (`order`), so the file compiles
// INTO that package's test binary and its exported factories are visible to the
// directory's internal and external (`order_test`) test files alike.
func renderEntityFactoryFile(modulePath, pkgName string, specs []entityFactorySpec) []byte {
	var b strings.Builder
	b.WriteString("// Code generated by forge. DO NOT EDIT.\n")
	b.WriteString("// forge-owned: regenerated every run — do not edit (forge project disown to take ownership)\n")
	b.WriteString("// Source: the APPLIED schema (db/migrations), baked through forge's seed planner.\n")
	b.WriteString("//\n")
	b.WriteString("// Typed entity factories for tests. New<Entity>(t, db, overrides…) inserts one\n")
	b.WriteString("// valid row — every NOT-NULL column and FK parent satisfied — and returns the\n")
	b.WriteString("// typed *db.<Entity>. Each call gets a fresh primary key, so call it once per\n")
	b.WriteString("// row:\n")
	b.WriteString("//\n")
	b.WriteString("//\titem := NewItem(t, database)\n")
	b.WriteString("//\n")
	b.WriteString("// WHY THIS IS A `_test.go` FILE. The toolchain compiles it only into this\n")
	b.WriteString("// package's test binary, so the `testing` import below is never linked into\n")
	b.WriteString("// cmd/. These factories used to live in their own non-test package\n")
	b.WriteString("// (internal/testfactory) to be importable from anywhere; that made a non-test\n")
	b.WriteString("// .go file importing `testing` part of the tree, safe only while nothing in\n")
	b.WriteString("// the production import graph happened to reach it.\n")
	b.WriteString("//\n")
	b.WriteString("// The package clause is `" + pkgName + "`, NOT `" + pkgName + "_test`. Go compiles a package's\n")
	b.WriteString("// in-package _test.go files INTO the package under test, and the external\n")
	b.WriteString("// `" + pkgName + "_test` package then imports that augmented package — so these\n")
	b.WriteString("// factories are visible to BOTH the internal and the external test files in\n")
	b.WriteString("// this directory (including the scaffolded CRUD lifecycle test), and to no\n")
	b.WriteString("// other package. See the forge `testing` skill.\n")
	b.WriteString("package " + pkgName + "\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	b.WriteString("\t\"testing\"\n\n")
	b.WriteString("\t\"github.com/oklog/ulid/v2\"\n")
	b.WriteString("\t\"github.com/reliant-labs/forge/pkg/orm\"\n\n")
	fmt.Fprintf(&b, "\tdb %s\n", strconv.Quote(modulePath+"/internal/db"))
	b.WriteString(")\n\n")

	b.WriteString(entityFactoryHelperSource)

	for _, s := range specs {
		fmt.Fprintf(&b, "\n// --- %s ---\n\n", s.goName)
		fmt.Fprintf(&b, "// %sOverride mutates the *db.%s New%s is about to insert.\n", s.goName, s.goName, s.goName)
		fmt.Fprintf(&b, "type %sOverride func(*db.%s)\n\n", s.goName, s.goName)

		if s.parentSQL != "" {
			fmt.Fprintf(&b, "const %sFactoryParentSQL = %s\n\n", s.lower, backquoteOrQuote(s.parentSQL))
		}
		fmt.Fprintf(&b, "const %sFactoryRootSQL = %s\n\n", s.lower, backquoteOrQuote(s.rootSQL))

		fmt.Fprintf(&b, "// New%s inserts one %s row with every NOT-NULL column and FK parent\n", s.goName, s.goName)
		b.WriteString("// satisfied (forge's seed planner), and returns it. Each call gets a fresh\n")
		b.WriteString("// primary key, so call it once per row you need. Override the columns your\n")
		b.WriteString("// test asserts on; leave the rest to the seeded defaults:\n")
		b.WriteString("//\n")
		fmt.Fprintf(&b, "//\t%s := New%s(t, database, func(x *db.%s) { /* x.Field = … */ })\n", s.lower, s.goName, s.goName)
		b.WriteString("//\n")
		b.WriteString("// A single-column-unique NOT-NULL field other than the primary key keeps its\n")
		b.WriteString("// seeded value across calls — override such a field yourself to insert more\n")
		b.WriteString("// than one row.\n")
		fmt.Fprintf(&b, "func New%s(t testing.TB, database orm.Context, overrides ...%sOverride) *db.%s {\n", s.goName, s.goName, s.goName)
		b.WriteString("\tt.Helper()\n")
		if s.parentSQL != "" {
			fmt.Fprintf(&b, "\tseedFactoryParents(t, database, %sFactoryParentSQL)\n", s.lower)
		}
		b.WriteString("\tid := ulid.Make().String()\n")
		fmt.Fprintf(&b, "\tif _, err := database.Exec(context.Background(), %sFactoryRootSQL, id); err != nil {\n", s.lower)
		fmt.Fprintf(&b, "\t\tt.Fatalf(\"New%s: insert row: %%v\", err)\n", s.goName)
		b.WriteString("\t}\n")
		fmt.Fprintf(&b, "\trow, err := db.Get%sByID(context.Background(), database, id)\n", s.goName)
		b.WriteString("\tif err != nil {\n")
		fmt.Fprintf(&b, "\t\tt.Fatalf(\"New%s: load inserted row: %%v\", err)\n", s.goName)
		b.WriteString("\t}\n")
		b.WriteString("\tif len(overrides) > 0 {\n")
		b.WriteString("\t\tfor _, o := range overrides {\n")
		b.WriteString("\t\t\to(row)\n")
		b.WriteString("\t\t}\n")
		fmt.Fprintf(&b, "\t\tif err := db.Update%s(context.Background(), database, row); err != nil {\n", s.goName)
		fmt.Fprintf(&b, "\t\t\tt.Fatalf(\"New%s: apply overrides: %%v\", err)\n", s.goName)
		b.WriteString("\t\t}\n")
		b.WriteString("\t}\n")
		b.WriteString("\treturn row\n")
		b.WriteString("}\n")
	}
	return []byte(b.String())
}

// entityFactoryHelperSource is the schema-independent shared helper.
const entityFactoryHelperSource = `// seedFactoryParents seeds an entity's FK-ancestor spine. Every INSERT is
// ON CONFLICT DO NOTHING, so repeated factory calls (and factories for sibling
// entities that share ancestors) converge on one shared parent spine.
func seedFactoryParents(t testing.TB, database orm.Context, sql string) {
	t.Helper()
	if sql == "" {
		return
	}
	if _, err := database.Exec(context.Background(), sql); err != nil {
		t.Fatalf("factory: seed FK parents: %v", err)
	}
}
`

// backquoteOrQuote renders s as a raw (backtick) Go string literal when it
// contains no backtick, else falls back to strconv.Quote. Baked seed SQL is
// multi-line, so a raw literal keeps it readable in the generated file.
func backquoteOrQuote(s string) string {
	if !strings.Contains(s, "`") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// GenerateEntityFactories writes one forge-owned factories_gen_test.go per
// handler package, holding the typed New<Entity> factories for the entities
// that package's CRUD RPCs own.
//
// Best-effort and forge-owned + checksum-tracked: a no-op (writes nothing,
// returns nil) when there are no factory-eligible entities or the schema can't
// be introspected, so it never fails a generate run on a machine without a
// reachable shadow database.
//
// An entity whose CRUD RPCs no service declares is SKIPPED rather than parked
// in a shared package. The factory's whole value is being callable from the
// test that exercises that entity's handlers, and Go's in-package-_test.go
// visibility rule means it can only be called from the directory it lands in —
// so an entity with no owning handler package has no directory that could use
// it. Emitting one anyway would reintroduce exactly the importable-from-
// anywhere non-test package this move removed.
func GenerateEntityFactories(projectDir, modulePath string, services []ServiceDef, cs *checksums.FileChecksums) error {
	specs := buildEntityFactorySpecs(projectDir)
	if len(specs) == 0 {
		return nil
	}

	byService := groupFactorySpecsByService(projectDir, services, specs)
	for _, dir := range sortedFactoryDirs(byService) {
		group := byService[dir]
		content := renderEntityFactoryFile(modulePath, group.pkg, group.specs)
		rel, err := filepath.Rel(projectDir, filepath.Join(dir, entityFactoryFile))
		if err != nil {
			return fmt.Errorf("resolve factory path for %s: %w", group.pkg, err)
		}
		if err := writeForgeOwned(projectDir, rel, content, cs); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

// entityFactoryFile is the file name emitted into each handler package. The
// `_test.go` suffix is load-bearing — see the file header.
const entityFactoryFile = "factories_gen_test.go"

// factoryGroup is one handler package's share of the factories: the package
// clause to render under, and the entity specs it owns.
type factoryGroup struct {
	pkg   string
	specs []entityFactorySpec
}

// groupFactorySpecsByService routes each entity spec to the handler directory
// whose service declares that entity's CRUD RPCs, keyed by absolute dir.
//
// The routing question — "which service owns entity E?" — is the same one
// MatchCRUDMethods answers when it pairs a CRUD method with an entity, and it
// is answered from the same evidence: a method named Create<E>/Get<E>/
// Update<E>/Delete<E>/List<Es> on that service. Re-using ParseCRUDOperation
// keeps the two from drifting into disagreeing about ownership.
//
// An entity claimed by two services lands in both. That is deliberate: each
// package's factory is private to its own test binary, so there is no
// redeclaration between them, and a test in either directory can seed the row
// it needs.
func groupFactorySpecsByService(projectDir string, services []ServiceDef, specs []entityFactorySpec) map[string]factoryGroup {
	out := map[string]factoryGroup{}
	for _, svc := range services {
		owned := ownedFactorySpecs(svc, specs)
		if len(owned) == 0 {
			continue
		}
		res, err := ResolveServiceComponent(projectDir, svc.Name)
		if err != nil {
			// No handler dir on disk (declared but not yet scaffolded).
			// Skipping matches renderComponentTestHelpers rather than
			// conjuring a stray directory.
			continue
		}
		g := out[res.Dir]
		g.pkg = res.PackageName
		g.specs = append(g.specs, owned...)
		out[res.Dir] = g
	}
	return out
}

// ownedFactorySpecs returns the specs for entities svc declares CRUD RPCs for.
func ownedFactorySpecs(svc ServiceDef, specs []entityFactorySpec) []entityFactorySpec {
	claimed := map[string]bool{}
	for _, m := range svc.Methods {
		if m.ClientStreaming || m.ServerStreaming {
			continue // CRUD is unary only, same as MatchCRUDMethods
		}
		op, entityName := parseCRUDOperation(m.Name)
		if op == "" {
			continue
		}
		claimed[strings.ToLower(entityName)] = true
		// `ListOrders` names the entity in the plural; the spec is keyed
		// by the singular Go type name.
		if op == "list" {
			claimed[strings.ToLower(inflection.Singular(entityName))] = true
		}
	}
	var owned []entityFactorySpec
	for _, s := range specs {
		if claimed[strings.ToLower(s.goName)] {
			owned = append(owned, s)
		}
	}
	return owned
}

// sortedFactoryDirs orders the emission targets so a run's writes are
// deterministic (the generate pipeline's idempotency check compares bytes).
func sortedFactoryDirs(byService map[string]factoryGroup) []string {
	dirs := make([]string, 0, len(byService))
	for dir := range byService {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// RetiredEntityFactoryRelPath is the pre-move location of the entity factories:
// their own non-test package, which every project scaffolded before this change
// still carries. GenerateEntityFactories' caller retires it so an upgrading
// project does not end up with both copies — the old one being a non-test .go
// file that imports `testing`, which is the shape this move exists to remove.
func RetiredEntityFactoryRelPath() string {
	return filepath.Join("internal", "testfactory", "factory_gen.go")
}
