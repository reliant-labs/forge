package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/checksums"
)

// reconcileMountHelper keeps service.go's mount helper named what
// mountMethodFor says, so the generated mounts, route sets, test helpers and
// mock — which all call the resolved name — find it.
//
// service.go is scaffolded once, usually before the service declares any
// RPC, so it is born with Register(mux *http.ServeMux, opts
// ...connect.HandlerOption). An RPC called Register added to the proto later
// collides with it: the stub scan (ScanExistingMethods) would count the
// helper as that RPC's implementation and emit no stub, *Service would not
// satisfy the generated handler interface, and nothing calling Mount would
// compile. So when the names disagree, the helper's declaration — and its
// doc comment's leading word — is renamed in place, and nothing else in the
// owned file is touched (the same surgical edit reconcileConnectIdentity
// makes). It renames back when the RPC is removed.
//
// Only the helper's exact shape is renamed — a *Service method whose first
// parameter is a *<pkg>.ServeMux, optionally followed by one variadic
// parameter, with no results — so an RPC method of either name is never
// mistaken for it. It stands down when the target name is already declared
// on *Service anywhere in the package, and when service.go is disowned or
// missing.
func reconcileMountHelper(svc ServiceDef, root, dir string, cs *checksums.FileChecksums) error {
	want := mountMethodFor(svc)
	from := "Register"
	if want == from {
		from = "Mount"
	}
	path := filepath.Join(dir, "service.go")
	if root != "" {
		if cs == nil {
			var err error
			if cs, err = checksums.Load(root); err != nil {
				return err
			}
		}
		if rel, err := filepath.Rel(root, path); err == nil && cs.IsDisowned(filepath.ToSlash(rel)) {
			return nil
		}
	}
	src, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil // a half-edited service.go is the author's; the build says what is wrong
	}
	var helper *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == from && isMountHelper(fn) {
			helper = fn
			break
		}
	}
	if helper == nil {
		return nil
	}
	existing, err := ScanExistingMethods(dir, true)
	if err != nil {
		return err
	}
	if existing[want] {
		return nil
	}

	// The helper's name, and the doc comment's leading word when it names
	// the helper; spans in file order, applied back to front so the earlier
	// offset stays valid.
	var spans [][2]int
	if helper.Doc != nil && len(helper.Doc.List) > 0 {
		if c := helper.Doc.List[0]; strings.HasPrefix(c.Text, "// "+from+" ") {
			start := fset.Position(c.Pos()).Offset + len("// ")
			spans = append(spans, [2]int{start, start + len(from)})
		}
	}
	spans = append(spans, [2]int{fset.Position(helper.Name.Pos()).Offset, fset.Position(helper.Name.End()).Offset})
	out := src
	for i := len(spans) - 1; i >= 0; i-- {
		out = append(append(append([]byte{}, out[:spans[i][0]]...), want...), out[spans[i][1]:]...)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, out, info.Mode().Perm())
}

// isMountHelper reports whether fn has the connect-go v1 mount helper's
// shape: func (s *Service) <name>(mux *<pkg>.ServeMux[, opts ...T]), no
// results.
func isMountHelper(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Service" {
		return false
	}
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		return false
	}
	params := fn.Type.Params.List
	if len(params) == 0 || len(params) > 2 || len(params[0].Names) > 1 {
		return false
	}
	if len(params) == 2 {
		if _, variadic := params[1].Type.(*ast.Ellipsis); !variadic || len(params[1].Names) > 1 {
			return false
		}
	}
	p, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := p.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ServeMux"
}
