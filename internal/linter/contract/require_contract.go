package contract

import (
	"go/ast"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
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

	// Skip proto-service handler packages (internal/handlers/<svc>). By forge
	// convention these are thin-translation Connect handlers: the package's
	// exported methods are the proto-defined RPC methods (GetCurrentUser, ...)
	// plus framework glue (Name/Register/RegisterHTTP), and the package
	// IMPLEMENTS the generated Connect handler interface
	// (<svc>v1connect.<Svc>ServiceHandler). Their contract is the proto service,
	// not a hand-written Go contract.go — business logic lives in a separate
	// domain package (internal/<svc>), which this rule still covers. Requiring a
	// contract.go here would duplicate the proto boundary. See the api-handlers
	// skill. (When a handler package DOES declare a contract.go the single-seam
	// Analyzer still enforces it; this only removes the *requirement*.)
	if isHandlerPackage(pkgPath) {
		return nil, nil
	}

	// Honor forge.yaml's contracts.exclude AND the per-package
	// //forge:exclude-contract header — these packages are intentionally kept
	// contract-free (utility packages with no behavioral interface), opted out
	// via either source.
	if IsExcludedPass(pass) {
		return nil, nil
	}

	// Check if any struct has exported methods — but ONLY in hand-written
	// files. Forge-generated files (e.g. authorizer_gen.go's Can/CanAccess/
	// NewGeneratedAuthorizer, *_mock.go's mock methods) are the codegen OUTPUT
	// of a contract, not a hand-authored behavioral surface, so they must not
	// force a contract.go. A handler package whose only exported methods come
	// from authorizer_gen.go is contract-defined by its proto service, not by a
	// Go contract.go. Skipping generated files here keeps the rule pointed at
	// genuine hand-written service packages.
	//
	// "Generated" is detected two ways (union): the canonical
	// `// Code generated ... DO NOT EDIT.` header (ast.IsGenerated), which
	// covers files like handlers/mocks/*_mock.go that don't use the _gen.go
	// suffix, AND the `*_gen.go` filename convention as a belt-and-suspenders
	// fallback.
	hasExportedMethods := false
	for _, file := range pass.Files {
		if hasExportedMethods {
			break
		}
		filename := pass.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(filename, "_gen.go") || ast.IsGenerated(file) {
			continue
		}
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
				continue
			}
			if funcDecl.Name.IsExported() {
				hasExportedMethods = true
				break
			}
		}
	}

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

// isHandlerPackage returns true for proto-service handler packages, which by
// forge convention live under an `internal/handlers/` directory. Their contract
// is the proto service (the package implements the generated Connect handler
// interface), so they are exempt from the require-contract.go rule.
func isHandlerPackage(pkgPath string) bool {
	return strings.Contains(pkgPath, "/internal/handlers/") ||
		strings.HasPrefix(pkgPath, "internal/handlers/")
}
