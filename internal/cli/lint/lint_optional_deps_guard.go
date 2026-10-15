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
// Map-typed optional deps
//
// A Deps field may be a table of collaborators (`map[string]Accessor`,
// accepted by deps-are-interfaces). The walker resolves each optional
// field's declared type — a map literal type, or a named type declared in
// the package whose underlying type is one — and asks of a map the only
// question this lint asks of anything: does this site panic while the dep
// is nil? A nil map reads as empty, so these are NOT flagged:
//
//	v := s.deps.M[k]       // index read: the zero value
//	v, ok := s.deps.M[k]   // comma-ok read
//	len(s.deps.M)          // 0
//	for k := range s.deps.M // zero iterations
//	delete(s.deps.M, k)    // no-op
//	s.deps.M.Lookup(k)     // method on a named map type
//
// Two shapes panic, and are flagged until a nil-guard dominates them:
//
//	s.deps.M[k] = v        // write (also op= and ++/--): assignment into a nil map
//	s.deps.M[k].Do()       // deref of a looked-up element
//
// The second is the decision worth stating. Every lookup in a nil map yields
// the element type's zero value; when that is nil (interface, pointer, func,
// chan, map, slice — or a type declared in another package, which the
// walker cannot see into), the deref panics for EVERY key while the dep is
// unwired — exactly the failure this lint exists for. An element read into a
// variable (`a := s.deps.M[k]`) is tracked like an alias, so `a.Do()` is the
// same finding. What guards it: a nil check on the map, a nil check on the
// element (`if a == nil { return }` — a non-nil element proves the map
// non-nil), or a comma-ok read, whose value the walker does not track
// (`a, ok := s.deps.M[k]; if !ok { return }`). An element whose zero value
// is not nil (a struct, a number, a string, an array) is never flagged.
//
// A missing key in a WIRED map yields the same nil element. That is not an
// optional-dep concern — a required map dep has it too — so a nil-guard on
// the map silences element derefs without proving each key present; checking
// the key is the comma-ok read's job, and the fix hint names it.
//
// Slices and arrays keep the plain rule: indexing one is a deref.
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
// collect/format split so `forge lint --json` and `forge project audit --json`
// reuse the same engine.

package lint

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

// OptionalDepsGuardFinding is the exported alias for the optional-deps-guard
// finding, consumed by the internal/cli/audit group's auditOptionalDepsGuard
// roll-up. A type alias (not a new type) so audit's field access
// (.Role/.Package/.File/.Line/.Expr/.Method) is unchanged from when both
// commands lived in package cli.
type OptionalDepsGuardFinding = optionalDepsGuardFinding

// CollectOptionalDepsGuardFindings is the exported entry the audit group
// calls. It forwards to the package-internal collector; the audit roll-up
// is the only cross-package consumer.
func CollectOptionalDepsGuardFindings(projectDir string) ([]OptionalDepsGuardFinding, error) {
	return collectOptionalDepsGuardFindings(projectDir)
}

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
	Use     string // how the site uses the field: optionalUseDeref, optionalUseMapWrite or optionalUseMapElement
}

// The ways a site can use an optional field that panic while it is nil.
const (
	// optionalUseDeref is a selector / call / index / slice / star on the
	// field itself.
	optionalUseDeref = "deref"
	// optionalUseMapWrite is an assignment into a map-typed field.
	optionalUseMapWrite = "map-write"
	// optionalUseMapElement is a deref of an element looked up in a
	// map-typed field whose element type's zero value is nil.
	optionalUseMapElement = "map-element"
)

// optionalDepsGuardMessage is a finding's one-line diagnosis, shared by the
// text report and the --json message.
func optionalDepsGuardMessage(f optionalDepsGuardFinding) string {
	switch f.Use {
	case optionalUseMapWrite:
		return fmt.Sprintf("%s writes into optional map dep Deps.%s (marked `// forge:optional-dep` — may be nil, and assigning into a nil map panics) without a dominating nil-guard in %s", f.Expr, f.Field, f.Method)
	case optionalUseMapElement:
		return fmt.Sprintf("%s dereferences an element of optional map dep Deps.%s (marked `// forge:optional-dep` — while it is nil every lookup yields the element's zero value, nil) without checking it in %s", f.Expr, f.Field, f.Method)
	}
	return fmt.Sprintf("%s dereferences optional dep Deps.%s (marked `// forge:optional-dep` — may be nil) without a dominating nil-guard in %s", f.Expr, f.Field, f.Method)
}

// optionalDepsGuardFixHint renders the canonical remediation for a
// finding — shown in text mode and carried as fix_hint in JSON.
func optionalDepsGuardFixHint(f optionalDepsGuardFinding) string {
	const suppress = "if this site is provably safe (invariant established elsewhere), append `// forge:optional-checked` on the line to suppress"
	switch f.Use {
	case optionalUseMapWrite:
		return fmt.Sprintf("guard the write with `if s.deps.%s == nil { return ... }` (or `if ... != nil { ... }`); %s", f.Field, suppress)
	case optionalUseMapElement:
		return fmt.Sprintf("read the element with comma-ok and check it (`v, ok := s.deps.%s[k]; if !ok { ... }`), or nil-check the value before use (`if v == nil { ... }`), which also covers a key the wired map lacks; %s", f.Field, suppress)
	}
	return fmt.Sprintf(
		"add an early return `if %s == nil { return ... }` at the top of %s, or wrap the use in `if %s != nil { ... }`; %s",
		f.Expr, f.Method, f.Expr, suppress)
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
		_, _ = fmt.Fprintf(w, "      %s\n", optionalDepsGuardMessage(f))
		_, _ = fmt.Fprintf(w, "      → %s\n", optionalDepsGuardFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d unguarded optional-dep deref(s).\n", len(findings))
	_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
}

// collectOptionalDepsGuardFindings is the shared engine behind text
// mode, `forge lint --json`, and `forge project audit --json`. Findings come
// back sorted by (file, line, col) so output is deterministic.
func collectOptionalDepsGuardFindings(projectDir string) ([]optionalDepsGuardFinding, error) {
	var findings []optionalDepsGuardFinding

	// Same role roots as bootstrap-deps-coverage: every tree that hosts
	// the conventional <pkg>/Deps shape. Missing roots are fine — many
	// projects ship no operators/ or workers/.
	roleRoots := []string{"internal", "internal/handlers", "internal/workers", "internal/operators"}
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
			var optional []codegen.DepsField
			for _, d := range deps {
				if d.Optional {
					optional = append(optional, d)
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

// scanPackageForUnguardedDerefs parses every non-test .go file in pkgDir,
// resolves the optional fields' types against the package's own type
// declarations, and runs the guard walker over each function body of the
// non-generated files.
func scanPackageForUnguardedDerefs(projectDir, pkgDir, role, pkgName string, optionalDeps []codegen.DepsField) ([]optionalDepsGuardFinding, error) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pkgDir, err)
	}

	type parsedFile struct {
		path string
		file *ast.File
	}
	var walk []parsedFile
	types := packageTypeDecls{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		// Tests construct zero-value Deps on purpose — never scanned, and
		// their type declarations are not the package's.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fp := filepath.Join(pkgDir, name)
		file, parseErr := parser.ParseFile(fset, fp, nil, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			// Don't double-report parse errors — the Go toolchain will.
			continue
		}
		types.collect(file)
		// Generated files (_gen.go) declare types the fields may name, but
		// are forge-owned and regenerate — flagging them would be noise
		// the user can't act on.
		if !strings.HasSuffix(name, "_gen.go") {
			walk = append(walk, parsedFile{path: fp, file: file})
		}
	}

	optional := make(map[string]optionalFieldShape, len(optionalDeps))
	for _, d := range optionalDeps {
		optional[d.Name] = types.shapeOf(d.Type)
	}

	var findings []optionalDepsGuardFinding
	for _, pf := range walk {
		fp, file := pf.path, pf.file
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
				fset:        fset,
				file:        rel,
				role:        role,
				pkg:         pkgName,
				optional:    optional,
				suppressed:  suppressed,
				funcName:    fn.Name.Name,
				depsIdents:  map[string]bool{},
				aliases:     map[string]string{},
				elemAliases: map[string]string{},
				findings:    &findings,
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
	optional   map[string]optionalFieldShape
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

	// elemAliases maps a local identifier to the map-typed optional field
	// it holds an ELEMENT of (`a := s.deps.M[k]` → elemAliases["a"] =
	// "M"), for an element type whose zero value is nil. A deref of the
	// alias is a deref of a looked-up element; a nil check on it guards
	// the field, because a non-nil element proves the map non-nil.
	elemAliases map[string]string

	findings *[]optionalDepsGuardFinding
}

// optionalTarget is what an expression denotes, when it denotes an
// optional field at all: the field itself, or an element looked up in a
// map-typed field.
type optionalTarget int

const (
	targetField optionalTarget = iota
	targetElement
)

// fieldKey resolves an expression to the optional field it denotes — the
// field itself or, for a map-typed field, an element looked up in it — or
// ("", false). Guards key on the field either way: a nil check on a
// looked-up element proves the map non-nil.
func (w *optionalGuardWalker) fieldKey(e ast.Expr) (string, bool) {
	k, _, ok := w.resolve(e)
	return k, ok
}

// resolve resolves an expression to the optional field it denotes, and
// whether it is the field or one of its elements. Recognized roots:
//
//	x              alias previously assigned from the field (or an element)
//	d.X            d is a Deps-typed receiver/param ident
//	s.deps.X       any ident . deps/Deps . field — the conventional
//	               implementation shape (s *Service holding `deps Deps`)
//	s.deps.M[k]    an element of map-typed field M, when M's element type
//	               has a nil zero value (anything above in place of s.deps.M)
func (w *optionalGuardWalker) resolve(e ast.Expr) (string, optionalTarget, bool) {
	e = unparen(e)
	switch v := e.(type) {
	case *ast.Ident:
		if k, ok := w.aliases[v.Name]; ok {
			return k, targetField, true
		}
		if k, ok := w.elemAliases[v.Name]; ok {
			return k, targetElement, true
		}
	case *ast.IndexExpr:
		k, target, ok := w.resolve(v.X)
		if ok && target == targetField && w.optional[k].isMap && w.optional[k].elemNilable {
			return k, targetElement, true
		}
	case *ast.SelectorExpr:
		if k, ok := w.selectorField(v); ok {
			return k, targetField, true
		}
	}
	return "", targetField, false
}

// selectorField resolves `d.X` / `s.deps.X` to the optional field X.
func (w *optionalGuardWalker) selectorField(v *ast.SelectorExpr) (string, bool) {
	if _, ok := w.optional[v.Sel.Name]; !ok {
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

// report records a finding for the use of field key at expression
// root, unless the site is suppressed.
func (w *optionalGuardWalker) report(root ast.Expr, key, use string) {
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
		Use:     use,
	})
}

// exprString renders the small selector chains we report (s.deps.X,
// d.X, s.deps.M[k], alias idents). Hand-rolled instead of go/printer
// because the shapes are tiny and we don't want printer's fset plumbing
// here.
func exprString(e ast.Expr) string {
	switch v := unparen(e).(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.IndexExpr:
		idx := exprString(v.Index)
		if idx == "<expr>" {
			idx = "…"
		}
		return exprString(v.X) + "[" + idx + "]"
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
	// Each non-trivial statement kind delegates to a dedicated walker so
	// the per-kind flow analysis stays readable; trivial one-liners are
	// handled inline. Traversal order and the guard-map mutation contract
	// (only walkIfStmt's early-return accrual mutates the caller-owned
	// `guarded`) are unchanged from the original single-function switch.
	switch v := s.(type) {
	case *ast.IfStmt:
		w.walkIfStmt(v, guarded)
	case *ast.AssignStmt:
		w.walkAssignStmt(v, guarded)
	case *ast.DeclStmt:
		w.walkDeclStmt(v, guarded)
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
		w.walkForStmt(v, guarded)
	case *ast.RangeStmt:
		w.scanExpr(v.X, guarded)
		if v.Tok == token.ASSIGN {
			w.checkMapWrite(v.Key, guarded)
			w.checkMapWrite(v.Value, guarded)
		}
		w.walkStmts(v.Body.List, copyGuards(guarded, nil))
	case *ast.SwitchStmt:
		w.walkSwitchStmt(v, guarded)
	case *ast.TypeSwitchStmt:
		w.walkTypeSwitchStmt(v, guarded)
	case *ast.SelectStmt:
		w.walkSelectStmt(v, guarded)
	case *ast.DeferStmt:
		w.scanExpr(v.Call, guarded)
	case *ast.GoStmt:
		w.scanExpr(v.Call, guarded)
	case *ast.SendStmt:
		w.scanExpr(v.Chan, guarded)
		w.scanExpr(v.Value, guarded)
	case *ast.IncDecStmt:
		w.checkMapWrite(v.X, guarded)
		w.scanExpr(v.X, guarded)
	case *ast.LabeledStmt:
		w.walkStmt(v.Stmt, guarded)
	}
}

// walkIfStmt handles `if`/`else` flow: it scans the condition, walks each
// branch with the guards that branch establishes, and accrues an
// early-return guard into the caller-owned `guarded` map.
func (w *optionalGuardWalker) walkIfStmt(v *ast.IfStmt, guarded map[string]bool) {
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
}

// walkAssignStmt scans both sides of an assignment for derefs and records
// any alias the assignment establishes.
func (w *optionalGuardWalker) walkAssignStmt(v *ast.AssignStmt, guarded map[string]bool) {
	for _, rhs := range v.Rhs {
		w.scanExpr(rhs, guarded)
	}
	for _, lhs := range v.Lhs {
		// LHS derefs count too: `s.deps.X.Field = v` derefs X.
		// Plain `s.deps.X = v` does not (the field itself is the
		// target) — scanExpr's parent-node rule handles both. An index
		// into a map-typed field is a read to scanExpr, so the write is
		// checked here.
		w.checkMapWrite(lhs, guarded)
		w.scanExpr(lhs, guarded)
	}
	w.recordAliases(v.Lhs, v.Rhs)
}

// checkMapWrite reports an assignment target that writes into a map-typed
// optional field (`s.deps.M[k] = v`, `m[k] += v`, `m[k]++` through an
// alias): assigning into a nil map panics, where reading one does not.
func (w *optionalGuardWalker) checkMapWrite(target ast.Expr, guarded map[string]bool) {
	ix, ok := unparen(target).(*ast.IndexExpr)
	if !ok {
		return
	}
	root := unparen(ix.X)
	k, kind, ok := w.resolve(root)
	if !ok || kind != targetField || !w.optional[k].isMap || guarded[k] {
		return
	}
	w.report(root, k, optionalUseMapWrite)
}

// walkDeclStmt scans `var` value expressions and records `var x = s.deps.X`
// aliases the same way an `:=` assignment would.
func (w *optionalGuardWalker) walkDeclStmt(v *ast.DeclStmt, guarded map[string]bool) {
	gd, ok := v.Decl.(*ast.GenDecl)
	if !ok {
		return
	}
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
				w.bindAlias(n.Name, vs.Values[i])
			}
		}
	}
}

// walkForStmt walks a classic for loop; the body gets a guard COPY so
// guards it establishes don't leak past the loop (the body may never run).
func (w *optionalGuardWalker) walkForStmt(v *ast.ForStmt, guarded map[string]bool) {
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
}

// walkSwitchStmt handles an expression/tagless switch. Tagless clauses are
// exclusive, so an earlier `case X == nil:` guards X in later clauses.
func (w *optionalGuardWalker) walkSwitchStmt(v *ast.SwitchStmt, guarded map[string]bool) {
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
}

// walkTypeSwitchStmt walks a type switch; each clause body gets a fresh
// guard copy (the type assertion establishes no nil guard).
func (w *optionalGuardWalker) walkTypeSwitchStmt(v *ast.TypeSwitchStmt, guarded map[string]bool) {
	if v.Init != nil {
		w.walkStmt(v.Init, guarded)
	}
	w.walkStmt(v.Assign, guarded)
	for _, cs := range v.Body.List {
		if cc, ok := cs.(*ast.CaseClause); ok {
			w.walkStmts(cc.Body, copyGuards(guarded, nil))
		}
	}
}

// walkSelectStmt walks each comm clause's guard statement and body with a
// fresh guard copy.
func (w *optionalGuardWalker) walkSelectStmt(v *ast.SelectStmt, guarded map[string]bool) {
	for _, cs := range v.Body.List {
		if cc, ok := cs.(*ast.CommClause); ok {
			if cc.Comm != nil {
				w.walkStmt(cc.Comm, copyGuards(guarded, nil))
			}
			w.walkStmts(cc.Body, copyGuards(guarded, nil))
		}
	}
}

// recordAliases tracks simple positional `x := s.deps.X` (and `=`)
// assignments. Any non-field RHS — or a shape we can't pair up —
// drops the LHS ident from tracking; conservatism cuts both ways.
func (w *optionalGuardWalker) recordAliases(lhs, rhs []ast.Expr) {
	if len(lhs) != len(rhs) {
		// Multi-value call (`a, err := f()`) — every plain-ident LHS
		// stops being an alias.
		// That includes the comma-ok read `v, ok := s.deps.M[k]`, whose
		// value is deliberately untracked: `ok` is its guard.
		for _, l := range lhs {
			if id, ok := unparen(l).(*ast.Ident); ok {
				w.bindAlias(id.Name, nil)
			}
		}
		return
	}
	for i, l := range lhs {
		id, ok := unparen(l).(*ast.Ident)
		if !ok {
			continue
		}
		w.bindAlias(id.Name, rhs[i])
	}
}

// bindAlias records what name holds after `name := value` (or `=` / `var`):
// the optional field, an element of a map-typed one, or neither (a nil
// value drops it).
func (w *optionalGuardWalker) bindAlias(name string, value ast.Expr) {
	delete(w.aliases, name)
	delete(w.elemAliases, name)
	if value == nil {
		return
	}
	k, kind, ok := w.resolve(value)
	switch {
	case !ok:
	case kind == targetField:
		w.aliases[name] = k
	default:
		w.elemAliases[name] = k
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
// optional field, or an element looked up in a map-typed one, that is not
// currently guarded.
//
// Every deref of a map-typed field itself is safe while it is nil: an
// index reads the zero value, and a method on a named map type takes the
// nil map as its receiver. (Writes are checked at the assignment, by
// checkMapWrite; a call, star or slice of a map does not compile.)
func (w *optionalGuardWalker) checkDeref(child ast.Expr, guarded map[string]bool) {
	child = unparen(child)
	k, kind, ok := w.resolve(child)
	if !ok || guarded[k] {
		return
	}
	switch {
	case kind == targetElement:
		w.report(child, k, optionalUseMapElement)
	case !w.optional[k].isMap:
		w.report(child, k, optionalUseDeref)
	}
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

// ---------------------------------------------------------------------------
// Field types — what the walker needs to know about an optional field.
// ---------------------------------------------------------------------------

// optionalFieldShape is what the walker knows of an optional field's
// declared type: whether it is a map, and if so whether its element's zero
// value is nil (so a looked-up element of the nil map is nil).
type optionalFieldShape struct {
	isMap       bool
	elemNilable bool
}

// packageTypeDecls are the type declarations of one package, by name — the
// only types the walker resolves. It has no type checker: a type declared
// in another package is unknown to it.
type packageTypeDecls map[string]ast.Expr

func (d packageTypeDecls) collect(f *ast.File) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok && ts.TypeParams == nil {
				d[ts.Name.Name] = ts.Type
			}
		}
	}
}

// shapeOf classifies a Deps field's type expression (DepsField.Type, as
// printed by the deps parser). An unparseable or unknown type is not a map,
// which keeps the plain deref rule: the conservative direction.
func (d packageTypeDecls) shapeOf(typ string) optionalFieldShape {
	expr, err := parser.ParseExpr(typ)
	if err != nil {
		return optionalFieldShape{}
	}
	m, ok := d.underlying(expr).(*ast.MapType)
	if !ok {
		return optionalFieldShape{}
	}
	return optionalFieldShape{isMap: true, elemNilable: d.zeroIsNil(m.Value)}
}

// underlying follows named types declared in this package (and parens) to
// the type literal they stand for. A name it cannot follow — a predeclared
// type, a type from another package, a generic instantiation — is returned
// as is.
func (d packageTypeDecls) underlying(t ast.Expr) ast.Expr {
	seen := map[string]bool{}
	for {
		t = unparen(t)
		id, ok := t.(*ast.Ident)
		if !ok || seen[id.Name] {
			return t
		}
		next, declared := d[id.Name]
		if !declared {
			return t
		}
		seen[id.Name] = true
		t = next
	}
}

// predeclaredNonNil are the predeclared types whose zero value is not nil.
var predeclaredNonNil = map[string]bool{
	"bool": true, "string": true, "byte": true, "rune": true, "uintptr": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

// zeroIsNil reports whether t's zero value is nil — so that dereferencing
// it (a method call, a field, a call, an index) can panic. A type the
// walker cannot see into counts as nil: the conservative direction, and the
// right answer for the interface a table of collaborators usually holds.
func (d packageTypeDecls) zeroIsNil(t ast.Expr) bool {
	switch u := d.underlying(t).(type) {
	case *ast.StructType:
		return false
	case *ast.ArrayType:
		return u.Len == nil // a slice is nil; an array is a value
	case *ast.Ident:
		return !predeclaredNonNil[u.Name] // error, any, an unresolved name
	}
	return true // pointer, map, func, chan, interface, another package's type
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
