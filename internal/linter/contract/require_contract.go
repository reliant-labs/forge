package contract

import (
	"go/ast"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// RequireContractAnalyzer checks that internal packages with exported methods
// on structs have a contract.go file defining the interface contract.
var RequireContractAnalyzer = &analysis.Analyzer{
	Name:     "requirecontract",
	Doc:      "checks that internal packages with exported struct methods have a contract.go file",
	Run:      runRequireContract,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

func runRequireContract(pass *analysis.Pass) (interface{}, error) {
	// Only check packages under internal/.
	pkgPath := pass.Pkg.Path()
	if !isInternalPackage(pkgPath) {
		return nil, nil
	}

	// Check if any struct has exported methods.
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	hasExportedMethods := false

	nodeFilter := []ast.Node{(*ast.FuncDecl)(nil)}
	insp.Preorder(nodeFilter, func(n ast.Node) {
		if hasExportedMethods {
			return
		}
		funcDecl := n.(*ast.FuncDecl)
		if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			return
		}
		if funcDecl.Name.IsExported() {
			hasExportedMethods = true
		}
	})

	if !hasExportedMethods {
		return nil, nil
	}

	// Check for contract.go in the package.
	hasContract := false
	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		if filepath.Base(filename) == "contract.go" {
			hasContract = true
			break
		}
	}

	if !hasContract {
		// Report on the package clause of the first file.
		if len(pass.Files) > 0 {
			pass.Reportf(pass.Files[0].Package,
				"package %s has exported methods but no contract.go",
				pass.Pkg.Name())
		}
	}

	return nil, nil
}

// isInternalPackage returns true if the package path contains an "internal/" segment.
func isInternalPackage(pkgPath string) bool {
	return strings.Contains(pkgPath, "/internal/") ||
		strings.HasPrefix(pkgPath, "internal/") ||
		pkgPath == "internal"
}
