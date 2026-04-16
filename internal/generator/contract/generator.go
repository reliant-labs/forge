// Package contract parses Go interface definitions from contract.go files
// and generates mock_gen.go (function-field mock pattern) and
// middleware_gen.go (logging/tracing wrapper) in the same directory.
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
	"sort"
	"strings"
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
}

// Generate parses contractPath and writes mock_gen.go and middleware_gen.go
// next to it.
func Generate(contractPath string) error {
	cf, err := ParseContract(contractPath)
	if err != nil {
		return fmt.Errorf("parse contract: %w", err)
	}

	dir := filepath.Dir(contractPath)

	if err := writeMock(cf, dir); err != nil {
		return fmt.Errorf("generate mock: %w", err)
	}

	if err := writeMiddleware(cf, dir); err != nil {
		return fmt.Errorf("generate middleware: %w", err)
	}

	if err := writeTracing(cf, dir); err != nil {
		return fmt.Errorf("generate tracing: %w", err)
	}

	if err := writeMetrics(cf, dir); err != nil {
		return fmt.Errorf("generate metrics: %w", err)
	}

	return nil
}

// ParseContract parses a contract.go file and extracts all interface definitions.
func ParseContract(path string) (*ContractFile, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse file: %w", err)
	}

	cf := &ContractFile{
		Package: file.Name.Name,
		Imports: make(map[string]string),
	}

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
func writeMock(cf *ContractFile, dir string) error {
	imports := collectImports(cf, cf.Interfaces)
	// Always need "fmt" for the not-set error message (if there are methods).
	hasMethods := false
	for _, iface := range cf.Interfaces {
		if len(iface.Methods) > 0 {
			hasMethods = true
			break
		}
	}
	if hasMethods {
		addImport(&imports, "fmt")
	}

	data := templateData{
		Package:    cf.Package,
		Imports:    imports,
		Interfaces: cf.Interfaces,
	}

	var buf bytes.Buffer
	if err := mockTmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("execute mock template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("gofmt mock output: %w\n---\n%s", err, buf.String())
	}

	return os.WriteFile(filepath.Join(dir, "mock_gen.go"), formatted, 0644)
}

// writeMiddleware generates middleware_gen.go in dir.
func writeMiddleware(cf *ContractFile, dir string) error {
	imports := collectImports(cf, cf.Interfaces)
	// Always need "log/slog" for the struct field (present even with zero methods).
	// Only need "time" when there are methods (used in method wrappers).
	if len(cf.Interfaces) > 0 {
		addImport(&imports, "log/slog")
	}
	hasMethods := false
	for _, iface := range cf.Interfaces {
		if len(iface.Methods) > 0 {
			hasMethods = true
			break
		}
	}
	if hasMethods {
		addImport(&imports, "time")
	}

	data := templateData{
		Package:    cf.Package,
		Imports:    imports,
		Interfaces: cf.Interfaces,
	}

	var buf bytes.Buffer
	if err := middlewareTmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("execute middleware template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("gofmt middleware output: %w\n---\n%s", err, buf.String())
	}

	return os.WriteFile(filepath.Join(dir, "middleware_gen.go"), formatted, 0644)
}

// writeTracing generates tracing_gen.go in dir.
func writeTracing(cf *ContractFile, dir string) error {
	imports := collectImports(cf, cf.Interfaces)
	// Always need "go.opentelemetry.io/otel/trace" for the struct field.
	if len(cf.Interfaces) > 0 {
		addImport(&imports, "go.opentelemetry.io/otel/trace")
	}
	// Need "go.opentelemetry.io/otel/codes" when there are methods with error results.
	hasMethods := false
	for _, iface := range cf.Interfaces {
		if len(iface.Methods) > 0 {
			hasMethods = true
			break
		}
	}
	if hasMethods {
		addImport(&imports, "go.opentelemetry.io/otel/codes")
	}
	// Need "context" when there are methods without a context.Context parameter.
	for _, iface := range cf.Interfaces {
		for _, m := range iface.Methods {
			if !m.HasContext() {
				addImport(&imports, "context")
				break
			}
		}
	}

	data := templateData{
		Package:    cf.Package,
		Imports:    imports,
		Interfaces: cf.Interfaces,
	}

	var buf bytes.Buffer
	if err := tracingTmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("execute tracing template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("gofmt tracing output: %w\n---\n%s", err, buf.String())
	}

	return os.WriteFile(filepath.Join(dir, "tracing_gen.go"), formatted, 0644)
}

// writeMetrics generates metrics_gen.go in dir.
func writeMetrics(cf *ContractFile, dir string) error {
	imports := collectImports(cf, cf.Interfaces)
	// Always need "go.opentelemetry.io/otel/metric" for the struct fields.
	if len(cf.Interfaces) > 0 {
		addImport(&imports, "go.opentelemetry.io/otel/metric")
	}
	// Need "go.opentelemetry.io/otel/attribute" and "time" when there are methods.
	hasMethods := false
	for _, iface := range cf.Interfaces {
		if len(iface.Methods) > 0 {
			hasMethods = true
			break
		}
	}
	if hasMethods {
		addImport(&imports, "go.opentelemetry.io/otel/attribute")
		addImport(&imports, "time")
	}
	// Need "context" when there are methods without a context.Context parameter.
	for _, iface := range cf.Interfaces {
		for _, m := range iface.Methods {
			if !m.HasContext() {
				addImport(&imports, "context")
				break
			}
		}
	}

	data := templateData{
		Package:    cf.Package,
		Imports:    imports,
		Interfaces: cf.Interfaces,
	}

	var buf bytes.Buffer
	if err := metricsTmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("execute metrics template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("gofmt metrics output: %w\n---\n%s", err, buf.String())
	}

	return os.WriteFile(filepath.Join(dir, "metrics_gen.go"), formatted, 0644)
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
// for use in mock fallback returns. E.g. "nil, fmt.Errorf(...)".
func (m MethodDef) ZeroResults(mockName string) string {
	if len(m.Results) == 0 {
		return ""
	}
	var parts []string
	for i, r := range m.Results {
		// Last result that is "error" gets the fmt.Errorf fallback.
		if i == len(m.Results)-1 && r.TypeExpr == "error" {
			parts = append(parts, fmt.Sprintf(`fmt.Errorf("%s.%sFunc not set")`, mockName, m.Name))
		} else {
			parts = append(parts, zeroValue(r.TypeExpr))
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

// zeroValue returns the zero value literal for a Go type expression.
func zeroValue(typeExpr string) string {
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
	case strings.HasPrefix(typeExpr, "*"),
		strings.HasPrefix(typeExpr, "[]"),
		strings.HasPrefix(typeExpr, "map["),
		strings.HasPrefix(typeExpr, "chan "),
		strings.HasPrefix(typeExpr, "<-chan "),
		strings.HasPrefix(typeExpr, "func("):
		return "nil"
	default:
		// For interface types and named types from other packages
		// (e.g. sql.Result is an interface), nil is usually right
		// when it's used as an interface. For struct values we'd
		// need the type, but in practice contract interfaces return
		// pointers or interfaces alongside error.
		return "nil"
	}
}