package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// orm_qualifier_test.go pins how generated CRUD code names the ORM package
// when database.orm.package is not internal/db. The case is reliant's:
// `orm.package: internal/store` beside a hand-owned internal/db facade that
// handler files import as `db`. Qualifying the ORM as `db.` there either
// redeclares `db` in the handler file (the import forge appends collides
// with the facade's) or, when the file already imports the store, makes
// the appended shim name the facade's types.

const (
	storePath  = "example.com/test/internal/store"
	facadePath = "example.com/test/internal/db"
)

// storeProject is the crews fixture in a project whose ORM lives in
// internal/store.
func storeProject(t *testing.T) (projectDir, handlerDir string) {
	t.Helper()
	projectDir, handlerDir = ownerTestHandlerDir(t)
	yaml := "name: test\nmodule_path: example.com/test\ndatabase:\n  orm:\n    package: internal/store\n"
	if err := os.WriteFile(filepath.Join(projectDir, "forge.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir, handlerDir
}

// importBindings parses src and maps each identifier its imports bind to
// the import path, failing the test when a name is bound twice — the
// "redeclared in this block" the compiler would report.
func importBindings(t *testing.T, src string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "f.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if prev, dup := out[name]; dup {
			t.Fatalf("%q is bound twice (%s and %s):\n%s", name, prev, p, src)
		}
		out[name] = p
	}
	return out
}

// entityQualifiers returns every identifier src qualifies the ORM's Crew
// type with: the X of *X.Crew and X.Crew{}, pb's wire Crew aside.
func entityQualifiers(t *testing.T, src string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "f.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	out := map[string]bool{}
	note := func(e ast.Expr) {
		if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == "Crew" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name != "pb" {
				out[id.Name] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.StarExpr:
			note(n.X)
		case *ast.CompositeLit:
			note(n.Type)
		}
		return true
	})
	return out
}

// assertQualifiesStore fails unless src qualifies Crew only through a name
// bound to the store package, and binds no name twice.
func assertQualifiesStore(t *testing.T, src string) {
	t.Helper()
	bindings := importBindings(t, src)
	quals := entityQualifiers(t, src)
	if len(quals) == 0 {
		t.Fatalf("no X.Crew reference in:\n%s", src)
	}
	for q := range quals {
		if bindings[q] != storePath {
			t.Errorf("Crew is qualified as %s., which binds %q, not the ORM %q:\n%s", q, bindings[q], storePath, src)
		}
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestORMQualifier_BirthUsesPackageName: a relocated ORM is imported and
// qualified by its own name in both the generated ops file and the shim
// file forge scaffolds.
func TestORMQualifier_BirthUsesPackageName(t *testing.T) {
	projectDir, handlerDir := storeProject(t)
	generateCrews(t, projectDir)
	for _, name := range []string{"handlers_crud.go", "handlers_crud_ops_gen.go"} {
		src := readTestFile(t, filepath.Join(handlerDir, name))
		assertQualifiesStore(t, src)
		if !strings.Contains(src, "\t\""+storePath+"\"\n") {
			t.Errorf("%s: want the store imported unaliased:\n%s", name, src)
		}
		if !entityQualifiers(t, src)["store"] {
			t.Errorf("%s: want Crew qualified as store.:\n%s", name, src)
		}
	}
}

// handWrittenCrews is a handlers_crud.go that already implements every
// crews RPC but GetCrew and UpdateCrew (the shims that name the ORM type),
// with the given import block lines.
func handWrittenCrews(imports ...string) string {
	var b strings.Builder
	b.WriteString("package crews\n\nimport (\n\t\"context\"\n\n")
	for _, imp := range imports {
		b.WriteString("\t" + imp + "\n")
	}
	b.WriteString(")\n")
	for _, m := range []string{"CreateCrew", "ListCrews", "DeleteCrew"} {
		b.WriteString("\nfunc (s *Service) " + m + "(ctx context.Context, req any) (any, error) { return use(ctx), nil }\n")
	}
	return b.String()
}

// TestORMQualifier_AppendBesideFacade: the existing shim file imports the
// project's own internal/db facade as db. The appended shims must not
// import the store as db too.
func TestORMQualifier_AppendBesideFacade(t *testing.T) {
	projectDir, handlerDir := storeProject(t)
	shim := filepath.Join(handlerDir, "handlers_crud.go")
	existing := handWrittenCrews(strconv.Quote(facadePath)) + "\nvar use = db.Facade\n"
	if err := os.WriteFile(shim, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	generateCrews(t, projectDir)
	src := readTestFile(t, shim)
	if !strings.Contains(src, "func (s *Service) GetCrew(") {
		t.Fatalf("GetCrew was not appended:\n%s", src)
	}
	assertQualifiesStore(t, src)
	if importBindings(t, src)["db"] != facadePath {
		t.Errorf("the facade's db import must be left as written:\n%s", src)
	}
}

// TestORMQualifier_AppendReusesExistingBinding: whatever name the file
// already binds the store to is the one the appended shim uses — the
// unaliased store beside the facade, and a previously scaffolded
// `db "<store>"` alike — and no second store import is added.
func TestORMQualifier_AppendReusesExistingBinding(t *testing.T) {
	cases := map[string]struct {
		imports  []string
		wantQual string
	}{
		"unaliased store beside facade": {[]string{strconv.Quote(facadePath), strconv.Quote(storePath)}, "store"},
		"legacy db alias":               {[]string{"db " + strconv.Quote(storePath)}, "db"},
		"custom alias":                  {[]string{"st " + strconv.Quote(storePath)}, "st"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			projectDir, handlerDir := storeProject(t)
			shim := filepath.Join(handlerDir, "handlers_crud.go")
			if err := os.WriteFile(shim, []byte(handWrittenCrews(c.imports...)), 0o644); err != nil {
				t.Fatal(err)
			}
			generateCrews(t, projectDir)
			src := readTestFile(t, shim)
			assertQualifiesStore(t, src)
			if q := entityQualifiers(t, src); !q[c.wantQual] || len(q) != 1 {
				t.Errorf("want Crew qualified only as %s., got %v:\n%s", c.wantQual, q, src)
			}
			if n := strings.Count(src, strconv.Quote(storePath)); n != 1 {
				t.Errorf("store imported %d times, want 1:\n%s", n, src)
			}
		})
	}
}

// TestORMQualifier_DefaultProjectUnchanged: with the ORM at internal/db the
// generated files import it unaliased and qualify it as db., as they always
// have.
func TestORMQualifier_DefaultProjectUnchanged(t *testing.T) {
	projectDir, handlerDir := ownerTestHandlerDir(t)
	generateCrews(t, projectDir)
	for _, name := range []string{"handlers_crud.go", "handlers_crud_ops_gen.go"} {
		src := readTestFile(t, filepath.Join(handlerDir, name))
		if !strings.Contains(src, "\t\"example.com/test/internal/db\"\n") {
			t.Errorf("%s: want internal/db imported unaliased:\n%s", name, src)
		}
		if q := entityQualifiers(t, src); !q["db"] || len(q) != 1 {
			t.Errorf("%s: want Crew qualified only as db., got %v", name, q)
		}
	}
}
