package deadcodeguard

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Rule names, used as the first column of every Finding and as the stable key
// a ledger entry refers to.
const (
	RulePhantomField = "phantom-field"
	RuleNoopFunc     = "noop-func"
)

// Finding is one violation. Key is stable across line moves — it names the
// declaration, not its position — so a quarantine ledger keyed on it does not
// rot the moment someone reformats a file.
type Finding struct {
	Rule string
	// Key is "<short pkg path>.<Type>.<Field>" for phantom-field and
	// "<short pkg path>.<Func>" for noop-func.
	Key string
	// Decl is the repo-relative "file:line" of the declaration.
	Decl string
	// Detail explains the specific violation in one line.
	Detail string
	// Evidence lists the repo-relative sites that make the case.
	Evidence []string
}

func (f Finding) String() string {
	s := fmt.Sprintf("[%s] %s (%s): %s", f.Rule, f.Key, f.Decl, f.Detail)
	for _, e := range f.Evidence {
		s += "\n        " + e
	}
	return s
}

// ForgeInternalPrefix is the import-path prefix whose declarations forge's own
// guard judges. See the package doc for why this is `internal/` and not the
// whole module.
const ForgeInternalPrefix = "github.com/reliant-labs/forge/internal/"

// Scan loads every package rooted at root (the directory holding the go.mod)
// and returns all findings, sorted by rule then key. Only declarations whose
// import path starts with judgePrefix are judged; everything loaded is still
// read for USES, which is what makes the "no production writer exists" verdict
// sound rather than merely local.
func Scan(root, judgePrefix string) ([]Finding, error) {
	return scanWith(root, judgePrefix, nil)
}

// ScanStandalone is Scan for a module that deliberately sits OUTSIDE any
// enclosing go.work. forge's workspace lists only `.` and `pkg`, so the
// planted-defect fixture under testdata/ can only be loaded with the workspace
// switched off.
func ScanStandalone(root, judgePrefix string) ([]Finding, error) {
	return scanWith(root, judgePrefix, []string{"GOWORK=off"})
}

func scanWith(root, judgePrefix string, extraEnv []string) ([]Finding, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Dir: root,
		// Tests must be loaded: "written only by tests" is half the
		// phantom-field verdict, and a scan that cannot see test files
		// would report a legitimately test-driven field identically to
		// a dead one.
		Tests: true,
	}
	if len(extraEnv) > 0 {
		cfg.Env = append(os.Environ(), extraEnv...)
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", root, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("loaded 0 packages from %s — a scan that sees nothing passes everything", root)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, fmt.Sprintf("%s: %v", p.PkgPath, e))
		}
	})
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("package load reported %d error(s); the analysis would be unsound:\n  %s",
			len(loadErrs), strings.Join(loadErrs[:min(len(loadErrs), 10)], "\n  "))
	}

	s := &scan{root: root, judge: judgePrefix, fields: map[fieldKey]*fieldFacts{}, counted: map[string]bool{}}
	s.collectGenerated(pkgs)
	s.collectFieldDecls(pkgs)
	s.collectFieldUses(pkgs)
	out := s.phantomFields()
	out = append(out, s.noopFuncs(pkgs)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// phantom-field
// ─────────────────────────────────────────────────────────────────────────────

type fieldKey struct{ pkg, typ, field string }

func (k fieldKey) String() string { return short(k.pkg) + "." + k.typ + "." + k.field }

type fieldFacts struct {
	decl                             token.Position
	prodRead, prodWrite, testWrite   int
	readSites, writeSites, testSites []string
}

type scan struct {
	root      string
	judge     string
	generated map[string]bool
	fields    map[fieldKey]*fieldFacts
	// counted dedupes a use by file+offset. With Tests:true a package is
	// type-checked more than once (p, p [p.test]); without this, every use
	// in a non-test file of a tested package is counted twice.
	counted map[string]bool
}

func (s *scan) collectGenerated(pkgs []*packages.Package) {
	s.generated = map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, f := range p.Syntax {
			if ast.IsGenerated(f) {
				s.generated[p.Fset.Position(f.Pos()).Filename] = true
			}
		}
	})
}

// collectFieldDecls records every judgeable struct field. Three kinds of field
// are deliberately not judged; each exclusion is a false-positive class that
// showed up on the real tree.
func (s *scan) collectFieldDecls(pkgs []*packages.Package) {
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.Types == nil || !strings.HasPrefix(p.PkgPath, s.judge) {
			return
		}
		sc := p.Types.Scope()
		for _, name := range sc.Names() {
			tn, ok := sc.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			st, ok := tn.Type().Underlying().(*types.Struct)
			if !ok {
				continue
			}
			// (1) A struct carrying ANY encoding tag is a serialization
			// target: json/yaml/env decoders write its fields by
			// reflection, which no call graph can see. Judging one field
			// of such a struct would be judging half the writers.
			tagged := false
			for i := 0; i < st.NumFields(); i++ {
				if strings.TrimSpace(st.Tag(i)) != "" {
					tagged = true
					break
				}
			}
			if tagged {
				continue
			}
			for i := 0; i < st.NumFields(); i++ {
				fv := st.Field(i)
				pos := p.Fset.Position(fv.Pos())
				// (2) A field declared in generated code is forge's own
				// emitter output; a human cannot durably remove it,
				// because the next `forge generate` re-emits it.
				if s.generated[pos.Filename] {
					continue
				}
				// (3) A func- or interface-typed field is an injection
				// seam by construction: nil means "use the default", and
				// production legitimately never overrides it.
				switch fv.Type().Underlying().(type) {
				case *types.Signature, *types.Interface:
					continue
				}
				k := fieldKey{p.PkgPath, name, fv.Name()}
				if s.fields[k] == nil {
					s.fields[k] = &fieldFacts{decl: pos}
				}
			}
		}
	})
}

func (s *scan) collectFieldUses(pkgs []*packages.Package) {
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.TypesInfo == nil {
			return
		}
		for _, f := range p.Syntax {
			file := p.Fset.Position(f.Pos()).Filename
			isTest := strings.HasSuffix(file, "_test.go")
			writes := writtenExprs(f, p.TypesInfo)

			ast.Inspect(f, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.SelectorExpr:
					sel, ok := p.TypesInfo.Selections[e]
					if !ok || sel.Kind() != types.FieldVal {
						return true
					}
					fv, _ := sel.Obj().(*types.Var)
					if fv == nil {
						return true
					}
					if k, ok := ownerKey(sel.Recv(), fv); ok {
						s.record(k, p.Fset.Position(e.Pos()), isTest, writes[ast.Expr(e)])
					}
				case *ast.CompositeLit:
					st, name, pkgPath := structOf(p.TypesInfo.TypeOf(e))
					if st == nil {
						return true
					}
					for i, el := range e.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok {
								s.record(fieldKey{pkgPath, name, id.Name},
									p.Fset.Position(id.Pos()), isTest, true)
							}
							continue
						}
						// Positional literal T{a, b} writes every field.
						if i < st.NumFields() {
							s.record(fieldKey{pkgPath, name, st.Field(i).Name()},
								p.Fset.Position(el.Pos()), isTest, true)
						}
					}
				}
				return true
			})
		}
	})
}

func (s *scan) record(k fieldKey, pos token.Position, isTest, write bool) {
	f := s.fields[k]
	if f == nil {
		return // not a judgeable field
	}
	id := fmt.Sprintf("%s:%d:%v", pos.Filename, pos.Offset, write)
	if s.counted[id] {
		return
	}
	s.counted[id] = true
	where := s.rel(pos)
	switch {
	case isTest && write:
		f.testWrite++
		f.testSites = appendCapped(f.testSites, where)
	case isTest:
		// A test-only read proves nothing either way.
	case write:
		f.prodWrite++
		f.writeSites = appendCapped(f.writeSites, where)
	default:
		f.prodRead++
		f.readSites = appendCapped(f.readSites, where)
	}
}

func (s *scan) phantomFields() []Finding {
	var out []Finding
	for k, f := range s.fields {
		if f.prodWrite > 0 || f.prodRead == 0 {
			continue
		}
		detail := fmt.Sprintf("read by production code %d× and written by production code 0× — "+
			"every read observes the zero value", f.prodRead)
		ev := []string{"production reads: " + strings.Join(f.readSites, ", ")}
		if f.testWrite > 0 {
			detail += fmt.Sprintf("; the only writers are %d test site(s), so those tests exercise "+
				"a data shape production cannot produce", f.testWrite)
			ev = append(ev, "test-only writes: "+strings.Join(f.testSites, ", "))
		}
		out = append(out, Finding{
			Rule:     RulePhantomField,
			Key:      k.String(),
			Decl:     s.rel(f.decl),
			Detail:   detail,
			Evidence: ev,
		})
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// noop-func
// ─────────────────────────────────────────────────────────────────────────────

// noopFuncs reports plain functions whose whole body is returns of zero values
// yet which declare parameters.
//
// METHODS ARE NOT JUDGED, on purpose. A method's signature is dictated by an
// interface it satisfies, and the Null Object pattern — a stub whose whole
// point is to do nothing for every call — is a legitimate, deliberate shape
// forge uses (secrets.noopProvider, audit.servedAllRegistry). A plain function
// has no such external constraint: its author chose the parameters, so
// parameters the body cannot read are a claim nothing backs.
func (s *scan) noopFuncs(pkgs []*packages.Package) []Finding {
	var out []Finding
	seen := map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.TypesInfo == nil || !strings.HasPrefix(p.PkgPath, s.judge) {
			return
		}
		for _, f := range p.Syntax {
			file := p.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(file, "_test.go") || s.generated[file] {
				continue
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Body == nil {
					continue
				}
				if countParams(fd.Type.Params) == 0 {
					continue
				}
				if !bodyIsZeroReturnsOnly(fd.Body, p.TypesInfo) {
					continue
				}
				pos := p.Fset.Position(fd.Pos())
				key := short(p.PkgPath) + "." + fd.Name.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, Finding{
					Rule:   RuleNoopFunc,
					Key:    key,
					Decl:   s.rel(pos),
					Detail: fmt.Sprintf("declares %d parameter(s) but its whole body returns zero values — the parameters cannot affect anything", countParams(fd.Type.Params)),
					Evidence: []string{
						"callers compute arguments this function discards; if that is intended, delete the function and the argument computation with it",
					},
				})
			}
		}
	})
	return out
}

// bodyIsZeroReturnsOnly reports whether every path through b returns zero
// values and nothing in b can have an effect.
//
// Branching is allowed — that is the static shadow of the matchServicePort
// defect, where an elaborate cascade of conditions all led to the same 0 — but
// only when the branch conditions are CALL-FREE. A condition that calls
// something may be where the real work happens, and a rule that flagged those
// would be flagging functions that do something.
//
// An empty body is NOT a match: a zero-statement function has no return values
// to lie about and is the ordinary shape of a build-tag stub.
func bodyIsZeroReturnsOnly(b *ast.BlockStmt, info *types.Info) bool {
	if len(b.List) == 0 {
		return false
	}
	var ok func(stmts []ast.Stmt) bool
	ok = func(stmts []ast.Stmt) bool {
		for _, st := range stmts {
			switch s := st.(type) {
			case *ast.ReturnStmt:
				for _, r := range s.Results {
					if !isZeroValue(r, info) {
						return false
					}
				}
			case *ast.IfStmt:
				if s.Init != nil || containsCall(s.Cond) {
					return false
				}
				if !ok(s.Body.List) {
					return false
				}
				switch e := s.Else.(type) {
				case nil:
				case *ast.BlockStmt:
					if !ok(e.List) {
						return false
					}
				case *ast.IfStmt:
					if !ok([]ast.Stmt{e}) {
						return false
					}
				default:
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	return ok(b.List)
}

func containsCall(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if _, isCall := n.(*ast.CallExpr); isCall {
			found = true
		}
		return !found
	})
	return found
}

func isZeroValue(e ast.Expr, info *types.Info) bool {
	switch v := e.(type) {
	case *ast.Ident:
		if v.Name == "nil" || v.Name == "false" {
			return true
		}
		if tv, ok := info.Types[e]; ok && tv.Value != nil {
			switch tv.Value.String() {
			case "0", `""`, "false":
				return true
			}
		}
	case *ast.BasicLit:
		switch v.Kind {
		case token.INT, token.FLOAT:
			return v.Value == "0"
		case token.STRING:
			return v.Value == `""` || v.Value == "``"
		}
	case *ast.CompositeLit:
		// map[string]string{} / []T{} / T{} — an empty container is the
		// zero-information answer just as much as nil is.
		return len(v.Elts) == 0
	}
	return false
}

func countParams(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	n := 0
	for _, f := range fl.List {
		n += max(1, len(f.Names))
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared plumbing
// ─────────────────────────────────────────────────────────────────────────────

// writtenExprs returns the expressions in f that are written rather than read.
// Assignment targets and address-of operands are the obvious cases. The third
// is subtler and was a false-positive class on the real tree: `x.f.M()` where M
// has a POINTER receiver mutates f in place, so `mu.Lock()` must not count as
// a read of a field nothing writes. A value-receiver method call stays a read.
func writtenExprs(f *ast.File, info *types.Info) map[ast.Expr]bool {
	out := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range s.Lhs {
				markWriteChain(lhs, out)
			}
		case *ast.IncDecStmt:
			markWriteChain(s.X, out)
		case *ast.UnaryExpr:
			if s.Op == token.AND {
				markWriteChain(s.X, out)
			}
		case *ast.SelectorExpr:
			sel, ok := info.Selections[s]
			if !ok || sel.Kind() != types.MethodVal {
				return true
			}
			fn, _ := sel.Obj().(*types.Func)
			if fn == nil {
				return true
			}
			sig, _ := fn.Type().(*types.Signature)
			if sig == nil || sig.Recv() == nil {
				return true
			}
			if _, isPtr := sig.Recv().Type().(*types.Pointer); isPtr {
				out[s.X] = true
			}
		}
		return true
	})
	return out
}

// markWriteChain marks e as written AND every enclosing selector on the way to
// it, because assigning through a chain mutates every struct along that chain.
//
// This was a false-positive class on the real tree. `g.Features.Codegen = off()`
// marks only `g.Features.Codegen`; the inner `g.Features` selector is then
// visited on its own and counted as a READ. The result is a field with dozens
// of reads and zero writes — a textbook phantom — that is in fact written in
// production on every non-service scaffold. Reporting it would have led to
// deleting working code, and a rule that cries wolf gets weakened, which is the
// failure mode that defeats the guard entirely.
//
// Descends selectors, index expressions and derefs; `a.b[i].c = v` mutates
// `a.b` just as surely as `a.b.c = v` does.
func markWriteChain(e ast.Expr, out map[ast.Expr]bool) {
	for {
		out[e] = true
		switch x := e.(type) {
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return
		}
	}
}

// ownerKey resolves the named struct type that DECLARES fv, starting from the
// receiver type of the selection. Walking embedded fields is what makes a
// promoted-field selection (`x.Embedded.F` written as `x.F`) attribute to the
// type that really owns F.
func ownerKey(recv types.Type, fv *types.Var) (fieldKey, bool) {
	var find func(t types.Type, depth int) (fieldKey, bool)
	find = func(t types.Type, depth int) (fieldKey, bool) {
		if depth > 8 {
			return fieldKey{}, false
		}
		if ptr, ok := t.(*types.Pointer); ok {
			t = ptr.Elem()
		}
		named, _ := t.(*types.Named)
		var st *types.Struct
		if named != nil {
			st, _ = named.Underlying().(*types.Struct)
		} else {
			st, _ = t.(*types.Struct)
		}
		if st == nil {
			return fieldKey{}, false
		}
		for i := 0; i < st.NumFields(); i++ {
			if st.Field(i) != fv {
				continue
			}
			if named == nil || named.Obj().Pkg() == nil {
				return fieldKey{}, false
			}
			return fieldKey{named.Obj().Pkg().Path(), named.Obj().Name(), fv.Name()}, true
		}
		for i := 0; i < st.NumFields(); i++ {
			if !st.Field(i).Embedded() {
				continue
			}
			if k, ok := find(st.Field(i).Type(), depth+1); ok {
				return k, true
			}
		}
		return fieldKey{}, false
	}
	return find(recv, 0)
}

func structOf(t types.Type) (*types.Struct, string, string) {
	if t == nil {
		return nil, "", ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil, "", ""
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, "", ""
	}
	return st, named.Obj().Name(), named.Obj().Pkg().Path()
}

func (s *scan) rel(pos token.Position) string {
	r, err := filepath.Rel(s.root, pos.Filename)
	if err != nil {
		r = pos.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(r), pos.Line)
}

func short(p string) string { return strings.TrimPrefix(p, "github.com/reliant-labs/forge/") }

func appendCapped(s []string, v string) []string {
	if len(s) >= 5 {
		return s
	}
	return append(s, v)
}
