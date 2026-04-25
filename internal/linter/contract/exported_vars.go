package contract

import (
	"go/ast"
	"go/token"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// ExportedVarsAnalyzer checks for exported package-level variables,
// allowing idiomatic exceptions like sentinel errors and compile-time
// interface checks.
var ExportedVarsAnalyzer = &analysis.Analyzer{
	Name:     "exportedvars",
	Doc:      "checks for exported package-level variables that should be methods or getter functions",
	Run:      runExportedVars,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

func runExportedVars(pass *analysis.Pass) (interface{}, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{(*ast.GenDecl)(nil)}
	insp.Preorder(nodeFilter, func(n ast.Node) {
		genDecl := n.(*ast.GenDecl)
		if genDecl.Tok != token.VAR {
			return
		}

		for _, spec := range genDecl.Specs {
			valueSpec := spec.(*ast.ValueSpec)
			for i, name := range valueSpec.Names {
				if !name.IsExported() {
					continue
				}

				// Exception: var _ Interface = (*Type)(nil) — compile-time interface check
				if name.Name == "_" {
					continue
				}

				// Exception: var Err* = errors.New(...) or fmt.Errorf(...)
				if isSentinelError(name.Name, valueSpec, i) {
					continue
				}

				pass.Reportf(name.Pos(),
					"exported package variable %s should be a method on a struct or a getter function",
					name.Name)
			}
		}
	})

	return nil, nil
}

// isSentinelError returns true if the variable looks like an idiomatic
// sentinel error: var ErrFoo = errors.New(...) or var ErrFoo = fmt.Errorf(...).
func isSentinelError(name string, spec *ast.ValueSpec, idx int) bool {
	if !strings.HasPrefix(name, "Err") {
		return false
	}

	// Must have an initializer.
	if idx >= len(spec.Values) {
		return false
	}

	callExpr, ok := spec.Values[idx].(*ast.CallExpr)
	if !ok {
		return false
	}

	return isCallTo(callExpr, "errors", "New") || isCallTo(callExpr, "fmt", "Errorf")
}

// isCallTo checks if a call expression is pkg.funcName(...).
func isCallTo(call *ast.CallExpr, pkg, funcName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == pkg && sel.Sel.Name == funcName
}
