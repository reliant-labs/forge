package codemod

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"

	"github.com/reliant-labs/forge/internal/goexec"
)

// connecterrors.go — `forge fix connect-errors`: the wire-identical half of
// moving a project's hand-built Connect errors onto pkg/svcerr.
//
// `connect.NewError(code, err)` publishes err.Error() to the caller, and
// svcerr's redaction cannot reach it: a *connect.Error is the handler's own
// verdict and svcerr.Interceptor passes it through as built. Most of a
// project's sites are harmless — a fixed message the handler wrote under an
// explicit code — and those convert mechanically. The rest wrap an error
// value under a guessed code, which is where the leaks and the wrong codes
// are, and converting them CHANGES what a client reads. So this codemod
// rewrites only the first kind, and lists the second.
//
// The contract is the one every codemod here keeps, at its strictest: for
// every site it rewrites, the client reads the same code and the same
// message as before. A site where no svcerr spelling says exactly the old
// thing is left alone and listed:
//
//   - the error is an error VALUE (`err`, `fmt.Errorf("…: %w", err)`, an
//     error-typed or .Error() argument) — svcerr.WrapOr is the fix, and it
//     changes the text, so it belongs in a reviewed change;
//   - the error is nil — the message was "", and every svcerr kind has text;
//   - the message is computed and could be empty (errors.New(msg), "%s") —
//     an empty detail makes a constructor return its bare sentinel, whose
//     text is not "";
//   - the code is not a constant (connect.NewError(code, …)).
//
// Where a rewritten value is read matters as much as what it says. A bare
// svcerr kind reaches the client with its code only through
// svcerr.Interceptor, which forge's chain builders install innermost on the
// HANDLER chain — so a bare kind is emitted only in a handler package
// (a path under handlers/), only where the expression's context type is
// exactly `error`, and only when no in-process reader of a *connect.Error
// (connect.CodeOf, errors.As into a *connect.Error, a type assertion), which
// would see a bare kind as something else, is in the package — its code or
// its tests (a test's connect.CodeOf aside, which is rewritten) — or in the
// production code of a package importing it. A reader further away is not
// traced, nor is an importer's test: one reads a client's error off the wire,
// which stays a *connect.Error, and one that does not fails loudly. Everywhere else
// — interceptors, clients, workers, a *connect.Error-typed return, a package
// that reads one — the result is svcerr.ToConnect(svcerr.<Kind>(…)): the same
// *connect.Error, built by svcerr, wherever it goes.
//
// Tests are rewritten in one way only: connect.CodeOf(x) → svcerr.Code(x),
// in the test files of every package that got a bare kind and of every
// package importing one. The two agree for a *connect.Error and for nil, and for a
// rewritten site svcerr.Code reads exactly the code connect.CodeOf read
// before; they differ only for an unclassified error (Unknown vs Internal),
// which a test then reports loudly. A connect.NewError inside a test is a
// fixture and stays.

const (
	connectPath = "connectrpc.com/connect"
	svcerrPath  = "github.com/reliant-labs/forge/pkg/svcerr"
)

func init() {
	register(Codemod{
		Name:    "connect-errors",
		Version: "v0.1.47",
		Short:   "Rewrite fixed-message connect.NewError calls to pkg/svcerr, wire-identically, and list the rest",
		Long: `Move hand-built Connect errors onto pkg/svcerr where that changes nothing a client reads.

connect.NewError(code, err) publishes err.Error() to the caller, and svcerr's
redaction cannot help: a *connect.Error passes svcerr.Interceptor as built. This
rewrites every connect.NewError whose client-visible code AND message an svcerr
spelling reproduces exactly:

  connect.NewError(connect.CodeNotFound, errors.New("chat not found"))  → svcerr.NotFound("chat")
  connect.NewError(connect.CodeNotFound, errors.New("no such chat"))    → svcerr.WithMessage(svcerr.ErrNotFound, "no such chat")
  connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
                                                                        → svcerr.InvalidArgument("id is required")
  connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("name %q is taken", n))
                                                                        → svcerr.AlreadyExists(fmt.Sprintf("name %q is taken", n))

A bare svcerr kind is written only in a handler package, where the expression is
an error and nothing reads a *connect.Error in process (connect.CodeOf, errors.As
into one) in the package, its tests, or the code of a package importing it;
elsewhere (interceptors, clients, a
*connect.Error-typed result, a package such a reader can see) the same value is
kept a *connect.Error with svcerr.ToConnect(…), and the report names the reader;
once that reader reads svcerr.Code, a re-run drops the wrappers.
In the test files of every package that got a bare kind, and of every package
importing one, connect.CodeOf(x) becomes svcerr.Code(x) — the code the
interceptor sends, which a handler test calling a *Service method directly needs
once the handler returns a bare kind.

It never changes a code or a message. A site it cannot rewrite exactly is listed
with the reason, file and line, and is the worklist for the change that does
change what clients read:

  wraps an error value     connect.NewError(code, err), fmt.Errorf("…: %w", err), an error-typed
                           or .Error() argument: publishes err.Error(). svcerr.WrapOr(err,
                           svcerr.<Kind>("<summary>")) redacts it and passes a classified err
                           through — which changes the code or text, so review it.
  nil error                the message is ""; every svcerr kind has text
  computed message         errors.New(msg) / a format with no literal text could be "", which no
                           svcerr constructor reproduces
  computed code            the code is not a connect.Code constant

Generated files, vendor/ and testdata/ are left alone, as is every
connect.NewError in a test (a fixture). Re-running it changes nothing.
--dry-run prints each rewrite as file:line: old → new, and the worklist.`,
		CleanTree: true,
		Run:       runConnectErrors,
	})
}

// connectErrorTokens pre-filter the files worth type-checking.
var connectErrorTokens = [][]byte{[]byte("NewError"), []byte("CodeOf"), []byte("connect.Error")}

func runConnectErrors(dir string, write bool) (Report, error) {
	candidates := map[string]bool{}
	err := ownedGoFiles(dir, func(rel string, src []byte) error {
		// A wrapper an earlier run left (svcerr.ToConnect) is in a file that
		// may no longer import connect at all.
		if bytes.Contains(src, []byte(svcerrPath)) && bytes.Contains(src, []byte("ToConnect(")) {
			candidates[filepath.Join(dir, filepath.FromSlash(rel))] = true
			return nil
		}
		if !bytes.Contains(src, []byte(connectPath)) {
			return nil
		}
		for _, tok := range connectErrorTokens {
			if bytes.Contains(src, tok) {
				candidates[filepath.Join(dir, filepath.FromSlash(rel))] = true
				break
			}
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	if goModulePath(dir) == "" || len(candidates) == 0 {
		return Report{}, nil
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return Report{}, err
	}
	// ownedGoFiles reported paths under dir; the loader reports absolute ones.
	abs := make(map[string]bool, len(candidates))
	for p := range candidates {
		a, err := filepath.Abs(p)
		if err != nil {
			return Report{}, err
		}
		abs[a] = true
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Dir:   absDir,
		Env:   goexec.Env(),
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return Report{}, fmt.Errorf("load the project's packages: %w", err)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].ID < pkgs[j].ID })

	ce := &connectErrorsRun{absDir: absDir, candidates: abs, out: map[string][]byte{}}
	if err := ce.run(pkgs); err != nil {
		return Report{}, err
	}
	if write {
		paths := make([]string, 0, len(ce.out))
		for path := range ce.out {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			if err := writeFile(path, ce.out[path]); err != nil {
				return Report{}, err
			}
		}
	}
	return ce.report(), nil
}

// ceFile is one candidate file and the package variant it was read from.
type ceFile struct {
	pkg  *packages.Package
	file *ast.File
	path string // absolute
	rel  string // slash-separated, relative to the project
	test bool
}

type connectErrorsRun struct {
	absDir     string
	candidates map[string]bool
	out        map[string][]byte

	rewrites []string
	leftHint []leftSite // sites left alone, with why
	// keptConnect: packages where every rewrite stays a *connect.Error
	// because the package reads one in process.
	keptConnect map[string]string
	touched     map[string]bool
	bareUsed    bool
}

// pkgPathOf is the import path a package variant belongs to: the test
// variant of p and p's external test package both count as p.
func pkgPathOf(p *packages.Package) string {
	return strings.TrimSuffix(p.PkgPath, "_test")
}

func (c *connectErrorsRun) run(pkgs []*packages.Package) error {
	files := c.collectFiles(pkgs)
	idx := newReaderIndex(files, pkgs)
	bare, err := c.rewriteSources(files, idx)
	if err != nil {
		return err
	}
	return c.rewriteTests(files, idx, bare)
}

// collectFiles lists each candidate file once: the test variant of a package
// re-lists its non-test files, and the first variant seen carries the same
// syntax.
func (c *connectErrorsRun) collectFiles(pkgs []*packages.Package) []ceFile {
	var files []ceFile
	seen := map[string]bool{}
	for _, p := range pkgs {
		if p.TypesInfo == nil || p.Types == nil {
			continue
		}
		for _, f := range p.Syntax {
			tf := p.Fset.File(f.Pos())
			if tf == nil {
				continue
			}
			path := tf.Name()
			if !c.candidates[path] || seen[path] {
				continue
			}
			seen[path] = true
			rel, err := filepath.Rel(c.absDir, path)
			if err != nil {
				rel = path
			}
			files = append(files, ceFile{pkg: p, file: f, path: path, rel: filepath.ToSlash(rel), test: strings.HasSuffix(path, "_test.go")})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files
}

// readerIndex knows which packages read a *connect.Error in process, and
// who imports whom.
//
// A test's connect.CodeOf is not a reader — rewriteTests turns it into
// svcerr.Code — but a test's errors.As into a *connect.Error, or a type
// assertion on one, is: it would fail on a bare kind, and it is not
// mechanical to change.
//
// A production reader blocks its own package and every package it imports
// (a value one call away reaches it). A test reader blocks only its own
// package: a handler package's tests call its functions directly, while a
// test in an importer — a composition root's route test — reads a client's
// error off the wire, which stays a *connect.Error.
type readerIndex struct {
	prod      map[string][]string        // package → production reader sites
	tests     map[string][]string        // package → reader sites in its own tests
	importers map[string]map[string]bool // package → the packages importing it
}

func newReaderIndex(files []ceFile, pkgs []*packages.Package) *readerIndex {
	idx := &readerIndex{prod: map[string][]string{}, tests: map[string][]string{}, importers: map[string]map[string]bool{}}
	for _, f := range files {
		into := idx.prod
		if f.test {
			into = idx.tests
		}
		pkg := pkgPathOf(f.pkg)
		into[pkg] = append(into[pkg], connectReaders(f, !f.test)...)
	}
	for _, m := range []map[string][]string{idx.prod, idx.tests} {
		for pkg := range m {
			sort.Strings(m[pkg])
		}
	}
	for _, p := range pkgs {
		for imp := range p.Imports {
			if idx.importers[imp] == nil {
				idx.importers[imp] = map[string]bool{}
			}
			idx.importers[imp][pkgPathOf(p)] = true
		}
	}
	return idx
}

// blocker names the reader that keeps pkg's rewrites a *connect.Error — one
// in the package or its tests, or in the code of a package importing it —
// or "" when a bare kind is safe. A reader further away is not traced.
func (idx *readerIndex) blocker(pkg string) string {
	if rs := idx.prod[pkg]; len(rs) > 0 {
		return rs[0]
	}
	if rs := idx.tests[pkg]; len(rs) > 0 {
		return rs[0]
	}
	var sites []string
	for q := range idx.importers[pkg] {
		if q != pkg && len(idx.prod[q]) > 0 {
			sites = append(sites, idx.prod[q][0])
		}
	}
	sort.Strings(sites)
	if len(sites) > 0 {
		return sites[0]
	}
	return ""
}

// rewriteSources rewrites every non-test file's connect.NewError calls, and
// returns the packages that got a bare svcerr kind.
func (c *connectErrorsRun) rewriteSources(files []ceFile, idx *readerIndex) (map[string]bool, error) {
	c.keptConnect = map[string]string{}
	c.touched = map[string]bool{}
	bare := map[string]bool{}
	for _, f := range files {
		if f.test {
			continue
		}
		pkg := pkgPathOf(f.pkg)
		blockedBy := ""
		if isHandlerFile(f.rel) {
			blockedBy = idx.blocker(pkg)
		}
		r, err := newCERewriter(f)
		if err != nil {
			return nil, err
		}
		r.allowBare = isHandlerFile(f.rel) && blockedBy == ""
		r.rewriteNewErrors()
		c.leftHint = append(c.leftHint, r.left...)
		if len(r.edits) == 0 {
			continue
		}
		if blockedBy != "" {
			c.keptConnect[pkg] = blockedBy
		}
		out, err := r.apply()
		if err != nil {
			return nil, err
		}
		c.out[f.path] = out
		c.rewrites = append(c.rewrites, r.done...)
		c.touched[pkg] = true
		if r.usedBare {
			c.bareUsed = true
			bare[pkg] = true
		}
	}
	return bare, nil
}

// rewriteTests rewrites connect.CodeOf to svcerr.Code in the tests of the
// packages that may now hand them a bare svcerr kind where a *connect.Error
// used to be: the ones that got one, and their direct importers. A package
// whose rewrites all stayed *connect.Error changed no type, and its tests
// are left alone.
func (c *connectErrorsRun) rewriteTests(files []ceFile, idx *readerIndex, bare map[string]bool) error {
	affected := map[string]bool{}
	for pkg := range bare {
		affected[pkg] = true
		for q := range idx.importers[pkg] {
			affected[q] = true
		}
	}
	for _, f := range files {
		if !f.test || !affected[pkgPathOf(f.pkg)] {
			continue
		}
		r, err := newCERewriter(f)
		if err != nil {
			return err
		}
		r.rewriteCodeOf()
		if len(r.edits) == 0 {
			continue
		}
		out, err := r.apply()
		if err != nil {
			return err
		}
		c.out[f.path] = out
		c.rewrites = append(c.rewrites, r.done...)
	}
	return nil
}

func (c *connectErrorsRun) report() Report {
	var rep Report
	rep.Rewrites = c.rewrites
	used := map[string]bool{}
	for _, l := range c.leftHint {
		rep.Next = append(rep.Next, "left as written: "+l.site+" — "+l.why)
		used[l.why] = true
	}
	for _, why := range leftReasons {
		if used[why] {
			rep.Next = append(rep.Next, why+": "+leftLegend[why])
		}
	}
	kept := make([]string, 0, len(c.keptConnect))
	for pkg, site := range c.keptConnect {
		kept = append(kept, fmt.Sprintf("%s: its rewrites stay *connect.Error (svcerr.ToConnect) because %s reads one in process — switch that reader to svcerr.Code(err) (and errors.As into a *connect.Error to svcerr.Code / the svcerr.Is* predicates), then re-run forge fix connect-errors: it drops the wrappers", pkg, site))
	}
	sort.Strings(kept)
	rep.Next = append(rep.Next, kept...)
	if rep.Changed() {
		if c.bareUsed {
			rep.Next = append(rep.Next,
				"A bare svcerr kind reaches the client with its code through svcerr.Interceptor, which forge's chain builders (serverkit.NewChain, observe.Chain, observe.DefaultMiddlewares) install innermost. A handler chain you assemble by hand must end with svcerr.Interceptor(), or the client reads unknown.",
				"In-process readers of a *connect.Error (connect.CodeOf, errors.As into one) in a handler package or a package importing it kept that package's rewrites *connect.Error; one further away is not traced. If handler errors travel further in process, check those readers: svcerr.Code(err) reads the same code for a *connect.Error and a bare kind.")
		}
		rep.Verify = []string{"go build ./...", "go vet ./...", "go test ./...", "forge lint --conventions"}
	}
	return rep
}

// isHandlerFile reports whether rel is under a handlers/ directory — the
// tree forge's handler chain (and so svcerr.Interceptor) serves, and the one
// forgeconv-handler-connect-error lints.
func isHandlerFile(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
		if part == "handlers" {
			return true
		}
	}
	return false
}

// ceRewriter rewrites one file.
type ceRewriter struct {
	f       ceFile
	info    *types.Info
	fset    *token.FileSet
	src     []byte
	parents map[ast.Node]ast.Node

	allowBare bool
	svcerrPfx string // "svcerr." or the file's alias
	addImport bool

	edits    []txEdit
	done     []string
	left     []leftSite
	usedBare bool
}

func newCERewriter(f ceFile) (*ceRewriter, error) {
	src, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	if tf := f.pkg.Fset.File(f.file.Pos()); tf == nil || tf.Size() != len(src) {
		return nil, fmt.Errorf("%s changed while it was being read; re-run", f.rel)
	}
	r := &ceRewriter{f: f, info: f.pkg.TypesInfo, fset: f.pkg.Fset, src: src, parents: map[ast.Node]ast.Node{}}
	var stack []ast.Node
	ast.Inspect(f.file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			r.parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	name := importName(f.file, svcerrPath, "svcerr")
	if name == "" {
		r.addImport = true
		name = "svcerr"
		if nameTaken(f.file, name) {
			name = "forgesvcerr"
		}
	}
	r.svcerrPfx = name + "."
	return r, nil
}

func (r *ceRewriter) off(pos token.Pos) int { return r.fset.Position(pos).Offset }

func (r *ceRewriter) text(n ast.Node) string { return string(r.src[r.off(n.Pos()):r.off(n.End())]) }

func (r *ceRewriter) site(n ast.Node) string {
	return fmt.Sprintf("%s:%d", r.f.rel, r.fset.Position(n.Pos()).Line)
}

// funcOf returns the package-level function a call's callee resolves to, as
// (package path, name), or ("", "") for anything else.
func funcOf(info *types.Info, call *ast.CallExpr) (string, string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return "", ""
	}
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		return "", ""
	}
	return fn.Pkg().Path(), fn.Name()
}

func (r *ceRewriter) rewriteNewErrors() {
	ast.Inspect(r.f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		pkg, name := funcOf(r.info, call)
		if pkg == svcerrPath && name == "ToConnect" && len(call.Args) == 1 {
			r.unwrapToConnect(call)
			return true
		}
		if pkg != connectPath || name != "NewError" || len(call.Args) != 2 {
			return true
		}
		r.newError(call)
		// A NewError nested in another's argument is a value of the outer
		// one; rewriting both would overlap, and the outer is left (its
		// argument is an error value), so the inner one is visited alone.
		return true
	})
}

// newError rewrites one connect.NewError call, or records why not.
func (r *ceRewriter) newError(call *ast.CallExpr) {
	// Whitespace collapsed but never truncated: the line is the diff a
	// reviewer reads in --dry-run, and the worklist a lane starts from.
	old := strings.Join(strings.Fields(r.text(call)), " ")
	kind, ok := r.codeKind(call.Args[0])
	if !ok {
		r.left = append(r.left, leftSite{fmt.Sprintf("%s: %s", r.site(call), old), leftComputedCode})
		return
	}
	msg, why := r.message(call.Args[1])
	if msg == nil {
		r.left = append(r.left, leftSite{fmt.Sprintf("%s: %s", r.site(call), old), why})
		return
	}
	expr := r.kindExpr(kind, *msg)
	if r.allowBare && r.contextIsError(call) {
		r.usedBare = true
	} else {
		expr = r.svcerrPfx + "ToConnect(" + expr + ")"
	}
	r.edits = append(r.edits, txEdit{start: r.off(call.Pos()), end: r.off(call.End()), text: expr})
	r.done = append(r.done, fmt.Sprintf("%s: %s → %s", r.site(call), old, expr))
}

// unwrapToConnect drops a svcerr.ToConnect around an svcerr call where a
// bare kind is now allowed: the shape an earlier run left in a package a
// reader kept on *connect.Error, once that reader reads svcerr.Code. Through
// svcerr.Interceptor the two are the same error on the wire.
func (r *ceRewriter) unwrapToConnect(call *ast.CallExpr) {
	if !r.allowBare || !r.contextIsError(call) {
		return
	}
	inner, ok := ast.Unparen(call.Args[0]).(*ast.CallExpr)
	if !ok {
		return
	}
	if pkg, _ := funcOf(r.info, inner); pkg != svcerrPath {
		return
	}
	text := r.text(inner)
	r.usedBare = true
	r.edits = append(r.edits, txEdit{start: r.off(call.Pos()), end: r.off(call.End()), text: text})
	r.done = append(r.done, fmt.Sprintf("%s: %s → %s", r.site(call), strings.Join(strings.Fields(r.text(call)), " "), strings.Join(strings.Fields(text), " ")))
}

// codeKind maps a constant connect.Code<X> to svcerr's <X>.
func (r *ceRewriter) codeKind(e ast.Expr) (string, bool) {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		id = x.Sel
	case *ast.Ident:
		id = x
	default:
		return "", false
	}
	c, ok := r.info.Uses[id].(*types.Const)
	if !ok || c.Pkg() == nil || c.Pkg().Path() != connectPath || !strings.HasPrefix(c.Name(), "Code") {
		return "", false
	}
	kind := strings.TrimPrefix(c.Name(), "Code")
	if !svcerrKinds[kind] {
		return "", false
	}
	return kind, true
}

// svcerrKinds are the connect codes with an svcerr constructor of the same
// name — every code connect defines.
var svcerrKinds = map[string]bool{
	"Canceled": true, "Unknown": true, "InvalidArgument": true, "DeadlineExceeded": true,
	"NotFound": true, "AlreadyExists": true, "PermissionDenied": true, "ResourceExhausted": true,
	"FailedPrecondition": true, "Aborted": true, "OutOfRange": true, "Unimplemented": true,
	"Internal": true, "Unavailable": true, "DataLoss": true, "Unauthenticated": true,
}

// ceMessage is the message a rewritten site passes to its constructor.
type ceMessage struct {
	expr string // Go source of a string expression, never empty at run time
	// lit is the constant value when the message is a constant, for the
	// NotFound sentence; valid when isConst.
	lit     string
	isConst bool
	// fromLit: expr is a string literal the source wrote, so a NotFound
	// may be respelled from its value without losing a named constant.
	fromLit bool
}

// message reduces connect.NewError's error argument to the string the
// client reads, when that string can be passed to an svcerr constructor
// unchanged and is provably non-empty. Otherwise it says why not.
func (r *ceRewriter) message(e ast.Expr) (*ceMessage, string) {
	e = ast.Unparen(e)
	if id, ok := e.(*ast.Ident); ok && id.Name == "nil" {
		if _, isNil := r.info.Uses[id].(*types.Nil); isNil {
			return nil, leftNil
		}
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return nil, r.errorValueHint()
	}
	switch pkg, name := funcOf(r.info, call); {
	case pkg == "errors" && name == "New" && len(call.Args) == 1:
		arg := ast.Unparen(call.Args[0])
		if r.carriesError(arg) {
			return nil, r.errorValueHint()
		}
		if v, ok := r.constString(arg); ok {
			if v == "" {
				return nil, leftEmptyMessage
			}
			_, isLit := arg.(*ast.BasicLit)
			return &ceMessage{expr: r.text(arg), lit: v, isConst: true, fromLit: isLit}, ""
		}
		if inner, ok := arg.(*ast.CallExpr); ok {
			if p, n := funcOf(r.info, inner); p == "fmt" && n == "Sprintf" {
				if why := r.formatOK(inner); why != "" {
					return nil, why
				}
				return &ceMessage{expr: r.text(inner)}, ""
			}
		}
		return nil, leftComputedMessage
	case pkg == "fmt" && name == "Errorf" && len(call.Args) >= 1:
		if why := r.formatOK(call); why != "" {
			return nil, why
		}
		if v, _ := r.constString(call.Args[0]); len(call.Args) == 1 {
			arg := ast.Unparen(call.Args[0])
			_, isLit := arg.(*ast.BasicLit)
			if !strings.Contains(v, "%") {
				// No verbs and no arguments: Errorf renders the format as is.
				return &ceMessage{expr: r.text(arg), lit: v, isConst: true, fromLit: isLit}, ""
			}
			if text, verbs, _ := formatText(v); verbs == 0 {
				// Only %% escapes: the rendered text is a constant.
				return &ceMessage{expr: strconv.Quote(text), lit: text, isConst: true, fromLit: true}, ""
			}
		}
		// fmt.Errorf(f, args…) without %w renders exactly fmt.Sprintf(f, args…).
		fmtName := strings.TrimSuffix(r.text(call.Fun), "Errorf")
		return &ceMessage{expr: fmtName + "Sprintf" + string(r.src[r.off(call.Fun.End()):r.off(call.End())])}, ""
	}
	return nil, r.errorValueHint()
}

func (r *ceRewriter) errorValueHint() string { return leftErrorValue }

// leftSite is one connect.NewError the codemod did not rewrite.
type leftSite struct {
	site string // file:line: the call
	why  string // one of leftReasons
}

// Why a site is left as written. Each worklist line carries one; the report
// explains each that occurs once, after the list.
const (
	leftErrorValue      = "wraps an error value"
	leftNil             = "nil error"
	leftComputedMessage = "computed message"
	leftEmptyMessage    = "empty message"
	leftComputedCode    = "computed code"
)

var leftReasons = []string{leftErrorValue, leftNil, leftComputedMessage, leftEmptyMessage, leftComputedCode}

var leftLegend = map[string]string{
	leftErrorValue: "the client reads the error's Error() text, unredacted. svcerr.WrapOr(err, svcerr.<Kind>(\"<summary>\")) redacts it and lets a classified err (a store's NotFound, a deadline) keep its code — which changes what the client reads, so it is a reviewed change. " +
		"Text the user should read goes in the constructor's argument (svcerr.FailedPrecondition(daemonMsg)); a remote Connect client's error is wrapped with svcerr.WithCause, never WrapOr.",
	leftNil:             "the client reads an empty message, and every svcerr kind has text: return the sentinel svcerr.Err<Kind> (its message is the category) or a constructor with real text.",
	leftComputedMessage: "the message is computed and could be empty, and an empty detail makes a constructor return its bare sentinel, whose text is not empty: pass the value to svcerr.<Kind>(…) if it never is.",
	leftEmptyMessage:    "no svcerr constructor publishes an empty message: give it one.",
	leftComputedCode:    "the code is not a connect.Code constant: name the kind at the site, or move the mapping to the producer (its sentinels become svcerr kinds) and svcerr.Wrap(err).",
}

// formatOK checks an Errorf / Sprintf call: a constant format with no %w,
// literal text outside its verbs (so the result is never ""), and no
// argument that is an error or calls Error() — those publish an error's text
// and belong on the worklist, not frozen into a constructor.
func (r *ceRewriter) formatOK(call *ast.CallExpr) string {
	f, ok := r.constString(call.Args[0])
	if !ok {
		return leftComputedMessage
	}
	lit, _, hasW := formatText(f)
	if hasW {
		return r.errorValueHint()
	}
	if lit == "" {
		return leftComputedMessage // no literal text: the result could be ""
	}
	for _, a := range call.Args[1:] {
		if r.carriesError(a) {
			return r.errorValueHint()
		}
	}
	return ""
}

var errorIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// carriesError reports whether an argument is an error, or contains a call
// to an error's Error method.
func (r *ceRewriter) carriesError(e ast.Expr) bool {
	if t := r.info.TypeOf(e); t != nil && types.Implements(t, errorIface) {
		return true
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Error" {
			return true
		}
		if t := r.info.TypeOf(sel.X); t != nil && types.Implements(t, errorIface) {
			found = true
		}
		return true
	})
	return found
}

func (r *ceRewriter) constString(e ast.Expr) (string, bool) {
	tv, ok := r.info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// formatText returns the text a printf format prints on its own —
// everything but its verbs, with %% as % — how many verbs it has, and
// whether one is %w.
func formatText(f string) (lit string, verbs int, hasW bool) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			b.WriteByte(f[i])
			continue
		}
		i++
		if i < len(f) && f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		// flags, [n], width, .precision, [n]
		for i < len(f) && strings.IndexByte("+-# 0", f[i]) >= 0 {
			i++
		}
		skipArgIndex := func() {
			if i < len(f) && f[i] == '[' {
				for i < len(f) && f[i] != ']' {
					i++
				}
				i++
			}
		}
		skipNum := func() {
			if i < len(f) && f[i] == '*' {
				i++
				return
			}
			for i < len(f) && f[i] >= '0' && f[i] <= '9' {
				i++
			}
		}
		skipArgIndex()
		skipNum()
		if i < len(f) && f[i] == '.' {
			i++
			skipArgIndex()
			skipNum()
		}
		skipArgIndex()
		// The loop's i++ steps over the verb; a format that ends mid-verb
		// still counts one (fmt prints %!(NOVERB) for it).
		verbs++
		if i < len(f) && f[i] == 'w' {
			hasW = true
		}
	}
	return b.String(), verbs, hasW
}

// kindExpr spells svcerr's constructor for kind with msg as its message.
func (r *ceRewriter) kindExpr(kind string, msg ceMessage) string {
	if kind != "NotFound" {
		return r.svcerrPfx + kind + "(" + msg.expr + ")"
	}
	// NotFound composes "<entity> not found" itself.
	if msg.isConst && msg.fromLit {
		if entity, ok := strings.CutSuffix(msg.lit, " not found"); ok && entity != "" {
			return r.svcerrPfx + "NotFound(" + strconv.Quote(entity) + ")"
		}
	}
	return r.svcerrPfx + "WithMessage(" + r.svcerrPfx + "ErrNotFound, " + msg.expr + ")"
}

// contextIsError reports whether the expression's context requires exactly
// the error interface — the one place a bare svcerr kind can replace a
// *connect.Error without changing a type.
func (r *ceRewriter) contextIsError(e ast.Expr) bool {
	t := r.contextType(e)
	return t != nil && types.Identical(t, types.Universe.Lookup("error").Type())
}

// contextType is the type the expression e is converted to where it is
// used, or nil when that is not a simple slot (a := declaration, a receiver,
// a composite literal, …).
func (r *ceRewriter) contextType(e ast.Expr) types.Type {
	var child ast.Node = e
	parent := r.parents[e]
	for {
		if p, ok := parent.(*ast.ParenExpr); ok {
			child, parent = p, r.parents[p]
			continue
		}
		break
	}
	switch p := parent.(type) {
	case *ast.ReturnStmt:
		sig := r.enclosingSig(p)
		if sig == nil || sig.Results().Len() != len(p.Results) {
			return nil
		}
		for i, res := range p.Results {
			if res == child {
				return sig.Results().At(i).Type()
			}
		}
	case *ast.AssignStmt:
		if p.Tok != token.ASSIGN || len(p.Lhs) != len(p.Rhs) {
			return nil
		}
		for i, rhs := range p.Rhs {
			if rhs == child {
				return r.info.TypeOf(p.Lhs[i])
			}
		}
	case *ast.ValueSpec:
		if p.Type == nil {
			return nil
		}
		return r.info.TypeOf(p.Type)
	case *ast.CallExpr:
		sig, ok := r.info.TypeOf(p.Fun).(*types.Signature)
		if !ok {
			return nil
		}
		for i, a := range p.Args {
			if a != child {
				continue
			}
			params := sig.Params()
			if sig.Variadic() && i >= params.Len()-1 {
				if p.Ellipsis.IsValid() {
					return nil
				}
				if s, ok := params.At(params.Len() - 1).Type().(*types.Slice); ok {
					return s.Elem()
				}
				return nil
			}
			if i < params.Len() {
				return params.At(i).Type()
			}
		}
	}
	return nil
}

// enclosingSig is the signature of the function (declaration or literal)
// whose body holds n.
func (r *ceRewriter) enclosingSig(n ast.Node) *types.Signature {
	for p := r.parents[n]; p != nil; p = r.parents[p] {
		switch fn := p.(type) {
		case *ast.FuncLit:
			sig, _ := r.info.TypeOf(fn).(*types.Signature)
			return sig
		case *ast.FuncDecl:
			if obj, ok := r.info.Defs[fn.Name].(*types.Func); ok {
				sig, _ := obj.Type().(*types.Signature)
				return sig
			}
			return nil
		}
	}
	return nil
}

// rewriteCodeOf rewrites a test file's connect.CodeOf(x) to svcerr.Code(x).
func (r *ceRewriter) rewriteCodeOf() {
	ast.Inspect(r.f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if pkg, name := funcOf(r.info, call); pkg != connectPath || name != "CodeOf" {
			return true
		}
		r.edits = append(r.edits, txEdit{start: r.off(call.Fun.Pos()), end: r.off(call.Fun.End()), text: r.svcerrPfx + "Code"})
		r.done = append(r.done, fmt.Sprintf("%s: %s → %sCode", r.site(call), r.text(call.Fun), r.svcerrPfx))
		return true
	})
}

// connectReaders lists a file's in-process readers of a *connect.Error:
// connect.CodeOf (when withCodeOf), errors.As into one, and type assertions
// or switches on one. Each would read a bare svcerr kind differently.
func connectReaders(f ceFile, withCodeOf bool) []string {
	info, fset := f.pkg.TypesInfo, f.pkg.Fset
	isConnErr := func(t types.Type) bool {
		for t != nil {
			ptr, ok := t.(*types.Pointer)
			if !ok {
				break
			}
			if named, ok := ptr.Elem().(*types.Named); ok && named.Obj().Pkg() != nil &&
				named.Obj().Pkg().Path() == connectPath && named.Obj().Name() == "Error" {
				return true
			}
			t = ptr.Elem()
		}
		return false
	}
	var sites []string
	add := func(n ast.Node, what string) {
		sites = append(sites, fmt.Sprintf("%s:%d (%s)", f.rel, fset.Position(n.Pos()).Line, what))
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			switch pkg, name := funcOf(info, x); {
			case pkg == connectPath && name == "CodeOf" && withCodeOf:
				add(x, "connect.CodeOf")
			case pkg == "errors" && name == "As" && len(x.Args) == 2 && isConnErr(info.TypeOf(x.Args[1])):
				add(x, "errors.As into a *connect.Error")
			}
		case *ast.TypeAssertExpr:
			if x.Type != nil && isConnErr(info.TypeOf(x.Type)) {
				add(x, "type assertion to *connect.Error")
			}
		case *ast.TypeSwitchStmt:
			for _, cl := range x.Body.List {
				for _, t := range cl.(*ast.CaseClause).List {
					if isConnErr(info.TypeOf(t)) {
						add(t, "type switch on *connect.Error")
					}
				}
			}
		}
		return true
	})
	return sites
}

// apply makes the edits and fixes the imports.
func (r *ceRewriter) apply() ([]byte, error) {
	sort.Slice(r.edits, func(i, j int) bool { return r.edits[i].start > r.edits[j].start })
	out := append([]byte(nil), r.src...)
	for _, e := range r.edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, r.f.rel, out, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("re-parse %s after the rewrite: %w", r.f.rel, err)
	}
	if r.addImport {
		name := strings.TrimSuffix(r.svcerrPfx, ".")
		if name == "svcerr" {
			name = ""
		}
		astutil.AddNamedImport(fset, file, name, svcerrPath)
	}
	// The rewrite removes uses of errors.New, fmt.Errorf and connect; drop
	// an import it left unused.
	for path, def := range map[string]string{"errors": "errors", "fmt": "fmt", connectPath: "connect"} {
		name := importName(file, path, def)
		if name == "" || len(packageRefs(file, name, nil)) > 0 {
			continue
		}
		alias := ""
		if name != def {
			alias = name
		}
		astutil.DeleteNamedImport(fset, file, alias, path)
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, fmt.Errorf("format %s: %w", r.f.rel, err)
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", r.f.rel, err)
	}
	return formatted, nil
}
