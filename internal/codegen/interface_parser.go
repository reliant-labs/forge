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
	// ExtraImports lists every package the flattened method signatures
	// reference by qualifier, resolved through the import block of the
	// file each method was declared in.
	//
	// A LOCAL interface can still have CROSS-PACKAGE signatures — the
	// interface type itself is in this package, but its parameter and
	// result types need not be. control-plane's `proxy_authz` handler
	// is the canonical case:
	//
	//	type AccessDecider interface {
	//	    Authorize(ctx context.Context, in proxyauthz.Input) proxyauthz.Decision
	//	}
	//
	// The synthesized stub renders that signature verbatim, so the
	// generated helper file must import internal/proxyauthz or it does
	// not compile. Only qualifiers that actually appear in a rendered
	// signature are listed: blanket-importing the declaring file's whole
	// import block would produce UNUSED imports, which fails the build
	// just as hard as a missing one.
	ExtraImports []ExtraImport
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
		// fileImports is the declaring file's alias -> path map, used to
		// resolve the package qualifiers appearing in this interface's
		// method signatures. Import scope is per-FILE in Go, so this must
		// be tracked per declaration, not per directory: two files in the
		// same package can bind the same alias to different paths.
		fileImports map[string]string
	}
	var entries2 []entry
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
		fileImports := fileImportMap(file)
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
				entries2 = append(entries2, entry{
					name: ts.Name.Name, ifaceAt: it, fileImports: fileImports,
				})
			}
		}
	}

	// Build raw method maps + embed maps, then flatten.
	directMethods := map[string][]InterfaceMethod{}
	embeds := map[string][]string{}
	// declFileImports records which file each interface was declared in,
	// so a flattened embed's signatures resolve through the EMBEDDED
	// interface's imports rather than the embedder's.
	declFileImports := map[string]map[string]string{}
	for _, e := range entries2 {
		declFileImports[e.name] = e.fileImports
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

	// needed accumulates import path -> alias for the signatures walked
	// so far. It is filled per declaring interface, so an embedded
	// interface contributes its OWN file's import bindings.
	var resolve func(name string, visited map[string]bool, needed map[string]string) []InterfaceMethod
	resolve = func(name string, visited map[string]bool, needed map[string]string) []InterfaceMethod {
		if visited[name] {
			return nil
		}
		visited[name] = true
		methods := append([]InterfaceMethod{}, directMethods[name]...)
		collectSignatureImports(directMethods[name], declFileImports[name], needed)
		for _, em := range embeds[name] {
			methods = append(methods, resolve(em, visited, needed)...)
		}
		return methods
	}

	for _, e := range entries2 {
		needed := map[string]string{}
		methods := resolve(e.name, map[string]bool{}, needed)
		out[e.name] = LocalInterface{
			Name:         e.name,
			Methods:      methods,
			ExtraImports: SortedNeededImports(needed),
		}
	}
	return out, nil
}

// fileImportMap renders one file's import block as alias -> path, where
// the alias is the name the file's own code must use: the explicit alias
// when present, otherwise the path's last segment.
//
// The leaf-segment fallback is a heuristic — a package's declared name may
// differ from its directory (`package v1` under `.../userv1`). That is fine
// here because we only ever look up qualifiers we OBSERVED in a signature
// that this same file compiles, and we re-emit the import under exactly the
// alias we matched. Where the two disagree, the file must already carry an
// explicit alias, which we read directly.
//
// Dot- and blank-imports are skipped: a dot-import contributes no qualifier
// to match on, and a blank import can never be referenced.
func fileImportMap(file *ast.File) map[string]string {
	out := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path == "" {
			continue
		}
		alias := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			alias = path[i+1:]
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			alias = spec.Name.Name
		}
		out[alias] = path
	}
	return out
}

// collectSignatureImports records, into needed (path -> alias), every
// import the RENDERED method signatures actually reference.
//
// It reads the rendered Params/Results strings rather than re-walking the
// AST deliberately: those strings are verbatim what the stub template
// emits, so matching on them cannot drift from what the generated file
// says. Emitting an import the signature does not reference would be an
// unused import — as fatal to the build as the missing one this fixes —
// so a qualifier that resolves to nothing in fileImports is skipped.
func collectSignatureImports(methods []InterfaceMethod, fileImports map[string]string, needed map[string]string) {
	if len(fileImports) == 0 {
		return
	}
	for _, m := range methods {
		for _, sig := range []string{m.Params, m.Results} {
			for alias := range qualifiersIn(sig) {
				if path, ok := fileImports[alias]; ok {
					needed[path] = alias
				}
			}
		}
	}
}

// qualifiersIn returns the set of package qualifiers appearing as the
// `pkg` in a `pkg.Type` selector inside a rendered type expression.
//
// The scan is lexical: walk identifier runs, and take a run as a
// qualifier when the character immediately after it is '.'. Decoration
// (`*`, `[]`, `map[`, `chan `, parens, commas) is naturally skipped
// because none of it is an identifier character. Parameter NAMES cannot
// be mistaken for qualifiers — a name is always followed by a space, not
// a dot.
func qualifiersIn(sig string) map[string]struct{} {
	out := map[string]struct{}{}
	i := 0
	for i < len(sig) {
		if !isIdentStart(sig[i]) {
			i++
			continue
		}
		j := i
		for j < len(sig) && isIdentPart(sig[j]) {
			j++
		}
		if j < len(sig) && sig[j] == '.' {
			out[sig[i:j]] = struct{}{}
		}
		// Skip the whole run (plus a trailing '.' and the selected name)
		// so `pkg.Type` never re-enters with `Type` as a candidate.
		i = j + 1
	}
	return out
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
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
