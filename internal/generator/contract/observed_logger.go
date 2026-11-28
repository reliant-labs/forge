// File: internal/generator/contract/observed_logger.go
//
// The decorator's chain logs to the component's own logger.
//
// The chain's log and recovery layers used slog.Default(), so a component
// built with a logger of its own — a test's discarded one, a replica's tagged
// one — still wrote every failed call to the process default. The component's
// logger is its Deps.Logger, and the decorator is handed the component itself
// (NewServiceWithForgeMiddleware(inner) — the shape compose.go, NewInstrumented
// and every hand-written construction share), not its Deps. So the decorator
// reads it back off the implementing value, in the package's own source
// shape: when the implementing type holds a `Deps` field and Deps declares
// `Logger *slog.Logger`, the constructor binds that logger with
// observe.ComponentChain.WithLogger. Any other inner (a mock, another
// decorator, a nil logger) keeps the chain's default.
//
// WithLogger replaces only loggers the chain was not given explicitly: both of
// DefaultComponentChain's, and a seam's RecoverMiddleware(nil) /
// LogMiddleware(nil, …) — what observe_chain.go scaffolds. A seam that passes
// a logger of its own keeps it.
package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// loggerBinding is how the decorator reaches the component's Deps.Logger.
type loggerBinding struct {
	Impl      string // the implementing type
	DepsField string // its field of type Deps
	// ValueRecv is true when every target interface method has a value
	// receiver, so New may return the type by value as well as by pointer.
	ValueRecv bool
}

// findLoggerBinding reports how the decorator for dir's impl reaches its
// Deps.Logger, if it can: impl is a non-generic struct with a field of type
// Deps (named or embedded), and Deps is a struct with a `Logger *slog.Logger`
// field. Test and generated files are not read.
func findLoggerBinding(dir, impl string, target InterfaceDef) (loggerBinding, bool) {
	if impl == "" {
		return loggerBinding{}, false
	}
	targetMethods := make(map[string]bool, len(target.Methods))
	for _, m := range target.Methods {
		targetMethods[m.Name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return loggerBinding{}, false
	}
	var (
		fset       = token.NewFileSet()
		b          = loggerBinding{Impl: impl, ValueRecv: true}
		haveField  bool
		haveLogger bool
		methods    int
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return loggerBinding{}, false
		}
		// The decorator is shared by all targets, not just the generator's
		// GOOS/GOARCH. Read even excluded files and decline binding if a
		// relevant declaration can vary by build context.
		unconditional := loggerFileUnconditional(name, f)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || receiverType(d.Recv) != impl || !targetMethods[d.Name.Name] {
					continue
				}
				if !unconditional {
					return loggerBinding{}, false
				}
				methods++
				if !plainReceiver(d.Recv) {
					return loggerBinding{}, false // generic: no assertion names it
				}
				if _, ptr := d.Recv.List[0].Type.(*ast.StarExpr); ptr {
					b.ValueRecv = false
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if (ts.Name.Name == impl || ts.Name.Name == "Deps") && !unconditional {
						return loggerBinding{}, false
					}
					if ts.TypeParams != nil {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					switch ts.Name.Name {
					case impl:
						if field, ok := depsFieldName(st); ok {
							b.DepsField, haveField = field, true
						}
					case "Deps":
						haveLogger = hasSlogLoggerField(st, f)
					}
				}
			}
		}
	}
	if !haveField || !haveLogger || methods == 0 {
		return loggerBinding{}, false
	}
	return b, true
}

// loggerFileUnconditional rejects explicit build constraints and implicit
// GOOS/GOARCH filename constraints without consulting the generator's target.
func loggerFileUnconditional(name string, file *ast.File) bool {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:build") || strings.HasPrefix(comment.Text, "// +build") {
				return false
			}
		}
	}
	// Go recognizes _GOOS, _GOARCH and _GOOS_GOARCH before .go.
	// Checking both trailing components is conservative (it also rejects
	// _GOARCH_GOOS), and includes historical targets for older toolchains.
	parts := strings.Split(strings.TrimSuffix(name, ".go"), "_")
	for i := len(parts) - 1; i > 0 && i >= len(parts)-2; i-- {
		switch parts[i] {
		case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows",
			"386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390", "s390x", "sparc", "sparc64", "wasm":
			return false
		}
	}
	return true
}

// plainReceiver reports a receiver of a non-generic type: T or *T.
func plainReceiver(fl *ast.FieldList) bool {
	t := fl.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	_, ok := t.(*ast.Ident)
	return ok
}

// depsFieldName is the name of st's field of type Deps (by value): its name, or
// "Deps" when embedded.
func depsFieldName(st *ast.StructType) (string, bool) {
	for _, f := range st.Fields.List {
		id, ok := f.Type.(*ast.Ident)
		if !ok || id.Name != "Deps" {
			continue
		}
		if len(f.Names) == 0 {
			return "Deps", true
		}
		for _, name := range f.Names {
			// Blank fields cannot be selected by the generated decorator.
			if name.Name != "_" {
				return name.Name, true
			}
		}
	}
	return "", false
}

// hasSlogLoggerField reports a `Logger *slog.Logger` field in st, with slog
// resolved through file's imports.
func hasSlogLoggerField(st *ast.StructType, file *ast.File) bool {
	for _, f := range st.Fields.List {
		named := false
		for _, n := range f.Names {
			named = named || n.Name == "Logger"
		}
		if !named {
			continue
		}
		star, ok := f.Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Logger" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && importPathFor(file, pkg.Name) == "log/slog"
	}
	return false
}

// importPathFor is the path file imports under local name, or "".
func importPathFor(file *ast.File, local string) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == local {
			return path
		}
	}
	return ""
}

// render is the statement that rebinds chain to the component's logger.
func (b loggerBinding) render() string {
	logger := "component." + b.DepsField + ".Logger"
	if !b.ValueRecv {
		return "if component, ok := inner.(*" + b.Impl + "); ok && component != nil {\n" +
			"\tchain = chain.WithLogger(" + logger + ")\n}"
	}
	var s strings.Builder
	s.WriteString("switch component := inner.(type) {\n")
	s.WriteString("case *" + b.Impl + ":\n")
	s.WriteString("\tif component != nil {\n\t\tchain = chain.WithLogger(" + logger + ")\n\t}\n")
	s.WriteString("case " + b.Impl + ":\n")
	s.WriteString("\tchain = chain.WithLogger(" + logger + ")\n")
	s.WriteString("}")
	return s.String()
}
