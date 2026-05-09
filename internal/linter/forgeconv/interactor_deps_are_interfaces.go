// File: internal/linter/forgeconv/interactor_deps_are_interfaces.go
//
// The forgeconv-interactor-deps-are-interfaces analyzer warns when an
// interactor package declares a `Deps` struct field whose type is a
// concrete struct pointer (or a concrete struct value) rather than an
// interface. The marker `// forge:interactor` opts a package into the
// rule.
//
// Why this exists
//
// The interactor pattern earns its keep precisely because every
// collaborator is behind an interface — the workflow is unit-testable
// with all-mock deps. Concrete struct pointers in `Deps` defeat that:
// tests have to construct real adapter instances (or fork-and-mock the
// whole concrete type). The rule catches the foot-gun before the
// workflow grows tests around the wrong shape.
//
// Detection
//
// 1. Scan internal/<pkg>/ for files marked `// forge:interactor`.
// 2. In a marked package, find `type Deps struct { ... }`.
// 3. For each field, classify the type:
//      *T            (pointer to anything other than `interface{...}`) → fire
//      pkg.T         (selector type)                                    → fire
//      []T, map[K]T  → recurse on T
//      interface{}, named interfaces                                    → ok
//      Logger / context.Context style stdlib interfaces by name         → ok
// 4. The `*slog.Logger`-style logger is the one allowed concrete
//    pointer (loggers are pre-configured singletons; mocking is rare
//    and hand-rolled). We allow it by matching the field name
//    `Logger` rather than the type, which is loose but pragmatic.
//
// Severity is warning. The rule is opinionated and a project may
// legitimately need a concrete dep (e.g. a process-local cache), so
// we surface the design pressure without gating the build.

package forgeconv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LintInteractorDepsAreInterfaces walks rootDir/internal/ for packages
// marked `// forge:interactor`, locates each package's `type Deps
// struct`, and warns on every concrete-typed field that isn't an
// interface. Returns findings in deterministic order (file, then
// line). A missing internal/ tree is not an error.
func LintInteractorDepsAreInterfaces(rootDir string) (Result, error) {
	internalDir := filepath.Join(rootDir, "internal")
	if _, err := os.Stat(internalDir); os.IsNotExist(err) {
		return Result{}, nil
	}

	var pkgDirs []string
	err := filepath.WalkDir(internalDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" || d.Name() == "node_modules" || d.Name() == "vendor" {
			return filepath.SkipDir
		}
		entries, readErr := os.ReadDir(p)
		if readErr != nil {
			return nil
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				pkgDirs = append(pkgDirs, p)
				break
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("walk %s: %w", internalDir, err)
	}
	sort.Strings(pkgDirs)

	var result Result
	for _, dir := range pkgDirs {
		findings, lintErr := lintInteractorPkg(dir, rootDir)
		if lintErr != nil {
			return Result{}, lintErr
		}
		result.Findings = append(result.Findings, findings...)
	}

	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].File != result.Findings[j].File {
			return result.Findings[i].File < result.Findings[j].File
		}
		if result.Findings[i].Line != result.Findings[j].Line {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].Rule < result.Findings[j].Rule
	})
	return result, nil
}

// lintInteractorPkg parses every .go file in a package, looks for the
// `// forge:interactor` marker on any of them, and — when found —
// checks the `type Deps struct` for non-interface fields. The marker
// and the Deps struct may live in different files (typically
// contract.go for the marker, interactor.go for the struct), so we
// have to look at the package as a whole.
func lintInteractorPkg(pkgDir, rootDir string) ([]Finding, error) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pkgDir, err)
	}

	fset := token.NewFileSet()
	var (
		isInteractor bool
		// fileForDeps holds the AST file containing `type Deps struct`
		// (if any) and its on-disk path so findings can point at the
		// right spot. Only the FIRST Deps struct in the package is
		// considered — multiple is unusual and likely a mistake the
		// user wants to learn about separately.
		fileForDeps *ast.File
		depsPath    string
	)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			// Test files often declare local fake structs; not what
			// the rule targets.
			continue
		}
		fp := filepath.Join(pkgDir, e.Name())
		file, parseErr := parser.ParseFile(fset, fp, nil, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			continue
		}
		if hasInteractorMarker(file) {
			isInteractor = true
		}
		if fileForDeps == nil {
			if findDepsStruct(file) != nil {
				fileForDeps = file
				depsPath = fp
			}
		}
	}

	if !isInteractor || fileForDeps == nil {
		return nil, nil
	}

	deps := findDepsStruct(fileForDeps)
	if deps == nil || deps.Fields == nil {
		return nil, nil
	}

	rel, relErr := filepath.Rel(rootDir, depsPath)
	if relErr != nil {
		rel = depsPath
	}

	var findings []Finding
	for _, field := range deps.Fields.List {
		// Anonymous embedded fields (no Names) — skip; embedding a
		// concrete type is a different smell that this rule doesn't
		// own.
		if len(field.Names) == 0 {
			continue
		}
		// Allow the standard *slog.Logger field: it's always concrete
		// and projects rarely mock it.
		if isLoggerField(field) {
			continue
		}
		if isLikelyInterfaceType(field.Type) {
			continue
		}
		// Config-shaped collections of primitives are DATA (allow-lists,
		// feature-flag rosters, scalar limits-by-key) rather than
		// behavioral collaborators. They have no meaningful interface
		// equivalent — `[]string` doesn't get easier to mock by hiding
		// behind an interface. Skip them at the rule level so the
		// warning stays focused on real foot-guns (concrete struct
		// pointers, concrete adapter selectors).
		if isPrimitiveConfigShape(field.Type) {
			continue
		}
		// Report each concrete field separately so users see every
		// site that needs an interface lift.
		for _, n := range field.Names {
			line := fset.Position(n.NamePos).Line
			findings = append(findings, Finding{
				Rule:     "forgeconv-interactor-deps-are-interfaces",
				Severity: SeverityWarning,
				File:     rel,
				Line:     line,
				Message: fmt.Sprintf(
					"Deps field %q has concrete type %s; interactor deps should be interfaces so the workflow is testable with all-mock deps",
					n.Name, exprString(field.Type)),
				Remediation: "extract a small interface declaring just the methods this interactor needs from the dep, and reference the interface here. Skill: forge skill load interactor",
			})
		}
	}
	return findings, nil
}

// hasInteractorMarker reports whether the file's package doc / early
// comment groups carry the `forge:interactor` directive.
func hasInteractorMarker(f *ast.File) bool {
	if f.Doc != nil && containsForgeMarker(f.Doc.Text(), "interactor") {
		return true
	}
	pkgPos := f.Package
	for _, cg := range f.Comments {
		if cg.End() >= pkgPos {
			break
		}
		if containsForgeMarker(cg.Text(), "interactor") {
			return true
		}
	}
	return false
}

// findDepsStruct returns the *ast.StructType for the first
// `type Deps struct {...}` declaration in the file, or nil.
func findDepsStruct(f *ast.File) *ast.StructType {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Deps" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			return st
		}
	}
	return nil
}

// isLoggerField returns true for the canonical `Logger *slog.Logger`
// (or any field named exactly `Logger`). We carve loggers out because
// projects ship one shared *slog.Logger; mocking it is rare and
// usually hand-rolled rather than worth an interface. Config is treated
// the same way: bootstrap supplies one *config.Config singleton and
// interactor tests typically construct an inline Config{...} value
// rather than fork an interface.
func isLoggerField(field *ast.Field) bool {
	for _, n := range field.Names {
		switch n.Name {
		case "Logger", "Config":
			return true
		}
	}
	return false
}

// isLikelyInterfaceType is a syntactic predicate: it returns true for
// expression shapes that the rule treats as "interface-like":
//
//   - `interface{...}` literal
//   - `any` (universe ident)
//   - bare ident type names (treated as same-package interface OR
//     domain marker; the rule errs on the side of permissive — a
//     concrete struct named locally would only be a violation if it's
//     a struct declared in this package, which is a flag-day decision
//     each project owns)
//
// Pointer types are explicitly NOT interface-like (the foot-gun
// case): `*adapter.Service` looks like an interface to the eye but is
// a concrete struct pointer.
//
// Selector types (`pkg.T`) are NOT interface-like: from the linter's
// vantage we can't always tell if `pkg.T` is `interface` or `struct`
// without resolving across packages. We default to "needs an
// interface lift" and rely on the warning's low severity to absorb
// the false-positive risk on legitimately-imported interfaces.
func isLikelyInterfaceType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.InterfaceType:
		return true
	case *ast.Ident:
		// `any` is universe-defined as `interface{}`.
		if t.Name == "any" {
			return true
		}
		// Same-package bare ident: treat as interface for permissive
		// scoring. Interactors that define `type Source interface`
		// in contract.go and reference `Source` in Deps will be
		// accepted; an interactor that references a same-package
		// struct here will be a false-negative, but the failure mode
		// (workflow tests can mock it) is exactly what the rule cares
		// about. False-negative > false-positive at warning severity.
		return true
	}
	return false
}

// isPrimitiveConfigShape returns true for slice/map types whose element
// (and key, for maps) is a Go built-in primitive — the canonical shape
// for config DATA on a Deps struct. Examples that pass:
//
//	[]string, []int, []float64, []bool, [][]byte
//	map[string]string, map[string]int, ...
//
// Examples that don't (the rule still fires):
//
//	[]Source                 // slice of an interface type
//	[]*adapter.Client        // slice of concrete pointer
//	map[string]*config.Tier  // map to concrete pointer
//
// Rationale: these primitives have no meaningful interface equivalent;
// hiding `[]string` behind an interface adds friction without unlocking
// any test power. Recognizing the shape keeps the rule from training
// users to ignore noisy warnings on routine config fields.
func isPrimitiveConfigShape(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.ArrayType:
		// []byte is `*ast.ArrayType{Elt: Ident{"byte"}}`; [][]byte nests.
		return isPrimitiveType(t.Elt) || isByteSlice(t.Elt)
	case *ast.MapType:
		return isPrimitiveType(t.Key) && (isPrimitiveType(t.Value) || isByteSlice(t.Value))
	}
	return false
}

// isPrimitiveType returns true for Go built-in scalar types that are
// universally safe to ship by value on a Deps struct.
func isPrimitiveType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	switch id.Name {
	case "string", "bool",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "byte", "rune",
		"float32", "float64",
		"complex64", "complex128":
		return true
	}
	return false
}

// isByteSlice returns true for the `[]byte` shape — common as an
// inline secret/key/seed, treated as primitive for our purposes.
func isByteSlice(expr ast.Expr) bool {
	at, ok := expr.(*ast.ArrayType)
	if !ok {
		return false
	}
	id, ok := at.Elt.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "byte"
}

// exprString returns a short human-readable rendering of an
// expression for inclusion in the lint message. Avoids dragging in
// go/printer for what is fundamentally a one-line tag.
func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.InterfaceType:
		return "interface{...}"
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + exprString(t.Value)
	default:
		return fmt.Sprintf("%T", expr)
	}
}
