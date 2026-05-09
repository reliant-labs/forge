package codegen

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// LocalInterface describes one interface type declared in a handler
// (or package) directory. Used by the testing.go auto-stub generator
// to synthesize zero-value implementations for service-owned Deps
// fields whose type is locally declared.
//
// We only consider interfaces declared in the same package as the
// service: cross-package interfaces would force the testing.go
// generator to chase imports across the project, and the interfaces
// that fail "Repo is required" today (Repository, CommandPublisher,
// AuditStore, etc.) are uniformly local to the handler that uses
// them. If a future need arises for cross-package stubs the parser
// can grow without changing the consumer's call sites.
type LocalInterface struct {
	// Name is the interface type name as declared (e.g. "Repository").
	Name string
	// Methods enumerates the interface's method set, with embedded
	// interfaces flattened so callers get a single list to walk.
	Methods []InterfaceMethod
}

// InterfaceMethod is one method on a LocalInterface, in a shape
// directly consumable by the testing.go template's stub emitter.
type InterfaceMethod struct {
	// Name is the method name as declared (e.g. "GetByID").
	Name string
	// Params is the rendered parameter list including names + types,
	// e.g. "ctx context.Context, id string". Empty when the method
	// takes no parameters.
	Params string
	// Results is the rendered result list with parens when there are
	// multiple, e.g. "(*db.User, error)". Empty when the method
	// returns nothing.
	Results string
	// ReturnStatement is the body of a stub implementation: either
	// "return <zeroes>" or empty when the method returns nothing.
	ReturnStatement string
}

// ParseLocalInterfaces returns every interface type declared in non-
// test .go files under dir. The set is keyed by interface name so
// callers can index by the Deps field's pretty-printed type.
//
// Returns an empty map (never nil) and no error if dir doesn't exist
// — callers treat the absent case the same as "no local interfaces
// to stub" and fall back to nil for that field.
func ParseLocalInterfaces(dir string) (map[string]LocalInterface, error) {
	out := make(map[string]LocalInterface)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}

	fset := token.NewFileSet()
	// Two-pass: first collect interface AST entries (so embeds can
	// resolve in either declaration order), then flatten methods.
	type entry struct {
		name    string
		ifaceAt *ast.InterfaceType
	}
	var entries2 []entry
	imports := map[string]map[string]string{} // file path -> alias -> path
	_ = imports                                // reserved for future cross-import resolution
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// Skip generated files — they re-declare nothing useful and
		// can transiently fail to parse. Same convention as ParseContract.
		if strings.HasSuffix(e.Name(), "_gen.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				entries2 = append(entries2, entry{name: ts.Name.Name, ifaceAt: it})
			}
		}
	}

	// Build raw method maps + embed maps, then flatten.
	directMethods := map[string][]InterfaceMethod{}
	embeds := map[string][]string{}
	for _, e := range entries2 {
		if e.ifaceAt.Methods == nil {
			continue
		}
		for _, field := range e.ifaceAt.Methods.List {
			switch ft := field.Type.(type) {
			case *ast.FuncType:
				for _, n := range field.Names {
					m := buildInterfaceMethod(fset, n.Name, ft)
					directMethods[e.name] = append(directMethods[e.name], m)
				}
			case *ast.Ident:
				// Embedded same-package interface.
				embeds[e.name] = append(embeds[e.name], ft.Name)
			case *ast.SelectorExpr:
				// Cross-package embed — skip; callers can hand-write
				// a stub override if they need it.
			}
		}
	}

	var resolve func(name string, visited map[string]bool) []InterfaceMethod
	resolve = func(name string, visited map[string]bool) []InterfaceMethod {
		if visited[name] {
			return nil
		}
		visited[name] = true
		methods := append([]InterfaceMethod{}, directMethods[name]...)
		for _, em := range embeds[name] {
			methods = append(methods, resolve(em, visited)...)
		}
		return methods
	}

	for _, e := range entries2 {
		out[e.name] = LocalInterface{
			Name:    e.name,
			Methods: resolve(e.name, map[string]bool{}),
		}
	}
	return out, nil
}

// buildInterfaceMethod renders a single interface method into its
// stub-template-ready form: pretty-printed params, results, and a
// "return <zeroes>" body the template can drop into the generated
// stub method.
func buildInterfaceMethod(fset *token.FileSet, name string, ft *ast.FuncType) InterfaceMethod {
	m := InterfaceMethod{Name: name}

	// Params: render each field's name(s) + type. We keep the names
	// where present (so the stub signature reads naturally) and
	// synthesize "_" placeholders only for unnamed params, since Go
	// requires either all-named or all-unnamed in a single field
	// list — but cross-field-mixing is fine, so we choose per-field.
	if ft.Params != nil && len(ft.Params.List) > 0 {
		var parts []string
		for _, field := range ft.Params.List {
			tStr := printType(fset, field.Type)
			if len(field.Names) == 0 {
				parts = append(parts, tStr)
				continue
			}
			var names []string
			for _, n := range field.Names {
				names = append(names, n.Name)
			}
			parts = append(parts, strings.Join(names, ", ")+" "+tStr)
		}
		m.Params = strings.Join(parts, ", ")
	}

	// Results: collect into a flat list of type expressions. The
	// template wraps them in parens when len > 1.
	var resultTypes []string
	if ft.Results != nil {
		for _, field := range ft.Results.List {
			tStr := printType(fset, field.Type)
			n := len(field.Names)
			if n == 0 {
				n = 1
			}
			for i := 0; i < n; i++ {
				resultTypes = append(resultTypes, tStr)
			}
		}
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

	return m
}

// zeroValueForType returns the Go literal for the zero value of the
// given pretty-printed type expression. Mirrors the contract package's
// zeroValue but lives here so the codegen package doesn't take an
// import-cycle on internal/generator/contract.
//
// The auto-stub use case is forgiving: stubs satisfy validateDeps,
// they don't satisfy realistic test assertions. A "T{}" fallback for
// a same-package interface would still typecheck because the stub
// itself is what implements the interface — we only use these zero
// values for return statements, not for the receiver type.
func zeroValueForType(t string) string {
	t = strings.TrimSpace(t)
	switch t {
	case "bool":
		return "false"
	case "string":
		return `""`
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "complex64", "complex128",
		"byte", "rune", "uintptr":
		return "0"
	case "error":
		return "nil"
	case "any", "interface{}":
		return "nil"
	}
	if strings.HasPrefix(t, "*") ||
		strings.HasPrefix(t, "[]") ||
		strings.HasPrefix(t, "map[") ||
		strings.HasPrefix(t, "chan ") ||
		strings.HasPrefix(t, "<-chan ") ||
		strings.HasPrefix(t, "chan<- ") ||
		strings.HasPrefix(t, "func(") ||
		strings.HasPrefix(t, "interface{") ||
		strings.HasPrefix(t, "interface ") {
		return "nil"
	}
	// Named type — most safely emitted as a composite literal.
	// Worst case (an imported interface): the resulting line won't
	// compile, the user gets a clear "T{} not allowed for interface"
	// error, and they can hand-roll a stub override. The marker
	// `// forge:optional-dep` exists for fields the user explicitly
	// doesn't want auto-stubbed.
	return t + "{}"
}

// IsLocallyDeclaredInterface reports whether typeExpr (as printed by
// printType — a bare identifier or selector) names an interface
// declared in locals. The Deps field type is matched ignoring
// surrounding pointer/array decoration: only fields with type "T"
// where "T" is a local interface name are auto-stubbable. Pointer-to-
// interface (`*Repository`) is not idiomatic and is left to the
// hand-roll path.
func IsLocallyDeclaredInterface(typeExpr string, locals map[string]LocalInterface) bool {
	if locals == nil {
		return false
	}
	_, ok := locals[strings.TrimSpace(typeExpr)]
	return ok
}

// printerInterface is a tiny indirection so the package-level
// printType lives in deps_parser.go without needing re-export.
var _ = printer.CommentedNode{}
