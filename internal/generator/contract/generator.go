// Package contract parses Go interface definitions from contract.go files
// and generates mock_gen.go (function-field mock pattern) in the same
// directory.
//
// Earlier versions of this package also emitted middleware_gen.go,
// tracing_gen.go and metrics_gen.go — per-method wrappers around every
// contract.go interface. Those were removed in favour of Connect
// interceptors at the handler boundary (forge/pkg/observe) plus opt-in
// helpers (observe.LogCall, observe.TraceCall, observe.NewCallMetrics)
// for users who want internal-package observability. The mock stays
// codegen because the per-method MockX struct is a real grep target —
// "show me MockUserService's methods" is a tight feedback loop that
// generic reflection can't replace.
package contract

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// InterfaceDef represents a parsed Go interface.
type InterfaceDef struct {
	Name    string
	Methods []MethodDef
}

// MethodDef represents a single method on an interface.
type MethodDef struct {
	Name    string
	Params  []ParamDef
	Results []ParamDef
}

// ParamDef represents a method parameter or return value.
type ParamDef struct {
	Name     string
	TypeExpr string // rendered Go type expression, e.g. "context.Context", "*sql.Rows", "...any"
	Variadic bool
}

// ContractFile holds everything extracted from a single contract.go.
type ContractFile struct {
	Package    string
	Imports    map[string]string // alias/name → import path (e.g. "sql" → "database/sql")
	Interfaces []InterfaceDef
	// InterfaceNames is the set of interface type names defined in this file.
	// Used by the zero-value generator to emit "nil" for interface-typed
	// returns instead of the invalid composite literal "T{}".
	InterfaceNames map[string]bool
	// PrimitiveAliases maps a named type (e.g. "BalanceCapReason") to its
	// underlying primitive kind (e.g. "string"). Populated by scanning
	// contract.go and its sibling .go files for `type X <primitive>`
	// declarations. Used by the zero-value generator to emit the
	// underlying primitive's zero (`""`, `0`, `false`) instead of the
	// invalid composite literal `BalanceCapReason{}`.
	PrimitiveAliases map[string]string
}

// Options controls optional aspects of mock generation. The zero value is
// equivalent to plain Generate(contractPath) — every field is opt-in and
// backwards compatible.
type Options struct {
	// ExtraInterfaceTypes is a project-supplied allow-list of cross-package
	// interface types the mock generator should treat as mockable. The
	// rendered type expression of a method's return (e.g.
	// "billing.MeterClient") is matched against this set during zero-value
	// emission; matches produce "nil" instead of the invalid composite
	// literal "T{}".
	//
	// Sourced from forge.yaml's `contracts.interface_types` at the CLI
	// layer; nil / empty is the no-op default.
	ExtraInterfaceTypes map[string]bool

	// ProjectRoot + Checksums route the mock write through the
	// checksums.WriteGeneratedFile chokepoint so the emitted
	// internal/<pkg>/mock_gen.go is recorded in the manifest AND in
	// the per-run WrittenThisRun set. Without this, the stale-artifact
	// sweep saw every manifest-tracked mock_gen.go as "tracked but not
	// re-emitted this run" and flagged it for deletion on every
	// `forge generate` — and `--force-cleanup` would actually delete
	// live mocks (kalshi-trader FORGE_BACKLOG #15).
	//
	// ProjectRoot is the project root (the directory containing .forge/);
	// relative manifest paths are computed against it. Both fields nil
	// /empty preserve the legacy raw-os.WriteFile behavior for callers
	// without a manifest in scope (e.g. the `forge add package` stub
	// emit — its mock is adopted into the manifest on the next full
	// generate).
	ProjectRoot string
	Checksums   *checksums.FileChecksums
}

// Generate parses contractPath and writes mock_gen.go next to it.
//
// In addition to (re)writing mock_gen.go, Generate sweeps any stale
// observability wrappers (middleware_gen.go, tracing_gen.go,
// metrics_gen.go) from the same directory — these were emitted by
// previous forge versions and are now superseded by the
// forge/pkg/observe Connect interceptors. Removing them here (rather
// than relying on the audit "orphan" report) keeps `forge generate`
// idempotent and gives the user a clear signal in the build output:
// either the file is present and current, or it's gone.
func Generate(contractPath string) error {
	return GenerateWithOptions(contractPath, Options{})
}

// GenerateWithOptions is Generate with project-level extension hooks
// (currently: ExtraInterfaceTypes). New plumbing should go through this
// entry point; the bare Generate is preserved for call sites that don't
// need the extension surface.
func GenerateWithOptions(contractPath string, opts Options) error {
	cf, err := ParseContract(contractPath)
	if err != nil {
		return fmt.Errorf("parse contract: %w", err)
	}

	dir := filepath.Dir(contractPath)

	if err := writeMock(cf, dir, opts); err != nil {
		return fmt.Errorf("generate mock: %w", err)
	}

	if err := removeLegacyWrappers(dir); err != nil {
		return fmt.Errorf("remove legacy wrappers: %w", err)
	}

	return nil
}

// removeLegacyWrappers deletes middleware_gen.go, tracing_gen.go and
// metrics_gen.go from dir if present. These were emitted by earlier
// forge versions; they're now replaced by observe.* libraries. Missing
// files are not an error — the function is safe to call on freshly
// scaffolded packages.
func removeLegacyWrappers(dir string) error {
	for _, name := range []string{"middleware_gen.go", "tracing_gen.go", "metrics_gen.go"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// ParseContract parses a contract.go file and extracts all interface definitions.
//
// Sibling .go files in the same package directory are also scanned (parse-only,
// no method extraction) to populate InterfaceNames so the mock generator can
// emit "nil" for interface-typed returns whose declaration lives outside
// contract.go (e.g. internal/debug defines Service in contract.go and
// Debugger in debugger.go).
func ParseContract(path string) (*ContractFile, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse file: %w", err)
	}

	cf := &ContractFile{
		Package:          file.Name.Name,
		Imports:          make(map[string]string),
		InterfaceNames:   make(map[string]bool),
		PrimitiveAliases: make(map[string]string),
	}

	// Scan sibling .go files in the same package directory and record any
	// interface type names. This lets zeroValue recognize interfaces whose
	// declarations are split across multiple files in the package (common
	// pattern: contract.go has Service, debugger.go has Debugger).
	dir := filepath.Dir(path)
	if entries, dirErr := os.ReadDir(dir); dirErr == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			// Skip generated files — they re-declare nothing useful and
			// can transiently fail to parse (e.g. an in-progress edit).
			if strings.HasSuffix(entry.Name(), "_gen.go") {
				continue
			}
			siblingPath := filepath.Join(dir, entry.Name())
			if siblingPath == path {
				continue
			}
			siblingFile, sErr := parser.ParseFile(fset, siblingPath, nil, parser.SkipObjectResolution)
			if sErr != nil {
				continue // best-effort: skip unparseable siblings
			}
			collectInterfaceNames(siblingFile, cf.InterfaceNames)
			collectPrimitiveAliases(siblingFile, cf.PrimitiveAliases)
		}
	}

	// Also collect primitive aliases declared in contract.go itself, so the
	// zero-value generator handles `type X string` defined alongside the
	// Service interface in the same file.
	collectPrimitiveAliases(file, cf.PrimitiveAliases)

	// Collect imports: build a map from local name → import path.
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		var name string
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			// Default name is the last path element.
			parts := strings.Split(path, "/")
			name = parts[len(parts)-1]
		}
		cf.Imports[name] = path
	}

	// First pass: collect all interface AST nodes by name.
	type ifaceEntry struct {
		name      string
		ifaceType *ast.InterfaceType
	}
	var ifaceEntries []ifaceEntry
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			ifaceType, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			ifaceEntries = append(ifaceEntries, ifaceEntry{
				name:      typeSpec.Name.Name,
				ifaceType: ifaceType,
			})
			cf.InterfaceNames[typeSpec.Name.Name] = true
		}
	}

	// Build a map of interface name → direct methods (no embedding resolved yet).
	ifaceMethodMap := make(map[string][]MethodDef)
	ifaceEmbeds := make(map[string][]string) // interface name → embedded interface names
	for _, entry := range ifaceEntries {
		if entry.ifaceType.Methods != nil {
			for _, field := range entry.ifaceType.Methods.List {
				switch ft := field.Type.(type) {
				case *ast.FuncType:
					for _, name := range field.Names {
						md := extractMethod(name.Name, ft, fset)
						ifaceMethodMap[entry.name] = append(ifaceMethodMap[entry.name], md)
					}
				case *ast.Ident:
					// Embedded interface from same package, e.g. "CommandPublisher"
					ifaceEmbeds[entry.name] = append(ifaceEmbeds[entry.name], ft.Name)
				case *ast.SelectorExpr:
					// Embedded interface from another package — skip for now
				}
			}
		}
	}

	// Resolve embedded interfaces: collect all methods including those from embeds.
	var resolveMethods func(name string, visited map[string]bool) []MethodDef
	resolveMethods = func(name string, visited map[string]bool) []MethodDef {
		if visited[name] {
			return nil
		}
		visited[name] = true
		methods := append([]MethodDef{}, ifaceMethodMap[name]...)
		for _, embedded := range ifaceEmbeds[name] {
			methods = append(methods, resolveMethods(embedded, visited)...)
		}
		return methods
	}

	// Second pass: build InterfaceDefs with resolved methods.
	for _, entry := range ifaceEntries {
		iface := InterfaceDef{
			Name:    entry.name,
			Methods: resolveMethods(entry.name, make(map[string]bool)),
		}
		cf.Interfaces = append(cf.Interfaces, iface)
	}

	return cf, nil
}

// collectInterfaceNames records the name of every interface type declared in
// file into the names set. Used to build a package-wide set of interface
// names so the zero-value generator can emit "nil" for interface-typed
// returns rather than the invalid composite literal "T{}".
func collectInterfaceNames(file *ast.File, names map[string]bool) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if _, ok := typeSpec.Type.(*ast.InterfaceType); ok {
				names[typeSpec.Name.Name] = true
			}
		}
	}
}

// primitiveKinds enumerates the underlying Go primitive identifiers the
// zero-value generator can resolve for a named alias. Limited to types
// whose zero value is unambiguous and renders to a Go literal — pointers,
// slices, maps, channels, funcs, etc. already have their own branches in
// zeroValue and are not alias targets here.
var primitiveKinds = map[string]bool{
	"bool":    true,
	"string":  true,
	"int":     true,
	"int8":    true,
	"int16":   true,
	"int32":   true,
	"int64":   true,
	"uint":    true,
	"uint8":   true,
	"uint16":  true,
	"uint32":  true,
	"uint64":  true,
	"uintptr": true,
	"float32": true,
	"float64": true,
	"byte":    true,
	"rune":    true,
}

// collectPrimitiveAliases records any `type X <primitive>` declarations
// from file into the aliases map (name → underlying primitive kind). Used
// so the zero-value generator emits the underlying primitive's zero
// (e.g. `""` for `type BalanceCapReason string`) rather than the invalid
// composite literal `BalanceCapReason{}`.
func collectPrimitiveAliases(file *ast.File, aliases map[string]string) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			ident, ok := typeSpec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			if primitiveKinds[ident.Name] {
				aliases[typeSpec.Name.Name] = ident.Name
			}
		}
	}
}

// primitiveZero returns the zero-value literal for a primitive kind name.
// Returns ("", false) if kind is not a recognized primitive.
func primitiveZero(kind string) (string, bool) {
	switch kind {
	case "bool":
		return "false", true
	case "string":
		return `""`, true
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"byte", "rune":
		return "0", true
	case "float32", "float64":
		return "0", true
	}
	return "", false
}

// extractMethod builds a MethodDef from a *ast.FuncType.
func extractMethod(name string, ft *ast.FuncType, fset *token.FileSet) MethodDef {
	md := MethodDef{Name: name}

	if ft.Params != nil {
		for _, field := range ft.Params.List {
			typeStr := renderExpr(field.Type, fset)
			variadic := false
			if _, ok := field.Type.(*ast.Ellipsis); ok {
				variadic = true
			}
			if len(field.Names) == 0 {
				md.Params = append(md.Params, ParamDef{TypeExpr: typeStr, Variadic: variadic})
			} else {
				for _, n := range field.Names {
					md.Params = append(md.Params, ParamDef{Name: n.Name, TypeExpr: typeStr, Variadic: variadic})
				}
			}
		}
	}

	if ft.Results != nil {
		for _, field := range ft.Results.List {
			typeStr := renderExpr(field.Type, fset)
			if len(field.Names) == 0 {
				md.Results = append(md.Results, ParamDef{TypeExpr: typeStr})
			} else {
				for _, n := range field.Names {
					md.Results = append(md.Results, ParamDef{Name: n.Name, TypeExpr: typeStr})
				}
			}
		}
	}

	return md
}

// renderExpr converts an ast.Expr back into its Go source representation.
func renderExpr(expr ast.Expr, fset *token.FileSet) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, expr); err != nil {
		return fmt.Sprintf("/* renderExpr error: %v */", err)
	}
	return buf.String()
}

// collectImports determines which imports from the source file are needed
// by the generated code for the given interfaces.
func collectImports(cf *ContractFile, ifaces []InterfaceDef) []string {
	needed := make(map[string]bool)
	for _, iface := range ifaces {
		for _, m := range iface.Methods {
			for _, p := range m.Params {
				collectFromTypeExpr(p.TypeExpr, cf.Imports, needed)
			}
			for _, r := range m.Results {
				collectFromTypeExpr(r.TypeExpr, cf.Imports, needed)
			}
		}
	}

	var imports []string
	for imp := range needed {
		imports = append(imports, imp)
	}
	sort.Strings(imports)
	return imports
}

// collectFromTypeExpr scans a type expression string for package references
// and adds the corresponding import paths to the needed set.
func collectFromTypeExpr(typeExpr string, importMap map[string]string, needed map[string]bool) {
	for alias, path := range importMap {
		// Look for "alias." in the type expression. This handles cases like
		// "context.Context", "*sql.Rows", "sql.Result", "func([]byte) ([]byte, error)".
		if strings.Contains(typeExpr, alias+".") {
			needed[path] = true
		}
	}
}

// writeMock generates mock_gen.go in dir.
//
// opts.ExtraInterfaceTypes is unioned into the set of interface names the
// template consults during zero-value emission. The union lets projects
// declare additional cross-package interface types in forge.yaml without
// requiring a forge fork; an entry like "billing.MeterClient" flips the
// zero value for that type from the (invalid) "billing.MeterClient{}" to
// the canonical "nil".
func writeMock(cf *ContractFile, dir string, opts Options) error {
	imports := collectImports(cf, cf.Interfaces)
	// The mock embeds contractkit.Recorder and uses contractkit.MockNotSet
	// for error-returning methods. The Recorder embed alone requires the
	// import even for interfaces that have zero methods, so include it
	// whenever the file declares at least one interface.
	if len(cf.Interfaces) > 0 {
		addImport(&imports, contractkitImport)
	}

	// Union the project-supplied extras into a fresh copy of the contract's
	// own interface name set. The template uses this combined set to decide
	// which return types collapse to "nil" instead of "T{}".
	interfaceNames := make(map[string]bool, len(cf.InterfaceNames)+len(opts.ExtraInterfaceTypes))
	for k, v := range cf.InterfaceNames {
		interfaceNames[k] = v
	}
	for k, v := range opts.ExtraInterfaceTypes {
		if v {
			interfaceNames[k] = true
		}
	}

	data := templateData{
		Package:          cf.Package,
		Imports:          imports,
		Interfaces:       cf.Interfaces,
		InterfaceNames:   interfaceNames,
		PrimitiveAliases: cf.PrimitiveAliases,
	}

	var buf bytes.Buffer
	if err := mockTmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("execute mock template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("gofmt mock output: %w\n---\n%s", err, buf.String())
	}

	mockPath := filepath.Join(dir, "mock_gen.go")

	// Manifest-aware path: when the caller supplied a project root, the
	// write MUST go through checksums.WriteGeneratedFile so the path
	// lands in WrittenThisRun (stale-sweep immunity) and honors
	// disowned entries (user-owned files are never stomped).
	// force=true matches the file's Tier-1 contract: mock_gen.go is
	// regenerated unconditionally from contract.go every run.
	if opts.ProjectRoot != "" {
		rel, relErr := filepath.Rel(opts.ProjectRoot, mockPath)
		if relErr != nil {
			return fmt.Errorf("compute project-relative mock path: %w", relErr)
		}
		if _, werr := checksums.WriteGeneratedFile(opts.ProjectRoot, rel, formatted, opts.Checksums, true); werr != nil {
			return fmt.Errorf("write %s: %w", rel, werr)
		}
		return nil
	}

	// Legacy path: no manifest in scope (e.g. `forge add package` stub
	// emit). Raw write; the file is adopted into the manifest by the
	// next full `forge generate`.
	return os.WriteFile(mockPath, formatted, 0644)
}

// addImport adds an import path if not already present.
func addImport(imports *[]string, path string) {
	for _, p := range *imports {
		if p == path {
			return
		}
	}
	*imports = append(*imports, path)
	sort.Strings(*imports)
}

// ParamSignature returns the Go parameter list for a method, e.g. "ctx context.Context, id string".
func (m MethodDef) ParamSignature() string {
	var parts []string
	for _, p := range m.Params {
		if p.Name != "" {
			parts = append(parts, p.Name+" "+p.TypeExpr)
		} else {
			parts = append(parts, p.TypeExpr)
		}
	}
	return strings.Join(parts, ", ")
}

// ResultSignature returns the Go result type list, e.g. "(string, error)" or "error".
func (m MethodDef) ResultSignature() string {
	if len(m.Results) == 0 {
		return ""
	}
	var parts []string
	hasNames := false
	for _, r := range m.Results {
		if r.Name != "" {
			hasNames = true
			parts = append(parts, r.Name+" "+r.TypeExpr)
		} else {
			parts = append(parts, r.TypeExpr)
		}
	}
	sig := strings.Join(parts, ", ")
	if len(m.Results) > 1 || hasNames {
		return "(" + sig + ")"
	}
	return sig
}

// RecordArgs returns the argument list to pass to the embedded
// contractkit.Recorder.Record. Variadic params are passed verbatim
// (no "..." suffix) so the recorder receives the slice as a single
// any value — this is what callers want when asserting on captured
// arguments. If the method has no parameters, returns the empty
// string and the template emits Record("Method") with no extra args.
func (m MethodDef) RecordArgs() string {
	var parts []string
	for _, p := range m.Params {
		name := p.Name
		if name == "" {
			name = "_"
		}
		// For variadic params, pass the slice as a single value rather
		// than spreading it — Recorder.Record uses ...any internally,
		// so spreading would scatter the elements across multiple
		// captured args, which is rarely what the test assertion wants.
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

// CallArgs returns the argument list for delegating to the inner implementation,
// e.g. "ctx, id" or "ctx, query, args...".
func (m MethodDef) CallArgs() string {
	var parts []string
	for _, p := range m.Params {
		name := p.Name
		if name == "" {
			// Unnamed params — should not happen in well-formed contracts,
			// but generate a placeholder.
			name = "_"
		}
		if p.Variadic {
			parts = append(parts, name+"...")
		} else {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, ", ")
}

// FuncFieldType returns the func type for mock function fields,
// e.g. "func(context.Context, string) (string, error)".
func (m MethodDef) FuncFieldType() string {
	var params []string
	for _, p := range m.Params {
		params = append(params, p.TypeExpr)
	}
	paramStr := strings.Join(params, ", ")

	result := m.ResultSignature()
	if result == "" {
		return "func(" + paramStr + ")"
	}
	return "func(" + paramStr + ") " + result
}

// ZeroResults returns the zero-value expression for the result types,
// for use in mock fallback returns. E.g. "nil, contractkit.MockNotSet(...)".
//
// interfaceNames is the set of locally-defined interface names from the
// parsed contract; passed through to zeroValue so interface-typed returns
// emit "nil" rather than the invalid composite literal "T{}".
//
// primitiveAliases maps locally-declared named primitive types (e.g.
// `type BalanceCapReason string`) to their underlying kind; passed
// through so the mock emits the primitive's zero value ("") rather than
// the invalid composite literal `BalanceCapReason{}`.
//
// The trailing error result is rendered as contractkit.MockNotSet so the
// canonical "Mock<Iface>.<Method>Func not set" error string lives in the
// library; bumping the format is now a one-place change.
func (m MethodDef) ZeroResults(mockName string, interfaceNames map[string]bool, primitiveAliases map[string]string) string {
	if len(m.Results) == 0 {
		return ""
	}
	var parts []string
	for i, r := range m.Results {
		// Last result that is "error" delegates to contractkit.MockNotSet.
		if i == len(m.Results)-1 && r.TypeExpr == "error" {
			parts = append(parts, fmt.Sprintf(`contractkit.MockNotSet(%q, %q)`, mockName, m.Name))
		} else {
			parts = append(parts, zeroValue(r.TypeExpr, interfaceNames, primitiveAliases))
		}
	}
	return strings.Join(parts, ", ")
}

// ResultNames returns placeholder variable names for capturing results,
// e.g. "r0, r1" for two return values.
func (m MethodDef) ResultNames() string {
	var parts []string
	for i := range m.Results {
		parts = append(parts, fmt.Sprintf("r%d", i))
	}
	return strings.Join(parts, ", ")
}

// ResultNamesReturn returns "r0, r1" or "return r0, r1".
func (m MethodDef) HasResults() bool {
	return len(m.Results) > 0
}

// LastResultIsError returns true if the last result type is "error".
func (m MethodDef) LastResultIsError() bool {
	if len(m.Results) == 0 {
		return false
	}
	return m.Results[len(m.Results)-1].TypeExpr == "error"
}

// HasContext returns true if the method has a context.Context parameter.
func (m MethodDef) HasContext() bool {
	for _, p := range m.Params {
		if p.TypeExpr == "context.Context" {
			return true
		}
	}
	return false
}

// ContextParamName returns the name of the context.Context parameter, or empty string.
func (m MethodDef) ContextParamName() string {
	for _, p := range m.Params {
		if p.TypeExpr == "context.Context" {
			if p.Name != "" {
				return p.Name
			}
			return "ctx"
		}
	}
	return ""
}

// ErrorResultName returns the placeholder name for the error result (last result).
func (m MethodDef) ErrorResultName() string {
	if !m.LastResultIsError() {
		return ""
	}
	return fmt.Sprintf("r%d", len(m.Results)-1)
}

// localNamedTypeRe matches an unqualified exported identifier — e.g. "CheckResult".
// These are almost always struct types defined in the same package as the
// contract, so the safe zero value is a composite literal "CheckResult{}".
var localNamedTypeRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)

// qualifiedNamedTypeRe matches "pkg.Type" — a type from another package.
// Heuristically we emit "pkg.Type{}". This is wrong for interface types
// (e.g. sql.Result), but the previous "nil" default was wrong for struct
// types. Structs are far more common as direct return values; interfaces
// are usually returned via pointer or wrapped in an error return.
var qualifiedNamedTypeRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*\.[A-Z][A-Za-z0-9_]*$`)

// crossPackageInterfaces is a small allow-list of well-known cross-package
// interface types. The zero-value generator emits "nil" for these instead
// of the invalid composite literal "pkg.T{}".
//
// Local interfaces defined in the same contract.go are detected
// automatically via ContractFile.InterfaceNames — this list only needs
// to enumerate types that come from other packages and that contract
// methods commonly return. Extend as needed; an unrecognized cross-
// package interface still produces a build error in the generated mock.
var crossPackageInterfaces = map[string]bool{
	"context.Context":     true,
	"io.Reader":           true,
	"io.Writer":           true,
	"io.Closer":           true,
	"io.ReadWriter":       true,
	"io.ReadCloser":       true,
	"io.WriteCloser":      true,
	"io.ReadWriteCloser":  true,
	"io.Seeker":           true,
	"io.ReaderAt":         true,
	"io.WriterAt":         true,
	"net.Conn":            true,
	"net.Listener":        true,
	"net.Addr":            true,
	"http.Handler":        true,
	"http.ResponseWriter": true,
	"http.RoundTripper":   true,
	"http.CookieJar":      true,
	"sql.Result":          true,
	"driver.Conn":         true,
	"driver.Driver":       true,
	"driver.Result":       true,
	"driver.Stmt":         true,
	"driver.Tx":           true,
	"fmt.Stringer":        true,
	"error":               true,
	// Connect-RPC framework interfaces. Contract methods that return
	// interceptors / interceptor funcs / streaming-handler interceptors
	// would otherwise emit `connect.Interceptor{}` which doesn't
	// compile. The v1 cp-forge migration repro'd this on every audit /
	// auth interceptor port.
	"connect.Interceptor":                 true,
	"connect.UnaryInterceptorFunc":        true,
	"connect.StreamingHandlerInterceptor": true,
	"connect.StreamingClientInterceptor":  true,
	"connect.AnyRequest":                  true,
	"connect.AnyResponse":                 true,
}

// isInterfaceType reports whether typeExpr names an interface — either a
// local interface defined in the contract file (interfaceNames) or one of
// the well-known cross-package interfaces listed in crossPackageInterfaces.
func isInterfaceType(typeExpr string, interfaceNames map[string]bool) bool {
	if interfaceNames[typeExpr] {
		return true
	}
	if crossPackageInterfaces[typeExpr] {
		return true
	}
	return false
}

// zeroValue returns the zero value literal for a Go type expression.
//
// interfaceNames is the set of locally-defined interface type names from
// the parsed ContractFile; types in that set (or in the cross-package
// allow-list) emit "nil" instead of "T{}" because composite literals are
// not valid for interface types.
//
// primitiveAliases maps locally-declared named primitive types (e.g.
// `type BalanceCapReason string`) to their underlying kind. Types in
// this map emit the underlying primitive's zero value (`""`, `0`,
// `false`) instead of the invalid composite literal `T{}`.
func zeroValue(typeExpr string, interfaceNames map[string]bool, primitiveAliases map[string]string) string {
	switch {
	case typeExpr == "bool":
		return "false"
	case typeExpr == "string":
		return `""`
	case typeExpr == "int", typeExpr == "int8", typeExpr == "int16",
		typeExpr == "int32", typeExpr == "int64",
		typeExpr == "uint", typeExpr == "uint8", typeExpr == "uint16",
		typeExpr == "uint32", typeExpr == "uint64",
		typeExpr == "float32", typeExpr == "float64",
		typeExpr == "complex64", typeExpr == "complex128",
		typeExpr == "byte", typeExpr == "rune", typeExpr == "uintptr":
		return "0"
	case typeExpr == "error":
		return "nil"
	case typeExpr == "any", typeExpr == "interface{}":
		return "nil"
	case strings.HasPrefix(typeExpr, "*"),
		strings.HasPrefix(typeExpr, "[]"),
		strings.HasPrefix(typeExpr, "map["),
		strings.HasPrefix(typeExpr, "chan "),
		strings.HasPrefix(typeExpr, "<-chan "),
		strings.HasPrefix(typeExpr, "chan<- "),
		strings.HasPrefix(typeExpr, "func("),
		strings.HasPrefix(typeExpr, "interface{"),
		strings.HasPrefix(typeExpr, "interface "):
		return "nil"
	case isInterfaceType(typeExpr, interfaceNames):
		// Named interface type (local or well-known cross-package). A
		// composite literal "T{}" is invalid for interfaces, so emit
		// "nil" — the typed-nil-interface zero value.
		return "nil"
	case primitiveAliases[typeExpr] != "":
		// Named primitive alias declared in the package (e.g.
		// `type BalanceCapReason string`). A composite literal `T{}`
		// is invalid for primitive-underlying named types, so emit the
		// underlying primitive's zero value.
		if z, ok := primitiveZero(primitiveAliases[typeExpr]); ok {
			return z
		}
		return typeExpr + "{}"
	case localNamedTypeRe.MatchString(typeExpr),
		qualifiedNamedTypeRe.MatchString(typeExpr):
		// Named type — assume a struct value and emit the composite-literal
		// zero value "T{}" / "pkg.T{}". This is the only safe default for
		// struct returns (where "nil" would not compile). Known limitation:
		// if the named type is actually an interface from another package
		// not in crossPackageInterfaces, "T{}" will not compile; either
		// hand-edit the function field, change the contract to return a
		// pointer, or extend the allow-list.
		return typeExpr + "{}"
	default:
		// Anything else (generics like "Result[T]", arrays "[N]T", etc.)
		// — fall back to nil. This is wrong for some shapes but matches
		// the long-standing behavior.
		return "nil"
	}
}