package depsurface

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// DepsFileName is where Narrow declares the interfaces of a package that has
// no contract.go; DepsFileMarker is its first line, which `forge generate`
// reads to keep the file's mocks (mock_gen.go) in step with it.
const (
	DepsFileName   = "deps.go"
	DepsFileMarker = "// forge:deps — consumer-declared interfaces for this package's Deps."
)

// NarrowResult is what Narrow did to one package.
type NarrowResult struct {
	Dir string
	// File is the project-relative file the interfaces were written to:
	// the package's contract.go, or deps.go.
	File     string
	Narrowed []Narrowed
	Skipped  []Skipped
}

// Narrowed is one field Narrow retyped.
type Narrowed struct {
	Field, From, To string
	Methods         []string
}

// Skipped is one interface field Narrow left alone, and why.
type Skipped struct {
	Field, Type, Reason string
}

// Narrow rewrites every foreign interface field of dir's Deps that declares
// methods the package never calls to an interface the package declares
// itself, holding exactly the methods it calls (design §2, K1). The
// composition still fills the field with the same value: Go satisfies the
// narrower interface structurally, so compose.go needs no edit.
//
// A field whose value the package passes on (Escapes) is left alone — what it
// needs is not all visible here — as is a field the package never calls
// through at all (delete it instead), and an interface the package already
// declares (edit it in place).
func Narrow(projectDir, dir string) (*NarrowResult, error) {
	pkgs, err := Analyze(projectDir, []string{dir})
	if err != nil {
		return nil, err
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("%s: no component package with a Deps struct that type-checks (run `go build ./%s` to see why)", dir, dir)
	}
	p := pkgs[0]
	res := &NarrowResult{Dir: p.Dir}
	target := filepath.Join(projectDir, filepath.FromSlash(p.Dir), "contract.go")
	if _, err := os.Stat(target); err != nil {
		target = filepath.Join(projectDir, filepath.FromSlash(p.Dir), DepsFileName)
	}
	res.File, _ = filepath.Rel(projectDir, target)
	res.File = filepath.ToSlash(res.File)

	w := newIfaceWriter(p)
	for path, name := range p.importNames(res.File) {
		w.imports[path], w.names[name] = name, path
	}
	retype := map[string]string{} // field → new type name
	for _, f := range p.Fields {
		switch {
		case !f.Interface:
			continue
		case !f.Foreign:
			if len(Unused(f)) > 0 {
				res.Skipped = append(res.Skipped, Skipped{f.Name, f.Type, "the package declares this interface itself: delete the methods it does not call there"})
			}
			continue
		case len(Unused(f)) == 0:
			continue
		case f.Escapes:
			res.Skipped = append(res.Skipped, Skipped{f.Name, f.Type, "its value is passed on or stored, so the calls in this package are not everything it needs"})
			continue
		case len(f.Used) == 0:
			res.Skipped = append(res.Skipped, Skipped{f.Name, f.Type, "the package never calls it: delete the field"})
			continue
		}
		name, err := w.declare(f)
		if err != nil {
			return nil, err
		}
		retype[f.Name] = name
		res.Narrowed = append(res.Narrowed, Narrowed{Field: f.Name, From: f.Type, To: name, Methods: sortedUsed(f)})
	}
	if len(retype) == 0 {
		return res, nil
	}
	if err := w.write(target); err != nil {
		return nil, err
	}
	if err := retypeFields(filepath.Join(projectDir, filepath.FromSlash(p.DepsFile)), retype, p.importNames(p.DepsFile)); err != nil {
		return nil, err
	}
	return res, nil
}

func sortedUsed(f Field) []string {
	out := make([]string, 0, len(f.Used))
	for m := range f.Used {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ifaceWriter renders interface declarations in the consumer's package,
// collecting the imports their signatures need.
type ifaceWriter struct {
	p       *Package
	taken   map[string]bool   // identifiers in use in the package scope
	imports map[string]string // import path → name used
	names   map[string]string // name used → import path
	added   map[string]bool   // imports the declarations introduce
	decls   bytes.Buffer
}

func newIfaceWriter(p *Package) *ifaceWriter {
	w := &ifaceWriter{p: p, taken: map[string]bool{}, imports: map[string]string{}, names: map[string]string{}, added: map[string]bool{}}
	for _, n := range p.pkg.Types.Scope().Names() {
		w.taken[n] = true
	}
	return w
}

// qualifier names a package in the consumer's file, choosing a unique import
// name for it.
func (w *ifaceWriter) qualifier(pkg *types.Package) string {
	if pkg == w.p.pkg.Types {
		return ""
	}
	if n, ok := w.imports[pkg.Path()]; ok {
		return n
	}
	name := pkg.Name()
	for i := 2; w.names[name] != "" || w.taken[name]; i++ {
		name = pkg.Name() + strconv.Itoa(i)
	}
	w.imports[pkg.Path()], w.names[name] = name, pkg.Path()
	w.added[pkg.Path()] = true
	return name
}

// declare renders the narrowed interface for f and returns its name.
func (w *ifaceWriter) declare(f Field) (string, error) {
	named, ok := f.typ.(*types.Named)
	if !ok {
		return "", fmt.Errorf("Deps.%s: %s is not a named interface", f.Name, f.Type)
	}
	iface := named.Underlying().(*types.Interface)
	name := named.Obj().Name()
	if w.taken[name] {
		name = f.Name + named.Obj().Name()
	}
	if w.taken[name] {
		return "", fmt.Errorf("Deps.%s: both %s and %s are taken in package %s — declare the interface by hand", f.Name, named.Obj().Name(), name, w.p.pkg.Name)
	}
	w.taken[name] = true
	fmt.Fprintf(&w.decls, "\n// %s is the part of %s that this package calls through Deps.%s:\n", name, types.TypeString(named, w.qualifier), f.Name)
	fmt.Fprintf(&w.decls, "// declared here, by its consumer, so the package knows only what it uses\n// (forge fix narrow-deps). Add a method when the package calls one.\n")
	fmt.Fprintf(&w.decls, "type %s interface {\n", name)
	for _, m := range sortedUsed(f) {
		obj, _, _ := types.LookupFieldOrMethod(iface, false, nil, m)
		fn, ok := obj.(*types.Func)
		if !ok {
			return "", fmt.Errorf("Deps.%s: %s has no method %s", f.Name, f.Type, m)
		}
		var sig bytes.Buffer
		types.WriteSignature(&sig, fn.Type().(*types.Signature), w.qualifier)
		fmt.Fprintf(&w.decls, "\t%s%s\n", m, sig.String())
	}
	w.decls.WriteString("}\n")
	return name, nil
}

// write appends the declarations to target (creating deps.go when absent)
// with the imports they need.
func (w *ifaceWriter) write(target string) error {
	src, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		src = []byte(DepsFileMarker + "\n// The interfaces below are this package's own declarations of what it\n// needs from its Deps; mock_gen.go mocks them.\npackage " + w.p.pkg.Name + "\n")
	} else if err != nil {
		return err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, append(append([]byte{}, src...), w.decls.Bytes()...), parser.ParseComments)
	if err != nil {
		return fmt.Errorf("narrow-deps rendered an unparseable %s (a forge bug — please report it): %w", target, err)
	}
	added := map[string]string{}
	for p := range w.added {
		added[p] = w.imports[p]
	}
	addImports(fset, file, added)
	return writeFormatted(fset, file, target)
}

// addImports adds each path under its name, omitting the name when it is
// the path's own.
func addImports(fset *token.FileSet, file *ast.File, imports map[string]string) {
	for p, name := range imports {
		if name == path.Base(p) {
			astutil.AddImport(fset, file, p)
		} else {
			astutil.AddNamedImport(fset, file, name, p)
		}
	}
}

func writeFormatted(fset *token.FileSet, file *ast.File, target string) error {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return fmt.Errorf("format %s: %w", target, err)
	}
	return os.WriteFile(target, buf.Bytes(), 0o644)
}

// importNames maps each import of the project-relative file to its name in
// that file, read off the type-checker (a package's name need not be its
// path's last element).
func (p *Package) importNames(rel string) map[string]string {
	out := map[string]string{}
	for _, f := range p.pkg.Syntax {
		if !strings.HasSuffix(filepath.ToSlash(p.pkg.Fset.Position(f.Pos()).Filename), "/"+rel) {
			continue
		}
		for _, spec := range f.Imports {
			if pn := p.pkg.TypesInfo.PkgNameOf(spec); pn != nil {
				out[pn.Imported().Path()] = pn.Name()
			}
		}
	}
	return out
}

// retypeFields rewrites the type of each named Deps field in file, then
// drops every import (names: path → name in file) nothing uses any more.
func retypeFields(file string, retype map[string]string, names map[string]string) error {
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.ParseComments)
	if err != nil {
		return err
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Deps" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range st.Fields.List {
			if len(field.Names) != 1 {
				continue
			}
			if to, ok := retype[field.Names[0].Name]; ok {
				edits = append(edits, edit{fset.Position(field.Type.Pos()).Offset, fset.Position(field.Type.End()).Offset, to})
			}
		}
		return false
	})
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		src = append(src[:e.start], append([]byte(e.text), src[e.end:]...)...)
	}
	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, file, src, parser.ParseComments)
	if err != nil {
		return err
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name, known := names[p]
		if !known || name == "_" || name == "." {
			continue
		}
		if !usesPackageName(f, name) {
			if imp.Name != nil {
				astutil.DeleteNamedImport(fset, f, imp.Name.Name, p)
			} else {
				astutil.DeleteImport(fset, f, p)
			}
		}
	}
	return writeFormatted(fset, f, file)
}

// usesPackageName reports whether any selector in f is name.X.
func usesPackageName(f *ast.File, name string) bool {
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
				used = true
			}
		}
		return !used
	})
	return used
}

// HasDepsMarker reports whether dir holds a deps.go Narrow wrote.
func HasDepsMarker(dir string) bool {
	src, err := os.ReadFile(filepath.Join(dir, DepsFileName))
	return err == nil && strings.HasPrefix(string(src), DepsFileMarker)
}
