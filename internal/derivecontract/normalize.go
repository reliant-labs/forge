package derivecontract

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/reliant-labs/forge/internal/goexec"
)

// Signatures are compared by type, never by text: parameter names do not
// matter, and a package named through two different import aliases in two
// files is the same package. Every qualifier is rewritten to its quoted
// import path before comparing, so `pb.Job` in contract.go and `jobsv1.Job`
// in service.go are equal when both name the same import.

// qualifiers maps each local import name of file to its import path, and
// reports a dot import (whose types cannot be told from the package's own).
func (p *pkgSrc) qualifiers(file *ast.File) (map[string]string, bool) {
	out := map[string]string{}
	dot := false
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		switch {
		case imp.Name == nil:
			out[p.nameOf(path)] = path
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name == "_":
		default:
			out[imp.Name.Name] = path
		}
	}
	return out, dot
}

// nameOf is the identifier an unaliased import of path binds: the package's
// declared name when the toolchain resolved it, else the Go convention (last
// element, or the one before a /vN suffix), which is exact for the standard
// library.
func (p *pkgSrc) nameOf(path string) string {
	if n, ok := p.names[path]; ok {
		return n
	}
	elems := strings.Split(path, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' {
		if n, err := strconv.Atoi(last[1:]); err == nil && n >= 2 {
			return elems[len(elems)-2]
		}
	}
	return last
}

func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// loadNames asks the toolchain for the declared names of paths: a name-only
// `go list`, so it is cheap and works for packages that do not build. Best
// effort — a path it cannot resolve falls back to the convention.
func loadNames(dir string, paths []string) map[string]string {
	out := map[string]string{}
	if len(paths) == 0 {
		return out
	}
	cfg := &packages.Config{Mode: packages.NeedName, Dir: dir, Env: goexec.Env()}
	pkgs, err := packages.Load(cfg, paths...)
	if err != nil {
		return out
	}
	for _, lp := range pkgs {
		if lp.Name != "" && lp.PkgPath != "" {
			out[lp.PkgPath] = lp.Name
		}
	}
	return out
}

// normSig is a func type's parameter and result types, names dropped and
// qualifiers resolved to import paths through quals.
func normSig(ft *ast.FuncType, quals map[string]string) string {
	return "(" + normFields(ft.Params, quals) + ") (" + normFields(ft.Results, quals) + ")"
}

func normFields(fl *ast.FieldList, quals map[string]string) string {
	if fl == nil {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		t := normExpr(f.Type, quals)
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, ", ")
}

func normExpr(e ast.Expr, quals map[string]string) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			if path, ok := quals[id.Name]; ok {
				return strconv.Quote(path) + "." + x.Sel.Name
			}
		}
	case *ast.StarExpr:
		return "*" + normExpr(x.X, quals)
	case *ast.ParenExpr:
		return normExpr(x.X, quals)
	case *ast.Ellipsis:
		return "..." + normExpr(x.Elt, quals)
	case *ast.ArrayType:
		if x.Len == nil {
			return "[]" + normExpr(x.Elt, quals)
		}
		return "[" + render(x.Len) + "]" + normExpr(x.Elt, quals)
	case *ast.MapType:
		return "map[" + normExpr(x.Key, quals) + "]" + normExpr(x.Value, quals)
	case *ast.ChanType:
		switch x.Dir {
		case ast.SEND:
			return "chan<- " + normExpr(x.Value, quals)
		case ast.RECV:
			return "<-chan " + normExpr(x.Value, quals)
		default:
			return "chan " + normExpr(x.Value, quals)
		}
	case *ast.FuncType:
		return "func" + normSig(x, quals)
	case *ast.IndexExpr:
		return normExpr(x.X, quals) + "[" + normExpr(x.Index, quals) + "]"
	case *ast.IndexListExpr:
		parts := make([]string, len(x.Indices))
		for i, ix := range x.Indices {
			parts[i] = normExpr(ix, quals)
		}
		return normExpr(x.X, quals) + "[" + strings.Join(parts, ", ") + "]"
	case *ast.InterfaceType:
		if x.Methods == nil || len(x.Methods.List) == 0 {
			return "any"
		}
	}
	// Anything else (a struct or non-empty interface literal) compares by
	// its exact text: a spelling difference there reads as a mismatch,
	// which refuses rather than guesses.
	return render(e)
}

func render(e ast.Expr) string { return types.ExprString(e) }

// usedQualifiers is every identifier file uses as a selector's left side —
// a superset of the import names it uses (a local variable can be one), so
// an import it names is never wrongly called unused.
func usedQualifiers(file *ast.File) map[string]bool {
	used := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	return used
}
