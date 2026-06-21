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

func init() {
	registerExcludeFlag(&RequireContractAnalyzer.Flags)
}

func runRequireContract(pass *analysis.Pass) (interface{}, error) {
	// Only check packages under internal/.
	pkgPath := pass.Pkg.Path()
	if !isInternalPackage(pkgPath) {
		return nil, nil
	}

	// Skip external test packages (`package <name>_test`). These exist only to
	// host black-box tests; their Test/Benchmark/Example functions and any
	// test helper structs are not part of the package's API surface by Go
	// convention, so requiring a contract.go here would be spurious and would
	// force users into the internal-test form, losing API-boundary discipline.
	// The package-under-test is analyzed separately and still subject to the
	// contract rule.
	if strings.HasSuffix(pass.Pkg.Name(), "_test") {
		return nil, nil
	}

	// Honor forge.yaml's contracts.exclude AND the per-package
	// //forge:exclude-contract header — these packages are intentionally kept
	// contract-free (utility packages with no behavioral interface), opted out
	// via either source.
	if IsExcludedPass(pass) {
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
