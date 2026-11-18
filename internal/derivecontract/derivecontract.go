// Package derivecontract migrates one package from a hand-written contract
// to a derived one: `forge fix derive-contract <package-dir>`.
//
// Before, the package states its API twice — `type Service interface` in
// contract.go, and the same methods on the one type that implements it.
// After, the implementing type carries `//forge:contract`, and `forge
// generate` writes the interface into contract_gen.go from the type's
// exported methods (internal/generator/contract, derive.go). The migration
// is opt-in and per package, because only the author knows a package is
// single-implementation for good: a strategy interface with several
// implementations IS the abstraction and keeps its contract.go.
//
// The rewrite never changes the contract. It refuses every package where
// deriving would: no implementation or several, an interface that embeds
// another (derivation reads declared methods only), a differently-named
// contract interface (the derived one is always Service), an implementation
// that is generic, or that exports a method Service does not have (the
// derived contract would grow it). And before anything is written, it
// renders the derived contract from the rewritten package and compares it,
// method by method, with the Service it replaces.
//
// Plan computes the rewrite and Apply writes it, so `--dry-run` and the
// tests read exactly what a run would write.
package derivecontract

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
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/contractsrc"
	"github.com/reliant-labs/forge/internal/depsurface"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/pkgdirective"
)

// marker is the directive the migration puts on the implementing type.
const marker = "//" + contractsrc.Marker

// goDirective is the comment form gofmt treats as a directive
// (`//go:generate`, `//forge:contract`): no space after the slashes.
var goDirective = regexp.MustCompile(`^//[a-z0-9]+:[a-z0-9]`)

// Migration is the planned rewrite of one package. Nothing is on disk until
// Apply.
type Migration struct {
	// Dir is the package directory, project-relative and slash-separated.
	Dir string
	// Type is the implementing type that now carries //forge:contract, and
	// TypeFile the base name of the file declaring it.
	Type, TypeFile string
	// Methods are Service's methods, in their contract.go order.
	Methods []string
	// KeptContract reports that contract.go stays, holding what it declared
	// besides Service (the Deps struct, constants, functions).
	KeptContract bool
	// DepsFile is the base name of the file contract.go's remaining
	// dependency interfaces moved to ("deps.go"); "" when none moved.
	DepsFile string
	// Writes are the files to (re)write, Removes the ones to delete;
	// absolute paths.
	Writes  []FileWrite
	Removes []string
	// Rewrites describe the change, one human-readable line each.
	Rewrites []string
}

// FileWrite is one file's new content.
type FileWrite struct {
	Path    string
	Content []byte
}

// RefusedError is a package the migration will not rewrite, with every
// reason it found.
type RefusedError struct {
	Dir     string
	Reasons []string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s cannot derive its contract: %s", e.Dir, strings.Join(e.Reasons, "; "))
}

// Apply writes m: every file first, then the removals.
func Apply(m *Migration) error {
	for _, w := range m.Writes {
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(w.Path); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := atomicfile.WriteFile(w.Path, w.Content, mode); err != nil {
			return err
		}
	}
	for _, p := range m.Removes {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// pkgSrc is the package's non-test, non-generated files, parsed once.
type pkgSrc struct {
	root   string // the project root: its golangci config and go.mod
	dir    string
	name   string // package clause
	fset   *token.FileSet
	files  []*srcFile
	byName map[string]*srcFile
	names  map[string]string // import path → declared name (toolchain)
	// nonStd reports an unaliased import outside the standard library: its
	// name is the toolchain's to give, so the derived render needs a
	// Resolver.
	nonStd bool
}

type srcFile struct {
	name  string
	path  string
	src   []byte
	file  *ast.File
	quals map[string]string
	dot   bool
}

type ifaceMethod struct {
	name    string
	sig     string // normalized
	display string // as written, for messages
	doc     *ast.CommentGroup
	comment *ast.CommentGroup
}

type serviceDecl struct {
	file    *srcFile
	gd      *ast.GenDecl
	ts      *ast.TypeSpec
	doc     *ast.CommentGroup
	methods []ifaceMethod
}

type typeDecl struct {
	name    string
	file    *srcFile
	gd      *ast.GenDecl
	ts      *ast.TypeSpec
	methods map[string]methodDecl
}

type methodDecl struct {
	fd   *ast.FuncDecl
	file *srcFile
}

// edit replaces src[start:end] with text; an insertion has start == end.
// Edits at one offset apply in the order they were added.
type edit struct {
	start, end int
	text       string
	seq        int
}

type edits map[string][]edit

func (e edits) add(file string, start, end int, text string) {
	e[file] = append(e[file], edit{start: start, end: end, text: text, seq: len(e[file])})
}

func applyEdits(src []byte, es []edit) []byte {
	sorted := append([]edit(nil), es...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start > sorted[j].start
		}
		return sorted[i].seq > sorted[j].seq
	})
	out := append([]byte(nil), src...)
	for _, e := range sorted {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	return out
}

// Plan computes the migration of the package at dir (absolute, or relative
// to root). It returns a *RefusedError, naming every reason, for a package
// the migration would change the contract of.
//
//nolint:funlen // One linear story: refuse, find, rewrite, verify.
func Plan(root, dir string) (*Migration, error) {
	abs := dir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, dir)
	}
	abs = filepath.Clean(abs)
	rel := abs
	if r, err := filepath.Rel(root, abs); err == nil {
		rel = filepath.ToSlash(r)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a package directory", rel)
	}
	refuse := func(reasons ...string) error { return &RefusedError{Dir: rel, Reasons: reasons} }

	if reasons := preflight(abs); len(reasons) > 0 {
		return nil, refuse(reasons...)
	}
	p, err := parsePackage(root, abs)
	if err != nil {
		return nil, err
	}
	svc, reasons := findService(p)
	if len(reasons) > 0 {
		return nil, refuse(reasons...)
	}
	impl, reasons := findImplementation(p, svc)
	if len(reasons) > 0 {
		return nil, refuse(reasons...)
	}

	m := &Migration{Dir: rel, Type: impl.name, TypeFile: impl.file.name}
	for _, im := range svc.methods {
		m.Methods = append(m.Methods, im.name)
	}
	ed := edits{}
	cf := svc.file

	// 1. Service leaves contract.go.
	start, end := serviceRange(p, svc)
	ed.add(cf.name, start, end, "")

	// 2. Each method's doc moves onto the implementation, when it has none
	// of its own; a forge directive (method-level forge:no-observe) moves
	// either way, since the derived interface reads it there.
	var docMoved []string
	for _, im := range svc.methods {
		md := impl.methods[im.name]
		if moved := moveMethodDoc(p, ed, im, md); moved {
			docMoved = append(docMoved, im.name)
		}
	}

	// 3. The marker, and the interface's doc, go on the type.
	markType(p, ed, svc, impl)

	// 4. What is left of contract.go: kept, or its dependency interfaces
	// moved to deps.go and the file removed.
	outputs := map[string][]byte{} // base name → content; nil = removed
	contractSrc := applyEdits(cf.src, ed[cf.name])
	delete(ed, cf.name)
	settled, err := settleContract(p, cf, contractSrc, ed, m)
	if err != nil {
		return nil, err
	}
	for name, content := range settled {
		outputs[name] = content
	}

	// 5. Every other edited file: comment edits only, so gofmt alone —
	// its imports are untouched and its grouping is the author's.
	for name, es := range ed {
		f := p.byName[name]
		out, err := format.Source(applyEdits(f.src, es))
		if err != nil {
			return nil, fmt.Errorf("format %s: %w", f.path, err)
		}
		outputs[name] = out
	}

	// 6. Verify before writing anything: derive the contract from the
	// rewritten package and compare it with Service.
	if reasons, err := verify(p, svc, outputs); err != nil {
		return nil, err
	} else if len(reasons) > 0 {
		return nil, refuse(reasons...)
	}

	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(abs, name)
		if outputs[name] == nil {
			m.Removes = append(m.Removes, path)
			continue
		}
		m.Writes = append(m.Writes, FileWrite{Path: path, Content: outputs[name]})
	}

	m.Rewrites = append([]string{fmt.Sprintf("%s/%s: //forge:contract on %s — its exported methods are the contract now", rel, impl.file.name, impl.name)}, m.Rewrites...)
	if len(docMoved) > 0 {
		m.Rewrites = append(m.Rewrites, fmt.Sprintf("%s: Service's method docs moved onto %s (%s)", rel, impl.name, strings.Join(docMoved, ", ")))
	}
	return m, nil
}

// preflight refuses the packages the migration does not apply to at all.
func preflight(dir string) []string {
	if mt := contractsrc.MarkedTypes(dir); len(mt) > 0 {
		return []string{fmt.Sprintf("it already derives its contract from the //forge:contract type %s (%s:%d)", mt[0].Name, mt[0].File, mt[0].Line)}
	}
	if _, err := os.Stat(filepath.Join(dir, contractsrc.FileName)); err != nil {
		return []string{"it has no contract.go, so there is no hand-written Service to migrate"}
	}
	if mk, ok := pkgdirective.Find(dir, "forge:exclude-contract"); ok {
		return []string{fmt.Sprintf("%s:%d opts it out of contract generation (//forge:exclude-contract), and a derived contract is generated code; remove the directive first if the package should have one", filepath.Base(mk.File), mk.Line)}
	}
	var reasons []string
	hw := contractsrc.HandWrittenContracts(dir)
	found := false
	for _, h := range hw {
		switch {
		case h.Name != contractsrc.InterfaceName:
			reasons = append(reasons, fmt.Sprintf("its contract interface is %s (%s:%d), and a derived contract is always named %s: rename the interface and its uses first, or keep the hand-written contract", h.Name, h.File, h.Line, contractsrc.InterfaceName))
		case h.File != contractsrc.FileName:
			reasons = append(reasons, fmt.Sprintf("its %s interface is declared in %s, not contract.go; move it to contract.go first", h.Name, h.File))
		default:
			found = true
		}
	}
	if len(reasons) == 0 && !found {
		reasons = append(reasons, "contract.go declares no `type Service interface`: there is no contract to derive")
	}
	return reasons
}

func parsePackage(root, dir string) (*pkgSrc, error) {
	p := &pkgSrc{root: root, dir: dir, fset: token.NewFileSet(), byName: map[string]*srcFile{}}
	var need []string
	seen := map[string]bool{}
	for _, name := range contractsrc.SourceFiles(dir) {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(p.fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if p.name == "" {
			p.name = file.Name.Name
		}
		f := &srcFile{name: name, path: path, src: src, file: file}
		p.files = append(p.files, f)
		p.byName[name] = f
		for _, imp := range file.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil || imp.Name != nil || isStdlib(ip) {
				continue
			}
			p.nonStd = true
			if !seen[ip] {
				seen[ip] = true
				need = append(need, ip)
			}
		}
	}
	p.names = loadNames(dir, need)
	for _, f := range p.files {
		f.quals, f.dot = p.qualifiers(f.file)
	}
	return p, nil
}

// findService returns contract.go's Service, refusing a generic one and one
// that embeds an interface.
func findService(p *pkgSrc) (*serviceDecl, []string) {
	cf := p.byName[contractsrc.FileName]
	if cf == nil {
		return nil, []string{"contract.go did not parse as part of the package"}
	}
	var svc *serviceDecl
	for _, decl := range cf.file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != contractsrc.InterfaceName {
				continue
			}
			if _, ok := ts.Type.(*ast.InterfaceType); ok {
				doc := ts.Doc
				if doc == nil && len(gd.Specs) == 1 {
					doc = gd.Doc
				}
				svc = &serviceDecl{file: cf, gd: gd, ts: ts, doc: doc}
			}
		}
	}
	if svc == nil {
		return nil, []string{"contract.go declares no `type Service interface`: there is no contract to derive"}
	}
	var reasons []string
	if svc.ts.TypeParams != nil && len(svc.ts.TypeParams.List) > 0 {
		reasons = append(reasons, "Service is generic; a derived contract is not")
	}
	it := svc.ts.Type.(*ast.InterfaceType)
	for _, field := range it.Methods.List {
		ft, ok := field.Type.(*ast.FuncType)
		if !ok || len(field.Names) == 0 {
			reasons = append(reasons, fmt.Sprintf("Service embeds %s; a derived contract lists the implementation's declared methods only — write the embedded methods into Service first, or keep the hand-written contract", render(field.Type)))
			continue
		}
		for _, n := range field.Names {
			svc.methods = append(svc.methods, ifaceMethod{
				name:    n.Name,
				sig:     normSig(ft, cf.quals),
				display: strings.TrimPrefix(render(ft), "func"),
				doc:     field.Doc,
				comment: field.Comment,
			})
		}
	}
	if len(svc.methods) == 0 && len(reasons) == 0 {
		reasons = append(reasons, "Service declares no methods, so no type can be told apart as its implementation")
	}
	return svc, reasons
}

// findImplementation returns the one type that DECLARES every method of
// Service with the same signature, refusing every shape whose derived
// contract would not be Service.
func findImplementation(p *pkgSrc, svc *serviceDecl) (*typeDecl, []string) {
	var types []*typeDecl
	byName := map[string]*typeDecl{}
	for _, f := range p.files {
		for _, decl := range f.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, iface := ts.Type.(*ast.InterfaceType); iface {
					continue
				}
				t := &typeDecl{name: ts.Name.Name, file: f, gd: gd, ts: ts, methods: map[string]methodDecl{}}
				types = append(types, t)
				byName[t.name] = t
			}
		}
	}
	for _, f := range p.files {
		for _, decl := range f.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			if t := byName[receiverTypeName(fd.Recv.List[0].Type)]; t != nil {
				t.methods[fd.Name.Name] = methodDecl{fd: fd, file: f}
			}
		}
	}

	var impls []*typeDecl
	var near []string
	for _, t := range types {
		missing := 0
		var differ []string
		for _, im := range svc.methods {
			md, ok := t.methods[im.name]
			if !ok {
				missing++
				continue
			}
			if normSig(md.fd.Type, md.file.quals) != im.sig {
				differ = append(differ, fmt.Sprintf("%s.%s is %s%s, Service has %s%s", t.name, im.name, im.name, strings.TrimPrefix(render(md.fd.Type), "func"), im.name, im.display))
			}
		}
		switch {
		case missing == 0 && len(differ) == 0:
			impls = append(impls, t)
		case missing == 0:
			near = append(near, differ...)
		}
	}

	switch {
	case len(impls) == 0:
		reasons := []string{"no type in the package declares every method of Service with Service's signature (methods promoted from an embedded field do not count: the derived contract lists the type's declared methods only)"}
		return nil, append(reasons, near...)
	case len(impls) > 1:
		var where []string
		for _, t := range impls {
			where = append(where, fmt.Sprintf("%s (%s:%d)", t.name, t.file.name, p.fset.Position(t.ts.Pos()).Line))
		}
		return nil, []string{fmt.Sprintf("%d types implement Service — %s — and a derived contract has one source; with several implementations the interface IS the abstraction, so keep it hand-written", len(impls), strings.Join(where, ", "))}
	}

	t := impls[0]
	var reasons []string
	line := p.fset.Position(t.ts.Pos()).Line
	if t.ts.Assign.IsValid() {
		reasons = append(reasons, fmt.Sprintf("its implementation %s (%s:%d) is an alias; mark the defined type instead", t.name, t.file.name, line))
	}
	if t.ts.TypeParams != nil && len(t.ts.TypeParams.List) > 0 {
		reasons = append(reasons, fmt.Sprintf("its implementation %s (%s:%d) is generic, and a generic type cannot implement the non-generic Service by itself", t.name, t.file.name, line))
	}
	inService := map[string]bool{}
	for _, im := range svc.methods {
		inService[im.name] = true
	}
	var extra []string
	for name, md := range t.methods {
		if ast.IsExported(name) && !inService[name] {
			extra = append(extra, fmt.Sprintf("%s (%s:%d)", name, md.file.name, p.fset.Position(md.fd.Pos()).Line))
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		reasons = append(reasons, fmt.Sprintf("%s exports methods Service does not have — %s — and the derived contract would grow them; unexport them, or move them off %s, first", t.name, strings.Join(extra, ", "), t.name))
	}
	return t, reasons
}

// receiverTypeName is the base type name of a method receiver: `s *service`,
// `s service` and a generic `s *service[T]` all name "service".
func receiverTypeName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// serviceRange is the byte range of the Service declaration in contract.go:
// its doc, the declaration (or, in a `type (...)` group, its spec), a
// trailing comment, and the rest of its last line.
func serviceRange(p *pkgSrc, svc *serviceDecl) (int, int) {
	src := svc.file.src
	var start, end token.Pos
	if len(svc.gd.Specs) == 1 {
		start, end = svc.gd.Pos(), svc.gd.End()
		if svc.gd.Doc != nil {
			start = svc.gd.Doc.Pos()
		}
	} else {
		start, end = svc.ts.Pos(), svc.ts.End()
	}
	if svc.ts.Doc != nil && svc.ts.Doc.Pos() < start {
		start = svc.ts.Doc.Pos()
	}
	if svc.ts.Comment != nil && svc.ts.Comment.End() > end {
		end = svc.ts.Comment.End()
	}
	s, e := p.fset.Position(start).Offset, p.fset.Position(end).Offset
	for s > 0 && (src[s-1] == ' ' || src[s-1] == '\t') {
		s--
	}
	if i := bytes.IndexByte(src[e:], '\n'); i >= 0 {
		e += i + 1
	} else {
		e = len(src)
	}
	return s, e
}

func commentLines(cg *ast.CommentGroup) []string {
	if cg == nil {
		return nil
	}
	out := make([]string, 0, len(cg.List))
	for _, c := range cg.List {
		out = append(out, c.Text)
	}
	return out
}

// isForgeDirective reports a whole-line forge directive, spaced or not.
func isForgeDirective(text string) bool {
	return strings.HasPrefix(pkgdirective.TrimCommentMarkers(text), "forge:")
}

// missingDirectives are the forge directives of from that have is without.
func missingDirectives(from []string, have *ast.CommentGroup) []string {
	present := map[string]bool{}
	for _, l := range commentLines(have) {
		present[pkgdirective.TrimCommentMarkers(l)] = true
	}
	var out []string
	for _, l := range from {
		if isForgeDirective(l) && !present[pkgdirective.TrimCommentMarkers(l)] {
			out = append(out, l)
		}
	}
	return out
}

// moveMethodDoc puts an interface method's doc on its implementation:
// all of it when the method has no doc, else only its forge directives. It
// reports whether anything moved.
func moveMethodDoc(p *pkgSrc, ed edits, im ifaceMethod, md methodDecl) bool {
	doc := commentLines(im.doc)
	if len(doc) == 0 {
		doc = commentLines(im.comment)
	} else if im.comment != nil {
		doc = append(doc, missingDirectives(commentLines(im.comment), im.doc)...)
	}
	if len(doc) == 0 {
		return false
	}
	if md.fd.Doc == nil {
		at := p.fset.Position(md.fd.Pos()).Offset
		ed.add(md.file.name, at, at, strings.Join(doc, "\n")+"\n")
		return true
	}
	extra := missingDirectives(doc, md.fd.Doc)
	if len(extra) == 0 {
		return false
	}
	at := p.fset.Position(md.fd.Doc.End()).Offset
	ed.add(md.file.name, at, at, "\n"+strings.Join(extra, "\n"))
	return true
}

// markType adds //forge:contract to the implementation's doc. A type with
// no doc of its own takes Service's (its leading "Service" renamed to the
// type), so contract_gen.go keeps describing the contract; one with a doc
// keeps it, gaining only Service's forge directives (a package-level
// forge:no-observe on the interface reads from the marked type's doc).
func markType(p *pkgSrc, ed edits, svc *serviceDecl, t *typeDecl) {
	doc := t.ts.Doc
	if doc == nil && len(t.gd.Specs) == 1 {
		doc = t.gd.Doc
	}
	if doc != nil {
		lines := missingDirectives(commentLines(svc.doc), doc)
		last := doc.List[len(doc.List)-1].Text
		if len(lines) > 0 {
			last = lines[len(lines)-1]
		}
		// gofmt sets a doc's directives off with a blank `//` line; inside a
		// `type (...)` group it does not reformat, so write it that way.
		if !goDirective.MatchString(last) && strings.TrimSpace(last) != "//" {
			lines = append(lines, "//")
		}
		lines = append(lines, marker)
		at := p.fset.Position(doc.End()).Offset
		ed.add(t.file.name, at, at, "\n"+strings.Join(lines, "\n"))
		return
	}
	var lines []string
	for i, l := range commentLines(svc.doc) {
		if strings.HasPrefix(l, "//nolint") || strings.HasPrefix(l, "//go:") {
			continue // they described the interface declaration, not the type
		}
		if i == 0 {
			if rest, ok := strings.CutPrefix(l, "// "+contractsrc.InterfaceName+" "); ok {
				l = "// " + t.name + " " + rest
			}
		}
		lines = append(lines, l)
	}
	if len(lines) > 0 {
		lines = append(lines, "//")
	}
	lines = append(lines, marker)
	pos, indent := t.gd.Pos(), ""
	if len(t.gd.Specs) > 1 {
		pos, indent = t.ts.Pos(), "\t"
	}
	at := p.fset.Position(pos).Offset
	ed.add(t.file.name, at, at, strings.Join(lines, "\n"+indent)+"\n"+indent)
}

// settleContract decides what becomes of contract.go once Service is gone,
// given its edited source. It returns the files to write (nil content =
// remove) and may add edits for other files (the package comments contract.go
// carried).
//
// contract.go is removed when nothing but dependency interfaces (or nothing
// at all) is left in it: those move to deps.go, the file `forge fix
// narrow-deps` and `forge scaffold package` declare them in, which `forge
// generate` mocks beside the derived Service. When it still holds anything
// else — the Deps struct, constants, functions — it stays, as a companion
// of the marked type: its interfaces are mocked from there too. It also stays
// when moving it would change a build (a build constraint, a blank or dot
// import) or would clobber a deps.go that is not forge's.
//
//nolint:funlen,gocognit // The decision and its two outcomes read together.
func settleContract(p *pkgSrc, cf *srcFile, src []byte, ed edits, m *Migration) (map[string][]byte, error) {
	out := map[string][]byte{}
	rel := m.Dir + "/" + contractsrc.FileName
	fset := token.NewFileSet()
	nf, err := parser.ParseFile(fset, cf.path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("re-parse %s: %w", cf.path, err)
	}
	usedBefore := usedQualifiers(cf.file)

	var ifaceNames []string
	onlyIfaces := true
	var lastImport *ast.GenDecl
	for _, decl := range nf.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if ok && gd.Tok == token.IMPORT {
			lastImport = gd
			continue
		}
		if !ok || gd.Tok != token.TYPE {
			onlyIfaces = false
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			if _, iface := ts.Type.(*ast.InterfaceType); !iface {
				onlyIfaces = false
			}
			ifaceNames = append(ifaceNames, ts.Name.Name)
		}
	}
	keepReason := ""
	switch {
	case !onlyIfaces:
		keepReason = "it still declares more than dependency interfaces"
	case hasBuildConstraint(nf):
		keepReason = "it has a build constraint"
	case hasSideEffectImport(nf):
		keepReason = "it has a blank or dot import"
	case len(ifaceNames) > 0 && fileExists(filepath.Join(p.dir, depsurface.DepsFileName)) && !depsurface.HasDepsMarker(p.dir):
		keepReason = "deps.go exists without the `// forge:deps` marker, so it is not forge's to extend"
	}
	if keepReason != "" {
		content, err := pruneImports(p, cf.path, src, usedBefore)
		if err != nil {
			return nil, err
		}
		out[contractsrc.FileName] = content
		m.KeptContract = true
		m.Rewrites = append(m.Rewrites, fmt.Sprintf("%s: Service removed; the file stays, as the marked type's companion (%s)", rel, keepReason))
		return out, nil
	}

	// Removed. What follows the imports (or the package clause) is the
	// body: the dependency interfaces with their docs and any free-standing
	// comments, directives included.
	bodyStart := fset.Position(nf.Name.End()).Offset
	importsText := ""
	if lastImport != nil {
		for _, decl := range nf.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				importsText = string(src[fset.Position(gd.Pos()).Offset:fset.Position(lastImport.End()).Offset])
				break
			}
		}
		bodyStart = fset.Position(lastImport.End()).Offset
	}
	body := strings.TrimSpace(string(src[bodyStart:]))

	target := headerTarget(p, m)
	moveHeader(p, nf, src, fset, ed, target)
	out[contractsrc.FileName] = nil

	if len(ifaceNames) == 0 {
		// Free-standing comments only. Its package directives can live in
		// any file, so they go where the package comments went; its prose
		// described contract.go (the scaffold's dependency-interface
		// guide), so it goes with the file — and the report says so.
		var directives []string
		dropped := 0
		for _, cg := range nf.Comments {
			if fset.Position(cg.Pos()).Offset < bodyStart {
				continue
			}
			for _, c := range cg.List {
				if isForgeDirective(c.Text) {
					directives = append(directives, c.Text)
				} else if strings.TrimSpace(c.Text) != "//" {
					dropped++
				}
			}
		}
		if len(directives) > 0 {
			ed.add(target.name, len(target.src), len(target.src), "\n"+strings.Join(directives, "\n")+"\n")
		}
		note := ""
		if dropped > 0 {
			note = fmt.Sprintf(" (its %d free-standing comment line(s) described the file and were not kept; `git diff` shows them)", dropped)
		}
		m.Rewrites = append(m.Rewrites, fmt.Sprintf("%s: removed — Service was all it declared%s", rel, note))
		return out, nil
	}

	depsPath := filepath.Join(p.dir, depsurface.DepsFileName)
	m.DepsFile = depsurface.DepsFileName
	var content []byte
	if depsurface.HasDepsMarker(p.dir) {
		prev, err := os.ReadFile(depsPath)
		if err != nil {
			return nil, err
		}
		merged, err := mergeIntoDeps(prev, body, cf.file, depsPath)
		if err != nil {
			return nil, err
		}
		pf, err := parser.ParseFile(token.NewFileSet(), depsPath, prev, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", depsPath, err)
		}
		for q := range usedQualifiers(pf) {
			usedBefore[q] = true
		}
		content = merged
		m.Rewrites = append(m.Rewrites, fmt.Sprintf("%s/%s: gained %s from contract.go", m.Dir, depsurface.DepsFileName, strings.Join(ifaceNames, ", ")))
	} else {
		var b strings.Builder
		b.WriteString(depsurface.DepsFileMarker + "\n\npackage " + p.name + "\n\n")
		if importsText != "" {
			b.WriteString(importsText + "\n\n")
		}
		b.WriteString(body + "\n")
		content = []byte(b.String())
		m.Rewrites = append(m.Rewrites, fmt.Sprintf("%s/%s: created with %s, moved from contract.go", m.Dir, depsurface.DepsFileName, strings.Join(ifaceNames, ", ")))
	}
	content, err = pruneImports(p, depsPath, content, usedBefore)
	if err != nil {
		return nil, err
	}
	out[depsurface.DepsFileName] = content
	m.Rewrites = append(m.Rewrites, fmt.Sprintf("%s: removed — Service is derived, and its dependency interfaces are in deps.go", rel))
	return out, nil
}

// mergeIntoDeps appends body to an existing deps.go, adding the imports of
// contract.go (pruneImports drops the ones nothing uses).
func mergeIntoDeps(prev []byte, body string, contractFile *ast.File, path string) ([]byte, error) {
	src := append(append([]byte(nil), prev...), []byte("\n"+body+"\n")...)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("merge into %s: %w", path, err)
	}
	for _, imp := range contractFile.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		astutil.AddNamedImport(fset, f, name, ip)
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// headerTarget is the file contract.go's package comments move to: doc.go
// when the package has one, else the implementation's file.
func headerTarget(p *pkgSrc, m *Migration) *srcFile {
	if f := p.byName["doc.go"]; f != nil {
		return f
	}
	return p.byName[m.TypeFile]
}

// moveHeader carries contract.go's comments above its package clause —
// the package doc, with any package directive in it — to target. A line
// target's header already has (a license, forge's ownership banner) is not
// duplicated, nor is the old contract.go scaffold's own description line;
// the package doc joins target's own, or becomes it.
func moveHeader(p *pkgSrc, nf *ast.File, src []byte, fset *token.FileSet, ed edits, target *srcFile) {
	targetHeader := string(target.src[:p.fset.Position(target.file.Package).Offset])
	have := map[string]bool{}
	for _, l := range strings.Split(targetHeader, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var top []string
	doc := ""
	for _, cg := range nf.Comments {
		if cg.Pos() >= nf.Package {
			break
		}
		var lines []string
		for _, c := range cg.List {
			t := c.Text
			switch {
			case strings.Contains(t, "\n"):
				if strings.Contains(targetHeader, t) {
					continue
				}
			case have[t], strings.HasPrefix(t, "// yours:"), strings.HasPrefix(t, scaffoldContractDocLine):
				continue
			}
			lines = append(lines, t)
		}
		lines = trimBlankComments(lines)
		if len(lines) == 0 {
			continue
		}
		if cg == nf.Doc {
			doc = strings.Join(lines, "\n")
		} else {
			top = append(top, strings.Join(lines, "\n"))
		}
	}
	if len(top) > 0 {
		ed.add(target.name, 0, 0, strings.Join(top, "\n\n")+"\n\n")
	}
	if doc == "" {
		return
	}
	if target.file.Doc != nil {
		at := p.fset.Position(target.file.Doc.End()).Offset
		ed.add(target.name, at, at, "\n//\n"+doc)
		return
	}
	at := p.fset.Position(target.file.Package).Offset
	ed.add(target.name, at, at, doc+"\n")
}

// scaffoldContractDocLine opens the package comment forge's contract.go
// scaffold wrote before contracts could be derived; it describes the file
// being removed, so it does not move.
const scaffoldContractDocLine = "// Package contract; canonical pattern"

// trimBlankComments drops the bare `//` lines at either end of lines.
func trimBlankComments(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "//" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "//" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// pruneImports drops the imports src no longer uses that it used before
// (usedBefore): those only Service named. An import whose name is not known
// for certain is never dropped on a guess — if it is unused, the compiler
// says so — and goimports-style formatting follows the project's standard.
func pruneImports(p *pkgSrc, path string, src []byte, usedBefore map[string]bool) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	used := usedQualifiers(f)
	for _, imp := range append([]*ast.ImportSpec(nil), f.Imports...) {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name, local := "", p.nameOf(ip)
		if imp.Name != nil {
			name, local = imp.Name.Name, imp.Name.Name
		}
		if local == "_" || local == "." || used[local] || !usedBefore[local] {
			continue
		}
		astutil.DeleteNamedImport(fset, f, name, ip)
	}
	// A block left holding one import reads as `import "x"`.
	for _, decl := range f.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT && len(gd.Specs) == 1 && gd.Lparen.IsValid() {
			if is := gd.Specs[0].(*ast.ImportSpec); is.Doc == nil && is.Comment == nil {
				gd.Lparen, gd.Rparen = token.NoPos, token.NoPos
			}
		}
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return checksums.RewriteGoFormat(p.root).Format(path, buf.Bytes())
}

func hasBuildConstraint(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() >= f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build") || strings.HasPrefix(c.Text, "// +build") {
				return true
			}
		}
	}
	return false
}

func hasSideEffectImport(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Name != nil && (imp.Name.Name == "_" || imp.Name.Name == ".") {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// verify renders the derived contract from the rewritten package, in a
// scratch copy, and compares it with Service. A difference — or a refusal
// from the deriver itself — is returned as reasons; nothing has been
// written.
func verify(p *pkgSrc, svc *serviceDecl, outputs map[string][]byte) ([]string, error) {
	tmp, err := os.MkdirTemp("", "forge-derive-contract-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	files := map[string][]byte{}
	for _, f := range p.files {
		files[f.name] = f.src
	}
	for name, content := range outputs {
		if content == nil {
			delete(files, name)
			continue
		}
		files[name] = content
	}
	for name, content := range files {
		if err := atomicfile.WriteFile(filepath.Join(tmp, name), content, 0o644); err != nil {
			return nil, err
		}
	}
	var r *contract.Resolver
	if p.nonStd {
		r = contract.NewResolver(filepath.Join(p.dir, contractsrc.FileName))
	}
	rendered, err := contract.RenderDerivedContract(tmp, r)
	if err != nil {
		return []string{"the derived contract cannot be generated from the rewritten package: " + strings.ReplaceAll(err.Error(), tmp, p.dir)}, nil
	}
	gf, err := parser.ParseFile(token.NewFileSet(), contractsrc.DerivedFileName, rendered, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse the derived contract: %w", err)
	}
	quals, _ := p.qualifiers(gf)
	got := map[string]string{}
	for _, decl := range gf.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok || ts.Name.Name != contractsrc.InterfaceName {
				continue
			}
			for _, field := range it.Methods.List {
				if ft, ok := field.Type.(*ast.FuncType); ok && len(field.Names) == 1 {
					got[field.Names[0].Name] = normSig(ft, quals)
				}
			}
		}
	}
	var reasons []string
	for _, im := range svc.methods {
		sig, ok := got[im.name]
		switch {
		case !ok:
			reasons = append(reasons, fmt.Sprintf("the derived contract would lose Service.%s", im.name))
		case sig != im.sig:
			reasons = append(reasons, fmt.Sprintf("the derived contract would change Service.%s from %s to %s", im.name, im.sig, sig))
		}
		delete(got, im.name)
	}
	for name := range got {
		reasons = append(reasons, fmt.Sprintf("the derived contract would gain %s", name))
	}
	sort.Strings(reasons)
	return reasons, nil
}
