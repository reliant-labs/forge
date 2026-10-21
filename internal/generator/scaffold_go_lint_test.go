package generator

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// Every Go file a fresh scaffold emits is gofmt-clean and takes its
// context.Context FIRST, for every project kind.
//
// Both are rules a brand-new project's own `forge lint` enforces: the format
// lane runs gofmt, and the scaffolded .golangci.yml enables revive's
// context-as-argument. A template that breaks either ships a project whose
// first `forge lint` is red in a file its author never opened. #1073 did it
// with login_broker.go's writePasswordRejection(w, ctx, ...), and the cli
// kind's cmd/<name>/version.go carried a space-indented doc-comment code
// block that gofmt rewrites. The e2e tests that run the real `forge lint`
// (internal/cli/scaffold_lint_clean_e2e_test.go) are off the pull-request
// tier and cover only the service kind.
//
// This is the unit-tier tripwire: the same two checks, over every Go file
// `forge project new` writes (plus the generate-time login_broker.go), in a
// `-short` run. The full golangci verdict stays with
// TestE2EFreshScaffoldLintExitsZero.
func TestFreshScaffold_GoIsFormattedAndTakesContextFirst(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		service   string
		frontend  string
		withLogin bool
	}{
		{kind: "service", service: "orders", frontend: "web", withLogin: true},
		{kind: "cli"},
		{kind: "library"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			name := tc.kind + "app"
			root := filepath.Join(t.TempDir(), name)
			g := NewProjectGenerator(name, root, "example.com/"+name)
			g.Kind = tc.kind
			g.ServiceName = tc.service
			g.FrontendName = tc.frontend
			if err := g.Generate(); err != nil {
				t.Fatalf("Generate(): %v", err)
			}
			if tc.withLogin {
				// login_broker.go is born by generate, not by project new.
				if err := codegen.GenerateLoginBroker("example.com/"+name, name, root); err != nil {
					t.Fatalf("GenerateLoginBroker: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "internal", "app", "login_broker.go")); err != nil {
					t.Fatalf("the login broker was not scaffolded, so this test would not cover it: %v", err)
				}
			}
			checkScaffoldedGo(t, root)
		})
	}
}

func checkScaffoldedGo(t *testing.T, root string) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		files++
		rel, _ := filepath.Rel(root, path)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// *_gen files are rewritten by generate, whose goimports pass formats
		// them; only owned (write-once) files keep their template's bytes.
		generated := strings.HasSuffix(path, "_gen.go") || strings.HasSuffix(path, "_gen_test.go")
		if formatted, err := format.Source(src); err != nil {
			t.Errorf("%s does not parse: %v", rel, err)
			return nil
		} else if !generated && !bytes.Equal(formatted, src) {
			t.Errorf("%s is not gofmt-clean: the new project's `forge lint` format lane fails on it", rel)
		}
		for _, v := range contextNotFirst(t, path, src) {
			t.Errorf("%s:%s: context.Context should be the first parameter (revive context-as-argument, enabled by the scaffolded .golangci.yml)", rel, v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scaffold: %v", err)
	}
	if files == 0 {
		t.Fatal("the scaffold wrote no Go files, so this test asserted nothing")
	}
}

// contextNotFirst returns the "line:col" positions of every function whose parameter
// list has a context.Context after a parameter of another type — revive's
// context-as-argument with no allowTypesBefore, as the scaffold configures it.
func contextNotFirst(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("scaffolded %s does not parse: %v", path, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		var params *ast.FieldList
		switch fn := n.(type) {
		case *ast.FuncDecl:
			params = fn.Type.Params
		case *ast.FuncLit:
			params = fn.Type.Params
		default:
			return true
		}
		if params == nil {
			return true
		}
		seenOther := false
		for _, field := range params.List {
			if isContextType(field.Type) {
				if seenOther {
					pos := fset.Position(field.Pos())
					out = append(out, fmt.Sprintf("%d:%d", pos.Line, pos.Column))
				}
				continue
			}
			seenOther = true
		}
		return true
	})
	return out
}

func isContextType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context" && sel.Sel.Name == "Context"
}
