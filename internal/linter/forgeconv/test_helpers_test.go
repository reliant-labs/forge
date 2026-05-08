package forgeconv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// mkdirAllImpl is split from forgeconv_test.go so the test helpers don't
// pull os/filepath into the main test surface; this also lets us swap to
// mocked impls if the linter gets a Filesystem abstraction later.
func mkdirAllImpl(dir string) error {
	return os.MkdirAll(filepath.Clean(dir), 0o755)
}

func writeFileImpl(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// mustParseGo parses a Go source snippet for predicate-level tests.
// The snippet must be a complete file; t.Fatalf on parse error so each
// test failure points at the snippet, not at downstream nil-deref.
//
// Used by handler_tests_use_tdd_test.go's isHandRolledTestsAssign
// predicate-only test surface; kept here so other predicate tests in
// the package can reuse the same one-liner.
func mustParseGo(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "snippet.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse snippet:\n%s\nerror: %v", src, err)
	}
	return fset, file
}

// anyAssign carries the result of evaluating isHandRolledTestsAssign on
// a single *ast.AssignStmt — kept as a struct so future predicate tests
// can extend the captured fields without changing every call site.
type anyAssign struct {
	stmt  *ast.AssignStmt
	match bool
}

// walkAssigns walks every short-form assignment in file and invokes fn
// once per node with the predicate already evaluated. Predicate tests
// stay declarative: they iterate, gate on `a.match`, and assert.
func walkAssigns(file *ast.File, fn func(*anyAssign)) {
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		fn(&anyAssign{
			stmt:  assign,
			match: isHandRolledTestsAssign(assign),
		})
		return true
	})
}
