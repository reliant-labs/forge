// File: internal/cli/lint_optional_deps_guard.go
//
// optional-deps-guard — `forge lint --optional-deps-guard`.
//
// A Deps struct field carrying the `// forge:optional-dep` directive is
// a contract: "this field may be nil at runtime; every use must be
// nil-checked at the call site". The directive exists precisely because
// validateDeps does NOT gate optional fields and wire_gen emits the
// typed zero silently when no producer matches (see DepsField.Optional
// in internal/codegen/deps_parser.go). That makes an unguarded
// `s.deps.X.Method(...)` a latent nil-pointer panic that no
// construction-time gate will ever catch — the canonical failure shape
// reported from cp-forge (FRICTION #23/#33/#69, billing handlers) and
// the kalshi optional-many workers.
//
// What counts as a deref
//
// Only selector / call / index / slice / star expressions ROOTED at the
// optional field dereference it:
//
//	s.deps.X.Method(...)   // method call           → deref
//	d.X.Y                  // field access           → deref
//	s.deps.X(...)          // func-typed field call  → deref
//	s.deps.X[i]            // index                  → deref
//	*s.deps.X              // explicit pointer deref → deref
//
// Passing the field around does NOT deref it and is never flagged:
//
//	helper(s.deps.X)       // argument
//	y := s.deps.X          // assignment (tracked as alias, see below)
//	return s.deps.X        // accessor
//	Deps{X: w.deps.X}      // composite literal
//	s.deps.X == nil        // comparison
//
// Guard patterns recognized
//
// The walker is a single-function, forward-flow scan — deliberately NOT
// full dataflow. A deref of optional field X is considered guarded when
// one of these dominates it inside the same function body:
//
//  1. Early-return guard: `if s.deps.X == nil { return ... }` (or any
//     body whose final statement always exits: return / panic /
//     continue / break / goto / os.Exit / log.Fatal*). Everything after
//     the if in the same block is guarded. `== nil` terms are honored
//     at the top level of an `||` chain — `if s.deps.X == nil || err
//     != nil { return }` still guards X (short-circuit: X nil forces
//     the exit).
//
//  2. Enclosing guard: `if s.deps.X != nil { ...derefs here... }`.
//     `!= nil` terms are honored at the top level of an `&&` chain.
//     The else-branch of an `== nil` check is symmetric (`if s.deps.X
//     == nil { ... } else { ...guarded... }`), as are later clauses of
//     a tagless switch whose earlier clause was `case s.deps.X == nil:`.
//
//  3. Alias-then-check: `x := s.deps.X` followed by either guard shape
//     on `x`. Simple positional `:=`/`=`/`var` aliases are tracked;
//     re-assigning the alias to anything else drops the tracking
//     (conservative both ways: the alias stops counting as the field
//     for derefs AND for guards).
//
// Conservatism trade-offs
//
// Anything the walker cannot prove guarded IS flagged — but every
// finding is severity "warning" and the lint never gates the build.
// Known sources of false positives, all intentional:
//
//   - guards hidden behind helper methods (`if err := s.requireX();
//     err != nil { return err }`),
//   - invariants established in Start()/Setup() before the deref runs,
//   - guards in the caller rather than the deref's own function.
//
// The escape hatch for provably-safe sites is the suppression
// directive: a `// forge:optional-checked` comment on the deref line
// (or the line directly above) silences the finding. Every finding's
// fix_hint documents it. Cross-function/inter-procedural analysis is
// explicitly out of scope — when the walker is unsure, it warns rather
// than stays silent, because the failure mode it protects against is a
// production nil-panic.
//
// Where it scans
//
// Same role roots as bootstrap-deps-coverage: internal/<pkg>/,
// handlers/<svc>/, workers/<w>/, operators/<o>/. A package
// participates only when its Deps struct (per codegen.ParseServiceDeps)
// has at least one optional field. All non-test, non-_gen .go files in
// the package dir are walked — generated files (mock_gen.go,
// handlers_crud_gen.go, …) are forge-owned and correct by
// construction; flagging them would send users editing files that
// regenerate.
//
// Why this lives in cli/ rather than internal/linter/forgeconv/:
// forgeconv is for proto-aware analyzers; this rule is a Deps-shape
// companion to lint_bootstrap_deps_coverage.go and shares its
// collect/format split so `forge lint --json` and `forge audit --json`
// reuse the same engine.

package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// optionalCheckedDirective is the per-line suppression marker. Same
// recognition discipline as `forge:optional-dep`: the directive must be
// the entire comment content (both `// forge:optional-checked` and the
// no-space `//forge:optional-checked` directive form are accepted), so
// prose that merely mentions the marker doesn't suppress anything.
const optionalCheckedDirective = "forge:optional-checked"

// optionalDepsGuardFinding is one unguarded deref of an optional-dep
// field. File is projectDir-relative; Line/Col are 1-based positions of
// the deref's root expression (the optional field selector itself, not
// the trailing method name) so editors jump to the foot of the chain.
type optionalDepsGuardFinding struct {
	File    string
	Line    int
	Col     int
	Role    string // "internal" | "handlers" | "workers" | "operators"
	Package string // directory / package name, e.g. "billing"
	Field   string // the optional Deps field, e.g. "SvcBillingHandler"
	Method  string // enclosing func/method name (the RPC for handlers)
	Expr    string // source text of the dereferenced expression, e.g. "s.deps.SvcBillingHandler"
}

// optionalDepsGuardFixHint renders the canonical remediation for a
// finding — shown in text mode and carried as fix_hint in JSON.
func optionalDepsGuardFixHint(f optionalDepsGuardFinding) string {
	return fmt.Sprintf(
		"add an early return `if %s == nil { return ... }` at the top of %s, or wrap the use in `if %s != nil { ... }`; if this site is provably safe (invariant established elsewhere), append `// forge:optional-checked` on the deref line to suppress",
		f.Expr, f.Method, f.Expr)
}

// runOptionalDepsGuardLint is the text-mode entry point. Warnings only
// — the walker is intentionally not full dataflow (see file header), so
// findings nudge rather than gate.
func runOptionalDepsGuardLint(projectDir string) error {
	fmt.Println("Running optional-deps-guard lint...")
	findings, err := collectOptionalDepsGuardFindings(projectDir)
	if err != nil {
		return err
	}
	formatOptionalDepsGuard(os.Stdout, findings)
	// Warnings only — never gate. The suppression directive exists for
	// confirmed-safe sites; everything else deserves a human look, not
	// a broken build.
	return nil
}

// formatOptionalDepsGuard writes the human report. Empty findings print
// a single success line, matching the sibling coverage lints.
func formatOptionalDepsGuard(w io.Writer, findings []optionalDepsGuardFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  optional-deps-guard clean — every optional-dep deref is nil-guarded (or suppressed)")
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ⚠ [forge-optional-deps-guard] %s:%d:%d\n", f.File, f.Line, f.Col)
		_, _ = fmt.Fprintf(w, "      %s dereferences optional dep Deps.%s (marked `// forge:optional-dep` — may be nil) without a dominating nil-guard in %s\n", f.Expr, f.Field, f.Method)
		_, _ = fmt.Fprintf(w, "      → %s\n", optionalDepsGuardFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d unguarded optional-dep deref(s).\n", len(findings))
	_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
}

// collectOptionalDepsGuardFindings is the shared engine behind text
// mode, `forge lint --json`, and `forge audit --json`. Findings come
// back sorted by (file, line, col) so output is deterministic.
func collectOptionalDepsGuardFindings(projectDir string) ([]optionalDepsGuardFinding, error) {
	var findings []optionalDepsGuardFinding

	// Same role roots as bootstrap-deps-coverage: every tree that hosts
	// the conventional <pkg>/Deps shape. Missing roots are fine — many
	// projects ship no operators/ or workers/.
	roleRoots := []string{"internal", "handlers", "workers", "operators"}
	for _, role := range roleRoots {
		rootDir := filepath.Join(projectDir, role)
		entries, err := os.ReadDir(rootDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", rootDir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pkgDir := filepath.Join(rootDir, e.Name())
			deps, depsErr := codegen.ParseServiceDeps(pkgDir)
			if depsErr != nil || len(deps) == 0 {
				// Unparseable / Deps-less packages have nothing optional
				// to guard; the project's own build reports parse errors.
				continue
			}
			optional := map[string]bool{}
			for _, d := range deps {
				if d.Optional {
					optional[d.Name] = true
				}
			}
			if len(optional) == 0 {
				continue
			}
			pkgFindings, scanErr := scanPackageForUnguardedDerefs(projectDir, pkgDir, role, e.Name(), optional)
			if scanErr != nil {
				return nil, scanErr
			}
			findings = append(findings, pkgFindings...)
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Col < findings[j].Col
	})
	return findings, nil
}

// scanPackageForUnguardedDerefs parses every non-test, non-generated
// .go file in pkgDir and runs the guard walker over each function body.
func scanPackageForUnguardedDerefs(projectDir, pkgDir, role, pkgName string, optional map[string]bool) ([]optionalDepsGuardFinding, error) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pkgDir, err)
	}

	var findings []optionalDepsGuardFinding
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Tests construct zero-value Deps on purpose; generated files
		// (_gen.go) are forge-owned and regenerate — flagging either
		// would be noise the user can't or shouldn't act on.
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		fp := filepath.Join(pkgDir, name)
		file, parseErr := parser.ParseFile(fset, fp, nil, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			// Don't double-report parse errors — the Go toolchain will.
			continue
		}

		rel, relErr := filepath.Rel(projectDir, fp)
		if relErr != nil {
			rel = fp
		}

		// Suppression lines: any comment whose whole content is the
		// `forge:optional-checked` directive suppresses findings on its
		// own line (trailing comment) or the line directly below (the
		// comment-above form).
		suppressed := map[int]bool{}
		for _, cg := range file.Comments {
			for _, c := range cg.List {
				if commentDirectiveText(c.Text) == optionalCheckedDirective {
					suppressed[fset.Position(c.Pos()).Line] = true
				}
			}
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			w := &optionalGuardWalker{
				fset:       fset,
				file:       rel,
				role:       role,
				pkg:        pkgName,
				optional:   optional,
				suppressed: suppressed,
				funcName:   fn.Name.Name,
				depsIdents: map[string]bool{},
				aliases:    map[string]string{},
				findings:   &findings,
			}
			// Receiver / params typed Deps (or *Deps) root the bare
			// `d.X` form — methods on Deps itself (validateDeps-style
			// helpers) and constructors like `func New(deps Deps)`.
			if fn.Recv != nil {
				for _, f := range fn.Recv.List {
					if isDepsType(f.Type) {
						for _, n := range f.Names {
							w.depsIdents[n.Name] = true
						}
					}
				}
			}
			if fn.Type.Params != nil {
				for _, f := range fn.Type.Params.List {
					if isDepsType(f.Type) {
						for _, n := range f.Names {
							w.depsIdents[n.Name] = true
						}
					}
				}
			}
			w.walkStmts(fn.Body.List, map[string]bool{})
		}
	}
	return findings, nil
}

// commentDirectiveText strips comment-syntax wrappers from a raw
// ast.Comment.Text value — same shape as codegen.trimCommentMarkers,
// duplicated here because that helper is unexported and the directive
// recognition must stay byte-identical regardless.
func commentDirectiveText(raw string) string {
	switch {
	case strings.HasPrefix(raw, "//"):
		return strings.TrimSpace(strings.TrimPrefix(raw, "//"))
	case strings.HasPrefix(raw, "/*"):
		inner := strings.TrimPrefix(raw, "/*")
		inner = strings.TrimSuffix(inner, "*/")
		return strings.TrimSpace(inner)
	default:
		return strings.TrimSpace(raw)
	}
}

// isDepsType reports whether a field type expression is `Deps` or
// `*Deps` — the package-local Deps struct per the strict-contract-names
// convention. Selector forms (otherpkg.Deps) are deliberately NOT
// matched: a foreign Deps has its own optional set we know nothing
// about.
func isDepsType(t ast.Expr) bool {
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == "Deps"
}

// optionalGuardWalker carries the per-function analysis state. Guard
// sets are passed down the statement walk by value-copy at every branch
// point so sibling branches can't see each other's guards; the ONLY
// mutation visible to a parent block is the early-return accrual in
// walkStmts (guard established for the remainder of the same block).
type optionalGuardWalker struct {
	fset       *token.FileSet
	file       string
	role       string
	pkg        string
	optional   map[string]bool
	suppressed map[int]bool
	funcName   string

	// depsIdents are identifiers that ARE a Deps value (receiver or
	// param typed Deps / *Deps) — they root the bare `d.X` form.
	depsIdents map[string]bool

	// aliases maps a local identifier to the optional field it was
	// assigned from (`x := s.deps.X` → aliases["x"] = "X"). Guards and
	// derefs on the alias count as guards/derefs of the field.
	// Tracking is per-function and order-sensitive: a re-assignment
	// drops the alias.
	aliases map[string]string

	findings *[]optionalDepsGuardFinding
}

// fieldKey resolves an expression to the optional field it denotes, or
// ("", false). Recognized roots:
//
//	x              alias previously assigned from the field
//	d.X            d is a Deps-typed receiver/param ident
//	s.deps.X       any ident . deps/Deps . field — the conventional
//	               implementation shape (s *Service holding `deps Deps`)
func (w *optionalGuardWalker) fieldKey(e ast.Expr) (string, bool) {
	e = unparen(e)
	switch v := e.(type) {
	case *ast.Ident:
		if k, ok := w.aliases[v.Name]; ok {
			return k, true
		}
	case *ast.SelectorExpr:
		if !w.optional[v.Sel.Name] {
			return "", false
		}
		switch x := unparen(v.X).(type) {
		case *ast.Ident:
			if w.depsIdents[x.Name] {
				return v.Sel.Name, true
			}
		case *ast.SelectorExpr:
			// Middle hop must literally be the deps-holder field. Any
			// base ident is accepted (receiver, closure capture, helper
			// param) — requiring exactly `.deps` / `.Deps` keeps the
			// false-positive surface to "unrelated struct with a field
			// named deps that itself has a same-named field", which is
			// vanishingly rare and warning-severity anyway.
			if (x.Sel.Name == "deps" || x.Sel.Name == "Deps") && isIdent(x.X) {
				return v.Sel.Name, true
			}
		}
	}
	return "", false
}

func isIdent(e ast.Expr) bool {
	_, ok := unparen(e).(*ast.Ident)
	return ok
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// report records a finding for the deref of field key at expression
// root, unless the site is suppressed.
func (w *optionalGuardWalker) report(root ast.Expr, key string) {
	pos := w.fset.Position(root.Pos())
	if w.suppressed[pos.Line] || w.suppressed[pos.Line-1] {
		return
	}
	*w.findings = append(*w.findings, optionalDepsGuardFinding{
		File:    w.file,
		Line:    pos.Line,
		Col:     pos.Column,
		Role:    w.role,
		Package: w.pkg,
		Field:   key,
		Method:  w.funcName,
		Expr:    exprString(root),
	})
}

// exprString renders the small selector chains we report (s.deps.X,
// d.X, alias idents). Hand-rolled instead of go/printer because the
// shapes are tiny and we don't want printer's fset plumbing here.
func exprString(e ast.Expr) string {
	switch v := unparen(e).(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return "<expr>"
	}
}

// ---------------------------------------------------------------------------
// Statement walk — forward flow with guard accrual.
// ---------------------------------------------------------------------------

// walkStmts processes a block's statements in order. guarded is OWNED
// by this call (callers pass a copy when branching); early-return
// guards mutate it so the remainder of the block sees the field as
// safe.
func (w *optionalGuardWalker) walkStmts(stmts []ast.Stmt, guarded map[string]bool) {
	for _, s := range stmts {
		w.walkStmt(s, guarded)
	}
}

func (w *optionalGuardWalker) walkStmt(s ast.Stmt, guarded map[string]bool) {
	switch v := s.(type) {
	case *ast.IfStmt:
		if v.Init != nil {
			w.walkStmt(v.Init, guarded)
		}
		// The condition itself can deref (`if s.deps.X.Enabled()`) —
		// scan it BEFORE applying the guards the condition establishes.
		w.scanExpr(v.Cond, guarded)
		neq := w.nilCheckKeys(v.Cond, true) // top-level && terms `X != nil`
		eq := w.nilCheckKeys(v.Cond, false) // top-level || terms `X == nil`
		w.walkStmts(v.Body.List, copyGuards(guarded, neq))
		if v.Else != nil {
			// All `== nil` terms false on the else path → fields non-nil.
			elseGuards := copyGuards(guarded, eq)
			switch e := v.Else.(type) {
			case *ast.BlockStmt:
				w.walkStmts(e.List, elseGuards)
			default: // else-if chain
				w.walkStmt(e, elseGuards)
			}
		}
		// Early-return accrual: when the then-branch always exits, an
		// `X == nil` condition guarantees X != nil for the remainder of
		// the CURRENT block. Mutating guarded (owned by our caller's
		// walkStmts loop) is exactly that scope.
		if blockAlwaysExits(v.Body) {
			for k := range eq {
				guarded[k] = true
			}
		}

	case *ast.AssignStmt:
		for _, rhs := range v.Rhs {
			w.scanExpr(rhs, guarded)
		}
		for _, lhs := range v.Lhs {
			// LHS derefs count too: `s.deps.X.Field = v` derefs X.
			// Plain `s.deps.X = v` does not (the field itself is the
			// target) — scanExpr's parent-node rule handles both.
			w.scanExpr(lhs, guarded)
		}
		w.recordAliases(v.Lhs, v.Rhs)

	case *ast.DeclStmt:
		if gd, ok := v.Decl.(*ast.GenDecl); ok {
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, val := range vs.Values {
					w.scanExpr(val, guarded)
				}
				// `var x = s.deps.X` aliases like := does.
				if len(vs.Names) == len(vs.Values) {
					for i, n := range vs.Names {
						if k, ok := w.fieldKey(vs.Values[i]); ok {
							w.aliases[n.Name] = k
						} else {
							delete(w.aliases, n.Name)
						}
					}
				}
			}
		}

	case *ast.ExprStmt:
		w.scanExpr(v.X, guarded)

	case *ast.ReturnStmt:
		for _, r := range v.Results {
			w.scanExpr(r, guarded)
		}

	case *ast.BlockStmt:
		// Bare nested block shares the parent's guard scope — guards
		// accrued inside it dominate only its own remainder, which the
		// copy keeps sound (the block may not be where the early return
		// lives, so nothing leaks out).
		w.walkStmts(v.List, copyGuards(guarded, nil))

	case *ast.ForStmt:
		if v.Init != nil {
			w.walkStmt(v.Init, guarded)
		}
		if v.Cond != nil {
			w.scanExpr(v.Cond, guarded)
		}
		if v.Post != nil {
			w.walkStmt(v.Post, guarded)
		}
		// Loop body gets a COPY: guards established inside one
		// iteration re-establish themselves each iteration before any
		// deref they dominate, and must not leak past the loop (the
		// body may never run).
		w.walkStmts(v.Body.List, copyGuards(guarded, nil))

	case *ast.RangeStmt:
		w.scanExpr(v.X, guarded)
		w.walkStmts(v.Body.List, copyGuards(guarded, nil))

	case *ast.SwitchStmt:
		if v.Init != nil {
			w.walkStmt(v.Init, guarded)
		}
		if v.Tag != nil {
			w.scanExpr(v.Tag, guarded)
		}
		// Tagless switch clauses are exclusive conditions evaluated in
		// order: clause i runs only when every earlier clause condition
		// was false, so an earlier `case X == nil:` guarantees X != nil
		// in later clauses (including default).
		priorEq := map[string]bool{}
		for _, cs := range v.Body.List {
			cc, ok := cs.(*ast.CaseClause)
			if !ok {
				continue
			}
			clauseGuards := copyGuards(guarded, priorEq)
			var clauseNeq map[string]bool
			for _, cond := range cc.List {
				w.scanExpr(cond, clauseGuards)
				if v.Tag == nil {
					clauseNeq = mergeKeys(clauseNeq, w.nilCheckKeys(cond, true))
					for k := range w.nilCheckKeys(cond, false) {
						priorEq[k] = true
					}
				}
			}
			// A multi-expr case is an OR of conditions — a `!= nil`
			// term only guards when it's the sole condition.
			if len(cc.List) != 1 {
				clauseNeq = nil
			}
			w.walkStmts(cc.Body, copyGuards(clauseGuards, clauseNeq))
		}

	case *ast.TypeSwitchStmt:
		if v.Init != nil {
			w.walkStmt(v.Init, guarded)
		}
		w.walkStmt(v.Assign, guarded)
		for _, cs := range v.Body.List {
			if cc, ok := cs.(*ast.CaseClause); ok {
				w.walkStmts(cc.Body, copyGuards(guarded, nil))
			}
		}

	case *ast.SelectStmt:
		for _, cs := range v.Body.List {
			if cc, ok := cs.(*ast.CommClause); ok {
				if cc.Comm != nil {
					w.walkStmt(cc.Comm, copyGuards(guarded, nil))
				}
				w.walkStmts(cc.Body, copyGuards(guarded, nil))
			}
		}

	case *ast.DeferStmt:
		w.scanExpr(v.Call, guarded)
	case *ast.GoStmt:
		w.scanExpr(v.Call, guarded)
	case *ast.SendStmt:
		w.scanExpr(v.Chan, guarded)
		w.scanExpr(v.Value, guarded)
	case *ast.IncDecStmt:
		w.scanExpr(v.X, guarded)
	case *ast.LabeledStmt:
		w.walkStmt(v.Stmt, guarded)
	}
}

// recordAliases tracks simple positional `x := s.deps.X` (and `=`)
// assignments. Any non-field RHS — or a shape we can't pair up —
// drops the LHS ident from tracking; conservatism cuts both ways.
func (w *optionalGuardWalker) recordAliases(lhs, rhs []ast.Expr) {
	if len(lhs) != len(rhs) {
		// Multi-value call (`a, err := f()`) — every plain-ident LHS
		// stops being an alias.
		for _, l := range lhs {
			if id, ok := unparen(l).(*ast.Ident); ok {
				delete(w.aliases, id.Name)
			}
		}
		return
	}
	for i, l := range lhs {
		id, ok := unparen(l).(*ast.Ident)
		if !ok {
			continue
		}
		if k, isField := w.fieldKey(rhs[i]); isField {
			w.aliases[id.Name] = k
		} else {
			delete(w.aliases, id.Name)
		}
	}
}

// scanExpr reports unguarded derefs inside an expression tree. The
// deref test is parent-driven: an optional-field expression is only a
// deref when its PARENT node selects / calls / indexes / slices /
// star-derefs it. The bare expression (argument, comparison operand,
// return value, composite-literal value, assignment source/target) is
// never reported.
//
// Short-circuit chains are honored WITHIN the expression: in
// `x != nil && x.M()` the right operand only evaluates when x is
// non-nil, and in `x == nil || x.M()` the right operand only evaluates
// when the `== nil` test failed. Both are idiomatic single-line guards
// (kalshi's trader worker uses the && form heavily) and flagging them
// would teach users the lint can't read Go.
func (w *optionalGuardWalker) scanExpr(e ast.Expr, guarded map[string]bool) {
	if e == nil {
		return
	}
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			// Closure body: walk as statements with a snapshot of the
			// current guards. Captured guards stay honored — a field
			// re-nil'd between guard and closure execution is possible
			// in theory but treating it as unguarded would flag the
			// overwhelmingly common safe pattern.
			w.walkStmts(v.Body.List, copyGuards(guarded, nil))
			return false
		case *ast.BinaryExpr:
			// && / || evaluate left-to-right with short-circuit: a nil
			// check in the left operand guards the right operand. Take
			// over the recursion so the right side scans with the
			// extended guard set; other operators fall through to the
			// generic walk.
			if v.Op == token.LAND || v.Op == token.LOR {
				w.scanExpr(v.X, guarded)
				wantNeq := v.Op == token.LAND
				w.scanExpr(v.Y, copyGuards(guarded, w.nilCheckKeys(v.X, wantNeq)))
				return false
			}
		case *ast.SelectorExpr:
			w.checkDeref(v.X, guarded)
		case *ast.CallExpr:
			// Direct call of a func-typed optional field: s.deps.X(...).
			// Method calls (s.deps.X.M(...)) are caught at the inner
			// SelectorExpr instead, so no double report.
			w.checkDeref(v.Fun, guarded)
		case *ast.IndexExpr:
			w.checkDeref(v.X, guarded)
		case *ast.IndexListExpr:
			w.checkDeref(v.X, guarded)
		case *ast.SliceExpr:
			w.checkDeref(v.X, guarded)
		case *ast.StarExpr:
			w.checkDeref(v.X, guarded)
		}
		return true
	})
}

// checkDeref reports when child (the parent node's operand) denotes an
// optional field that is not currently guarded.
func (w *optionalGuardWalker) checkDeref(child ast.Expr, guarded map[string]bool) {
	child = unparen(child)
	k, ok := w.fieldKey(child)
	if !ok || guarded[k] {
		return
	}
	w.report(child, k)
}

// nilCheckKeys extracts the optional-field keys nil-compared in cond.
//
//	wantNeq=true:  keys k with a top-level `k != nil` term in an &&
//	               chain — sound for guarding the then-branch.
//	wantNeq=false: keys k with a top-level `k == nil` term in an ||
//	               chain — sound for the else-branch and for
//	               early-return accrual (short-circuit: k nil forces
//	               the branch).
//
// Mixed/negated shapes (`!(x == nil)`, De Morgan spellings) are not
// chased — when in doubt the walker flags, and the suppression
// directive covers the exotic-but-safe leftovers.
func (w *optionalGuardWalker) nilCheckKeys(cond ast.Expr, wantNeq bool) map[string]bool {
	out := map[string]bool{}
	var collect func(e ast.Expr)
	collect = func(e ast.Expr) {
		e = unparen(e)
		be, ok := e.(*ast.BinaryExpr)
		if !ok {
			return
		}
		switch {
		case wantNeq && be.Op == token.LAND, !wantNeq && be.Op == token.LOR:
			collect(be.X)
			collect(be.Y)
		case wantNeq && be.Op == token.NEQ, !wantNeq && be.Op == token.EQL:
			if k, isField := w.nilComparedKey(be); isField {
				out[k] = true
			}
		}
	}
	collect(cond)
	return out
}

// nilComparedKey returns the field key when exactly one side of the
// comparison is the nil ident and the other denotes an optional field.
func (w *optionalGuardWalker) nilComparedKey(be *ast.BinaryExpr) (string, bool) {
	x, y := unparen(be.X), unparen(be.Y)
	if isNilIdent(y) {
		return w.fieldKey(x)
	}
	if isNilIdent(x) {
		return w.fieldKey(y)
	}
	return "", false
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// blockAlwaysExits reports whether the block's final statement always
// leaves the enclosing block: return, branch (continue/break/goto),
// panic, os.Exit, or log.Fatal*. An if whose BOTH arms exit also
// counts (covers `if a { return x } else { return y }` guard bodies).
// Anything fancier is treated as falling through — conservative in the
// flag-more direction.
func blockAlwaysExits(b *ast.BlockStmt) bool {
	if b == nil || len(b.List) == 0 {
		return false
	}
	return stmtAlwaysExits(b.List[len(b.List)-1])
}

func stmtAlwaysExits(s ast.Stmt) bool {
	switch v := s.(type) {
	case *ast.ReturnStmt, *ast.BranchStmt:
		return true
	case *ast.ExprStmt:
		call, ok := v.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fun := unparen(call.Fun).(type) {
		case *ast.Ident:
			return fun.Name == "panic"
		case *ast.SelectorExpr:
			// os.Exit / log.Fatal / log.Fatalf / logger.Fatal… — match
			// on the conventional terminator method names.
			name := fun.Sel.Name
			return name == "Exit" || strings.HasPrefix(name, "Fatal")
		}
		return false
	case *ast.BlockStmt:
		return blockAlwaysExits(v)
	case *ast.IfStmt:
		if v.Else == nil {
			return false
		}
		if !blockAlwaysExits(v.Body) {
			return false
		}
		switch e := v.Else.(type) {
		case *ast.BlockStmt:
			return blockAlwaysExits(e)
		case *ast.IfStmt:
			return stmtAlwaysExits(e)
		}
		return false
	}
	return false
}

// copyGuards clones base and folds extra in. Callers use it at every
// branch point so sibling paths can't observe each other's guards.
func copyGuards(base, extra map[string]bool) map[string]bool {
	out := make(map[string]bool, len(base)+len(extra))
	for k := range base {
		out[k] = true
	}
	for k := range extra {
		out[k] = true
	}
	return out
}

// mergeKeys unions two key sets, tolerating nil receivers.
func mergeKeys(a, b map[string]bool) map[string]bool {
	if a == nil {
		a = map[string]bool{}
	}
	for k := range b {
		a[k] = true
	}
	return a
}
