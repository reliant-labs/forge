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

func init() {
	registerExcludeFlag(&ExportedVarsAnalyzer.Flags)
}

func runExportedVars(pass *analysis.Pass) (interface{}, error) {
	// Honor forge.yaml's contracts.exclude — packages opted out of contract
	// enforcement (e.g. //go:embed wrappers) should not be flagged.
	if IsExcluded(pass.Pkg.Path()) {
		return nil, nil
	}

	// Build a set of vars that are //go:embed targets. The inspector strips
	// directive-only comments from genDecl.Doc, so we walk file-level decls
	// directly to check for the //go:embed directive that must immediately
	// precede the var declaration (per the embed package contract).
	embedTargets := map[*ast.ValueSpec]bool{}
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			if !hasEmbedDirective(gd) {
				continue
			}
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					embedTargets[vs] = true
				}
			}
		}
	}

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

				// Exception: //go:embed targets are inherently package vars —
				// the embed package requires the var to be at file scope and
				// directly preceded by a //go:embed directive. There is no
				// way to expose an embed.FS through a getter without copying
				// it, so flagging these is a false positive.
				if embedTargets[valueSpec] {
					continue
				}

				// Exception: kubebuilder / controller-runtime API group
				// convention. Operators MUST expose `GroupVersion`,
				// `SchemeBuilder`, and `AddToScheme` as package-level vars
				// because controller-runtime's scheme registration
				// (`AddToScheme(scheme)`) and the codegen tooling discover
				// them by name. Wrapping these in a getter would break the
				// k8s API tooling contract.
				if isKubebuilderAPIVar(name.Name, valueSpec, i) {
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

// hasEmbedDirective reports whether the given var GenDecl is preceded by a
// //go:embed compiler directive. The directive must appear in the doc comment
// group immediately above the declaration (per the standard library "embed"
// package contract).
func hasEmbedDirective(gd *ast.GenDecl) bool {
	if gd.Doc == nil {
		return false
	}
	for _, c := range gd.Doc.List {
		if strings.HasPrefix(c.Text, "//go:embed ") || c.Text == "//go:embed" {
			return true
		}
	}
	return false
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

// isKubebuilderAPIVar returns true when the variable matches one of the
// three package-level vars that controller-runtime (and kubebuilder)
// require operators to expose verbatim:
//
//	GroupVersion   = schema.GroupVersion{...}
//	SchemeBuilder  = runtime.NewSchemeBuilder(...)
//	AddToScheme    = SchemeBuilder.AddToScheme
//
// These are discovered by name by the k8s API machinery, so wrapping them
// in a getter would silently break operator registration.
func isKubebuilderAPIVar(name string, spec *ast.ValueSpec, idx int) bool {
	switch name {
	case "GroupVersion":
		// Initializer must be schema.GroupVersion{...}
		if idx >= len(spec.Values) {
			return false
		}
		cl, ok := spec.Values[idx].(*ast.CompositeLit)
		if !ok {
			return false
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		return ident.Name == "schema" && sel.Sel.Name == "GroupVersion"
	case "SchemeBuilder":
		// Initializer must be runtime.NewSchemeBuilder(...) (kubebuilder
		// classic style) or &scheme.Builder{...} (controller-runtime style
		// emitted by `kubebuilder create api` when the project layout
		// uses sigs.k8s.io/controller-runtime/pkg/scheme).
		if idx >= len(spec.Values) {
			return false
		}
		switch v := spec.Values[idx].(type) {
		case *ast.CallExpr:
			return isCallTo(v, "runtime", "NewSchemeBuilder")
		case *ast.UnaryExpr:
			// &scheme.Builder{...}
			if v.Op != token.AND {
				return false
			}
			cl, ok := v.X.(*ast.CompositeLit)
			if !ok {
				return false
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return false
			}
			return ident.Name == "scheme" && sel.Sel.Name == "Builder"
		}
		return false
	case "AddToScheme":
		// Initializer must be a selector ending in `.AddToScheme` —
		// typically `SchemeBuilder.AddToScheme`. This is a method value,
		// not a call, so check for a SelectorExpr whose Sel is AddToScheme.
		if idx >= len(spec.Values) {
			return false
		}
		sel, ok := spec.Values[idx].(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return sel.Sel.Name == "AddToScheme"
	}
	return false
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
