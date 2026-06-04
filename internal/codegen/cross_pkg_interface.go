// File: internal/codegen/cross_pkg_interface.go
//
// Cross-package interface resolution for the testing.go auto-stub
// generator. The locally-declared interface path lives in
// interface_parser.go; this file handles the other half: when a
// service's Deps field is typed as `pkg.RepositoryName` (selector,
// not a bare identifier), we have to chase the `pkg` alias to its
// import path, load that package, and dig out the named interface.
//
// Failure mode is deliberately soft: if the import alias can't be
// resolved, the package can't load, or the named type isn't an
// interface, we return ok=false and the caller falls through to
// "field stays nil + emit a TODO marker". The auto-stub feature is
// a convenience layered on top of "user can hand-roll a stub" — the
// failure path must not break the test scaffold for projects whose
// repo packages happen to be temporarily un-buildable.
//
// We deliberately do not vendor the loaded package's source into the
// generated testing.go. We only read enough type information to
// emit a stub struct with the right method signatures.

package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// CrossPkgInterfaceResult bundles everything the auto-stub emitter
// needs to satisfy a selector-typed Deps field. ImportPath is the
// canonical import path of the package declaring the interface (so
// the testing.go imports block can pick up a new entry). NeededImports
// lists every extra import path the stub's method signatures
// reference (e.g. an `*orm.Context` parameter contributes orm's
// path). NeededImports is map[importPath]alias, where alias is the
// suggested local alias (typically the package's declared name).
type CrossPkgInterfaceResult struct {
	// PackagePath is the import path of the package declaring the
	// interface (e.g. "example.com/proj/internal/repo").
	PackagePath string
	// PackageName is the package's declared Go name (the qualifier
	// the testing.go file should use for the interface, e.g. "repo").
	PackageName string
	// Methods is the flattened method set of the interface, with each
	// method's parameter / result types fully qualified using package
	// aliases that appear in NeededImports.
	Methods []InterfaceMethod
	// NeededImports maps every extra import path the stub's method
	// signatures reference to the local alias used in the rendered
	// signatures. The interface's own package is INCLUDED here so
	// callers can fold one map into the file's import block.
	NeededImports map[string]string
}

// ResolveCrossPkgInterface attempts to locate `<pkgAlias>.<typeName>` —
// where pkgAlias was imported by some file in handlerDir — and return
// the data the bootstrap_testing.go template needs to emit a
// satisfying stub struct.
//
// Returns ok=false on any failure mode (alias not found, package
// can't load, type isn't an interface, etc.). The caller should
// treat ok=false the same as "no stub possible" and skip the field —
// the generated testing.go will leave it nil and rely on the user
// overriding via With<Svc>Deps when a test cares.
//
// Implementation outline:
//
//  1. Parse every non-test, non-_gen.go file in handlerDir to build
//     a map alias -> importPath. We take the FIRST alias declaration
//     we see; in practice each package is imported with one alias
//     per file and aliases agree across files in a well-formed
//     package.
//  2. Resolve pkgAlias to an import path.
//  3. Use golang.org/x/tools/go/packages to load that path, with
//     cfg.Dir = handlerDir so module resolution works (go.mod in
//     the project root governs the lookup).
//  4. Look the named type up in the package's types scope. Reject
//     anything not an interface.
//  5. Walk the interface's method set (which Go's types package
//     pre-flattens — embedded methods are included automatically).
//  6. Render each method's signature with types.TypeString plus a
//     custom Qualifier that records every package the signature
//     references AND returns the right alias for the rendered text.
func ResolveCrossPkgInterface(handlerDir, pkgAlias, typeName string) (CrossPkgInterfaceResult, bool) {
	imports, err := collectImports(handlerDir)
	if err != nil || len(imports) == 0 {
		return CrossPkgInterfaceResult{}, false
	}
	importPath, ok := imports[pkgAlias]
	if !ok {
		return CrossPkgInterfaceResult{}, false
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedDeps | packages.NeedImports | packages.NeedSyntax,
		Dir: handlerDir,
	}
	pkgs, err := packages.Load(cfg, importPath)
	if err != nil || len(pkgs) == 0 {
		return CrossPkgInterfaceResult{}, false
	}
	loaded := pkgs[0]
	// Refuse to swallow type errors silently: if the package didn't
	// type-check, the method-set walk below would return a partial
	// answer (zero or wrong-arity methods). Better to skip and let
	// the user hand-roll a stub than emit a struct that doesn't
	// satisfy the interface at all.
	if len(loaded.Errors) > 0 || loaded.Types == nil {
		return CrossPkgInterfaceResult{}, false
	}

	obj := loaded.Types.Scope().Lookup(typeName)
	if obj == nil {
		return CrossPkgInterfaceResult{}, false
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return CrossPkgInterfaceResult{}, false
	}
	iface, ok := tn.Type().Underlying().(*types.Interface)
	if !ok {
		return CrossPkgInterfaceResult{}, false
	}

	// Pre-seed NeededImports with the interface's own package so the
	// stub's `<alias>.<TypeName>` reference resolves. Use the package's
	// declared name as the alias — same convention we already use for
	// imported handler packages.
	needed := map[string]string{
		importPath: loaded.Types.Name(),
	}

	// Render each method. types.NewMethodSet on the interface returns
	// the COMPLETE method set including embedded interfaces (Go's
	// types package handles the flattening), which is exactly what
	// we need — the resolve() recursion in ParseLocalInterfaces is
	// unnecessary here.
	//
	// We iterate iface.NumMethods()/Method(i) instead of NewMethodSet
	// because for an interface type, Method(i) returns the declared
	// methods in source order, while NewMethodSet sorts
	// alphabetically. Source order matches what users expect from
	// the generated stub and matches the existing local-interface
	// path's behavior.
	var methods []InterfaceMethod
	qualifier := makeQualifier(needed)
	for i := 0; i < iface.NumMethods(); i++ {
		fn := iface.Method(i)
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			continue
		}
		m := InterfaceMethod{Name: fn.Name()}

		// Params: render each tuple element with our qualifier.
		var paramParts []string
		for j := 0; j < sig.Params().Len(); j++ {
			p := sig.Params().At(j)
			tStr := types.TypeString(p.Type(), qualifier)
			name := p.Name()
			if name == "" {
				name = fmt.Sprintf("p%d", j)
			}
			paramParts = append(paramParts, name+" "+tStr)
		}
		// Variadic last param: types.Signature.Variadic() is true
		// when the last param is a slice rendered as `...T`. We have
		// to convert []T → ...T by hand since types.TypeString gives
		// us "[]T" by default.
		if sig.Variadic() && len(paramParts) > 0 {
			last := paramParts[len(paramParts)-1]
			last = strings.Replace(last, "[]", "...", 1)
			paramParts[len(paramParts)-1] = last
		}
		m.Params = strings.Join(paramParts, ", ")

		// Results: collect types, defer the parens decision to match
		// the existing buildInterfaceMethod shape.
		var resultTypes []string
		for j := 0; j < sig.Results().Len(); j++ {
			r := sig.Results().At(j)
			tStr := types.TypeString(r.Type(), qualifier)
			resultTypes = append(resultTypes, tStr)
		}
		switch len(resultTypes) {
		case 0:
			m.Results = ""
			m.ReturnStatement = ""
		case 1:
			m.Results = resultTypes[0]
			m.ReturnStatement = "return " + zeroValueForType(resultTypes[0])
		default:
			m.Results = "(" + strings.Join(resultTypes, ", ") + ")"
			var zeroes []string
			for _, t := range resultTypes {
				zeroes = append(zeroes, zeroValueForType(t))
			}
			m.ReturnStatement = "return " + strings.Join(zeroes, ", ")
		}
		methods = append(methods, m)
	}

	return CrossPkgInterfaceResult{
		PackagePath:   importPath,
		PackageName:   loaded.Types.Name(),
		Methods:       methods,
		NeededImports: needed,
	}, true
}

// collectImports parses every non-test, non-_gen.go .go file in dir
// and returns a map alias -> importPath. Aliases are taken from the
// import spec if present (`foo "x/y/z"`), otherwise the package
// name's declared identifier — which for our purposes is the LAST
// path segment, since the type-checker isn't loaded yet and we
// don't have access to the imported package's declared name.
//
// This is the same fast AST walk ParseServiceDeps uses; we don't
// reuse that function because we need the import information, not
// just the field list.
func collectImports(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") || strings.HasSuffix(e.Name(), "_gen.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, imp := range file.Imports {
			pathStr, perr := importPathFromSpec(imp)
			if perr != nil {
				continue
			}
			alias := aliasForImport(imp, pathStr)
			if _, seen := out[alias]; seen {
				continue
			}
			out[alias] = pathStr
		}
	}
	return out, nil
}

// importPathFromSpec extracts the quoted path out of an ImportSpec.
// Returns an error when the spec's Path.Value isn't a valid Go
// string literal — defensively guarding against malformed input.
func importPathFromSpec(spec *ast.ImportSpec) (string, error) {
	if spec == nil || spec.Path == nil {
		return "", fmt.Errorf("nil import spec")
	}
	// spec.Path.Value is a quoted string literal. Trim the quotes.
	v := spec.Path.Value
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", fmt.Errorf("malformed import path: %q", v)
	}
	return v[1 : len(v)-1], nil
}

// aliasForImport returns the local alias the file uses for the
// imported package. When the import has an explicit name (e.g.
// `foo "x/y"`), that wins. Otherwise we fall back to the last path
// segment, since that's what Go uses when there's no alias AND it
// matches the imported package's declared name in the overwhelming
// majority of cases (the exceptions — packages whose declared name
// disagrees with their import path's leaf — are rare in well-curated
// projects and resolved upstream by users adding an explicit alias).
func aliasForImport(spec *ast.ImportSpec, importPath string) string {
	if spec.Name != nil && spec.Name.Name != "" && spec.Name.Name != "_" && spec.Name.Name != "." {
		return spec.Name.Name
	}
	if idx := strings.LastIndex(importPath, "/"); idx >= 0 {
		return importPath[idx+1:]
	}
	return importPath
}

// makeQualifier returns a types.Qualifier that:
//
//   - For the empty-path "current package" (impossible here since we're
//     rendering an interface from another package), uses no qualifier.
//   - For every other package, records the path in `needed` and returns
//     the package's declared name so the rendered text reads
//     `pkg.Type` rather than the fully-qualified `path/to/pkg.Type`.
//
// The map is mutated in place; the caller passes the same map that
// was pre-seeded with the interface's own package so all references
// accumulate into one set.
func makeQualifier(needed map[string]string) types.Qualifier {
	return func(p *types.Package) string {
		if p == nil {
			return ""
		}
		path := p.Path()
		name := p.Name()
		if existing, ok := needed[path]; ok {
			return existing
		}
		needed[path] = name
		return name
	}
}

// SortedNeededImports turns the unordered map produced by
// ResolveCrossPkgInterface into a deterministic ordered slice. Used by
// the bootstrap_testing.go data assembly so generated imports are
// stable across runs (deterministic codegen — required for the
// checksums-based "no spurious diff" guarantee).
func SortedNeededImports(needed map[string]string) []ExtraImport {
	out := make([]ExtraImport, 0, len(needed))
	for path, alias := range needed {
		out = append(out, ExtraImport{Path: path, Alias: alias})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ExtraImport is a single rendered import line for the
// bootstrap_testing.go template's extra-imports block. Carries an
// explicit alias even when it matches the path's leaf, so the
// template can emit `<alias> "<path>"` uniformly — Go tolerates the
// redundant alias and it makes the template logic one line shorter.
type ExtraImport struct {
	Path  string
	Alias string
}
