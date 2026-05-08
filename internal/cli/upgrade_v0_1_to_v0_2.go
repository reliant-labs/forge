package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// upgrade_v0_1_to_v0_2.go — codemod for the wire-gen migration.
//
// The migration story (see SKILL.md at
// internal/templates/project/skills/forge/migration/v0.1-to-v0.2/):
//
//   - Two-phase Bootstrap → Setup → ApplyDeps becomes single-phase
//     wire-then-construct. Setup builds infra and assigns to *App
//     fields; wire_gen.go (codegen'd by `forge generate`) then assembles
//     the COMPLETE Deps struct; bootstrap calls service.New(deps)
//     directly. ApplyDeps is gone.
//
// What the codemod does (mechanical / unambiguous):
//
//   1. Walk pkg/app/setup.go. For every
//      `app.Services.X.ApplyDeps(X.Deps{...})` call:
//        - Extract field=value pairs.
//        - Prepend `app.<Field> = <value>` lines above the call site
//          for any field whose name doesn't match an existing *App
//          field. (Conventional fields like Logger / Config are skipped
//          — wire_gen sources them from bootstrap args, not App fields.)
//        - Delete the ApplyDeps call.
//
//   2. Walk every handlers/<svc>/handlers.go. Remove per-RPC
//      `if s.deps.<Field> == nil { return ..., connect.NewError(connect.CodeFailedPrecondition, ...) }`
//      guards where validateDeps now covers <Field>. The pattern
//      we match is conservative — single nil-check, FailedPrecondition,
//      "is required"-style message — so anything weirder gets left for
//      LLM review.
//
// What the codemod surfaces but doesn't touch:
//
//   - ApplyDeps method bodies in handlers/<svc>/service.go (deleted by
//     `forge generate` — the service.go.tmpl no longer emits ApplyDeps).
//   - Test fixtures that call ApplyDeps directly (LLM-assisted: convert
//     to service.New(deps) with mock-populated Deps).
//   - validateDeps additions (LLM-assisted: which deps to gate is an
//     intent decision; codemod surfaces the candidates from the
//     promoted setup.go assignments).
//
// The codemod is deliberately one-way (no `--undo`). The bug class it
// rewrites is "code that won't compile after `forge generate` rewrites
// bootstrap.go to call wireXxxDeps". Rolling back means restoring
// pre-upgrade git state, not running an inverse codemod.

func init() {
	registerCodemod("0.1", "0.2", migrateV01ToV02)
}

// migrateV01ToV02 implements the v0.1 → v0.2 codemod. See file-top
// comment for the rewrite contract.
func migrateV01ToV02(projectDir string) (CodemodReport, error) {
	report := CodemodReport{
		VerifyCommands: []string{
			"forge generate",
			"go build ./...",
			"go test -count=1 ./...",
			"forge lint",
		},
	}

	// Step 1: rewrite pkg/app/setup.go.
	setupPath := filepath.Join(projectDir, "pkg", "app", "setup.go")
	if _, err := os.Stat(setupPath); err == nil {
		auto, manual, err := codemodSetupGo(setupPath)
		if err != nil {
			return report, fmt.Errorf("rewrite setup.go: %w", err)
		}
		report.Auto = append(report.Auto, auto...)
		report.Manual = append(report.Manual, manual...)
	}

	// Step 2: walk handlers/<svc>/handlers.go for per-RPC nil-checks.
	handlersDir := filepath.Join(projectDir, "handlers")
	if _, err := os.Stat(handlersDir); err == nil {
		entries, err := os.ReadDir(handlersDir)
		if err != nil {
			return report, fmt.Errorf("read handlers dir: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(handlersDir, entry.Name(), "handlers.go")
			if _, err := os.Stat(path); err != nil {
				continue
			}
			auto, manual, err := codemodHandlersGo(path, projectDir)
			if err != nil {
				// Don't bail on a single handler — record as manual
				// and continue. The LLM can pick up the slack.
				rel, _ := filepath.Rel(projectDir, path)
				report.Manual = append(report.Manual, ManualItem{
					File:   rel,
					Reason: fmt.Sprintf("codemod could not parse this file: %v — review nil-checks manually", err),
				})
				continue
			}
			report.Auto = append(report.Auto, auto...)
			report.Manual = append(report.Manual, manual...)
		}
	}

	// Step 3: surface the post-codemod regenerate. We don't run
	// `forge generate` from inside the codemod — the user is in
	// the upgrade command's flow and runs it via the verification
	// commands. Note in Auto so the user sees what's coming.
	report.Auto = append(report.Auto,
		"Run `forge generate` to emit pkg/app/wire_gen.go, pkg/app/app_gen.go, and pkg/app/app_extras.go (the Tier-2 user-extension scaffold). The new bootstrap.go will call wireXxxDeps(app, cfg, logger, devMode) instead of the v0.1 construct-then-ApplyDeps shape.",
	)

	return report, nil
}

// codemodSetupGo rewrites pkg/app/setup.go in place: every
// `app.Services.X.ApplyDeps(X.Deps{Field1: v1, ...})` call becomes a
// sequence of `app.<Field1> = v1` assignments followed by deletion of
// the ApplyDeps call.
//
// Returns (auto-applied summaries, manual-review items, error).
//
// Implementation note: we use go/parser + go/format rather than regex
// — ApplyDeps calls span multiple lines and have nested struct
// literals that defeat line-oriented matching. A full AST walk also
// makes it easy to skip cases the codemod shouldn't touch (calls
// inside if-bodies, loops, etc. — anything other than a top-level
// statement in Setup).
func codemodSetupGo(path string) ([]string, []ManualItem, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("parse setup.go: %w", err)
	}

	// Find Setup func.
	var setupFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == "Setup" && fn.Recv == nil {
			setupFn = fn
			break
		}
	}
	if setupFn == nil || setupFn.Body == nil {
		return nil, nil, nil
	}

	var (
		auto           []string
		manual         []ManualItem
		insertedFields = map[string]bool{} // dedupe app.<Field> assignments
		didChange      bool
		// removedRanges records [start, end] token positions for every
		// ApplyDeps call we delete. After the rewrite, we filter
		// file.Comments to drop any comment whose position falls inside
		// a removed range — go/format would otherwise float those
		// comments to weird places (between the SelectorExpr halves of
		// the synthesized AssignStmts).
		removedRanges [][2]token.Pos
	)

	// ApplyDeps calls in v0.1 projects appear in three shapes:
	//
	//   (a) `app.Services.X.ApplyDeps(X.Deps{...})`             — bare ExprStmt
	//   (b) `if err := app.Services.X.ApplyDeps(...); err != nil { return ... }`
	//                                                            — IfStmt with init
	//   (c) Either of the above wrapped inside `if app.Services != nil { ... }`
	//                                                            — nested block
	//
	// rewriteApplyDepsBlock recursively walks each statement list,
	// promoting Deps fields and deleting the ApplyDeps call (or the
	// whole IfStmt for shape (b)).
	rewriteApplyDepsBlock(setupFn.Body, fset, &auto, &manual, insertedFields, &didChange, &removedRanges)

	if !didChange {
		// Nothing to do — file already in v0.2 shape, or had no
		// ApplyDeps calls.
		return nil, manual, nil
	}

	// Strip comments that fell inside removed ApplyDeps call ranges.
	// These comments were attached to inline trailing positions on
	// CompositeLit fields (e.g. `Stripe: app.Packages.PkgBilling, //
	// pkgBilling.Service satisfies svcBilling.StripeClient`); without
	// the host node, go/format scatters them across the synthesized
	// assignments. The user's intent-bearing comments outside the
	// ApplyDeps call (block comments above each call, etc.) are
	// preserved.
	if len(removedRanges) > 0 {
		filtered := file.Comments[:0]
		for _, cg := range file.Comments {
			if !commentInAnyRange(cg, removedRanges) {
				filtered = append(filtered, cg)
			}
		}
		file.Comments = filtered
	}

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, manual, fmt.Errorf("format rewritten setup.go: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, manual, err
	}
	return auto, manual, nil
}

// commentInAnyRange reports whether a comment group's position falls
// inside any of the [start, end] token.Pos ranges. Used to drop comments
// that were attached to fields inside a removed ApplyDeps call.
func commentInAnyRange(cg *ast.CommentGroup, ranges [][2]token.Pos) bool {
	if cg == nil {
		return false
	}
	pos := cg.Pos()
	end := cg.End()
	for _, r := range ranges {
		if pos >= r[0] && end <= r[1] {
			return true
		}
	}
	return false
}

// rewriteApplyDepsBlock walks the statements in block and rewrites
// ApplyDeps occurrences in place. Recurses into nested IfStmt bodies
// so calls wrapped in `if app.Services != nil { ... }` are handled.
//
// Three shapes are matched:
//   - `app.Services.X.ApplyDeps(X.Deps{...})` (bare ExprStmt)
//   - `if err := app.Services.X.ApplyDeps(X.Deps{...}); err != nil { return ... }`
//     (IfStmt with init — codemod replaces the whole IfStmt with the
//     promoted assignments since the err branch was the v0.1 nil-receiver
//     guard, no longer reachable in v0.2's wire-gen world).
//   - Either of the above nested inside an outer guard IfStmt (e.g.
//     `if app.Services != nil { ... }`).
func rewriteApplyDepsBlock(block *ast.BlockStmt, fset *token.FileSet, auto *[]string, manual *[]ManualItem, insertedFields map[string]bool, didChange *bool, removedRanges *[][2]token.Pos) {
	if block == nil {
		return
	}
	// Track whether this block contained any rewrite. If so, we record
	// the entire block range as a "drop comments" zone — the comments
	// inside it were attached to deleted ApplyDeps calls or to gaps
	// between them, and trying to preserve them would float them onto
	// the synthesized AssignStmts in nonsensical positions. Block-level
	// comments above the block (e.g. the "ApplyDeps mutates the
	// *Service pointer..." preamble) live OUTSIDE the block's start,
	// so they're preserved.
	blockStart := block.Lbrace
	blockEnd := block.Rbrace
	blockHadRewrite := false

	newStmts := make([]ast.Stmt, 0, len(block.List))
	for _, stmt := range block.List {
		// Shape (a): bare ApplyDeps ExprStmt.
		if expr, ok := stmt.(*ast.ExprStmt); ok {
			if call, ok := expr.X.(*ast.CallExpr); ok && isAppServicesApplyDeps(call) {
				promoted, replaced := promoteApplyDepsCall(call, fset, auto, manual, insertedFields)
				if replaced {
					*didChange = true
					blockHadRewrite = true
					*removedRanges = append(*removedRanges, [2]token.Pos{call.Pos(), call.End()})
					newStmts = append(newStmts, promoted...)
					continue
				}
			}
		}

		// Shape (b): if-init form `if err := app.Services.X.ApplyDeps(...); err != nil { ... }`.
		if ifStmt, ok := stmt.(*ast.IfStmt); ok && ifStmt.Init != nil {
			if assign, ok := ifStmt.Init.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && isAppServicesApplyDeps(call) {
					promoted, replaced := promoteApplyDepsCall(call, fset, auto, manual, insertedFields)
					if replaced {
						*didChange = true
						blockHadRewrite = true
						// Removed range spans the whole IfStmt — its
						// body is the v0.1 error-return that's no
						// longer reachable. Comments inside it (e.g.
						// `return fmt.Errorf("setup: ...")`) get
						// dropped along with the deletion.
						*removedRanges = append(*removedRanges, [2]token.Pos{ifStmt.Pos(), ifStmt.End()})
						newStmts = append(newStmts, promoted...)
						continue
					}
				}
			}
		}

		// Shape (c): outer guard IfStmt — recurse into both branches.
		if ifStmt, ok := stmt.(*ast.IfStmt); ok {
			rewriteApplyDepsBlock(ifStmt.Body, fset, auto, manual, insertedFields, didChange, removedRanges)
			if elseBlock, ok := ifStmt.Else.(*ast.BlockStmt); ok {
				rewriteApplyDepsBlock(elseBlock, fset, auto, manual, insertedFields, didChange, removedRanges)
			}
		}

		newStmts = append(newStmts, stmt)
	}
	block.List = newStmts

	// If we rewrote at least one ApplyDeps in THIS block, drop the
	// whole block's interior from the comment-preservation set. The
	// only intent-bearing comments here were attached to ApplyDeps
	// calls or to "// Stripe nil — ..." annotations between calls,
	// both of which lose their host nodes after the rewrite. Keeping
	// them would float them onto the new AssignStmts (e.g. between
	// `app.` and the field name). The block-preamble comment above
	// the outer guard `if app.Services != nil { ... }` lives outside
	// blockStart, so it's preserved.
	if blockHadRewrite {
		*removedRanges = append(*removedRanges, [2]token.Pos{blockStart, blockEnd})
	}
}

// isAppServicesApplyDeps returns true when call is
// `app.Services.<Svc>.ApplyDeps(...)`.
func isAppServicesApplyDeps(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ApplyDeps" {
		return false
	}
	svcSel, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	svcParent, ok := svcSel.X.(*ast.SelectorExpr)
	if !ok || svcParent.Sel.Name != "Services" {
		return false
	}
	appIdent, ok := svcParent.X.(*ast.Ident)
	if !ok || appIdent.Name != "app" {
		return false
	}
	return true
}

// promoteApplyDepsCall converts `app.Services.X.ApplyDeps(X.Deps{...})`
// into a sequence of `app.<Field> = <value>` assignment statements.
// Returns (assignments, replaced) — replaced=false means the call had
// an unexpected shape and the caller should leave the original stmt in
// place plus push a Manual item.
func promoteApplyDepsCall(call *ast.CallExpr, fset *token.FileSet, auto *[]string, manual *[]ManualItem, insertedFields map[string]bool) ([]ast.Stmt, bool) {
	if len(call.Args) != 1 {
		pos := fset.Position(call.Pos())
		*manual = append(*manual, ManualItem{
			File:   trimToProjectRel(pos.Filename),
			Line:   pos.Line,
			Reason: "ApplyDeps call has unexpected arity — codemod expects exactly one Deps literal arg. Convert manually: assign each rich dep to app.<Field> and delete the call.",
		})
		return nil, false
	}
	comp, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		pos := fset.Position(call.Pos())
		*manual = append(*manual, ManualItem{
			File:   trimToProjectRel(pos.Filename),
			Line:   pos.Line,
			Reason: "ApplyDeps arg is not a struct literal — codemod can't extract field assignments. Convert manually.",
		})
		return nil, false
	}

	conventional := map[string]bool{
		"Logger": true, "Config": true, "Authorizer": true,
	}

	var stmts []ast.Stmt
	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		keyIdent, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		fieldName := keyIdent.Name
		if conventional[fieldName] {
			continue
		}
		if insertedFields[fieldName] {
			// Already promoted by an earlier ApplyDeps in this Setup
			// — don't double-assign.
			continue
		}
		insertedFields[fieldName] = true

		stmts = append(stmts, &ast.AssignStmt{
			Lhs: []ast.Expr{
				&ast.SelectorExpr{
					X:   ast.NewIdent("app"),
					Sel: ast.NewIdent(fieldName),
				},
			},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{kv.Value},
		})
		*auto = append(*auto, fmt.Sprintf("setup.go: promoted ApplyDeps field `%s` to `app.%s = ...` (declare the field on AppExtras in pkg/app/app_extras.go)", fieldName, fieldName))
		*manual = append(*manual, ManualItem{
			File:   "pkg/app/app_extras.go",
			Reason: fmt.Sprintf("Add `%s <type>` to AppExtras so wire_gen can resolve `app.%s` (the codemod promoted the assignment in setup.go but doesn't know the field type — copy from the Deps struct in handlers/<svc>/service.go).", fieldName, fieldName),
		})
	}

	// Record the deletion in Auto.
	pos := fset.Position(call.Pos())
	sel := call.Fun.(*ast.SelectorExpr)
	*auto = append(*auto, fmt.Sprintf("setup.go:%d removed `%s.ApplyDeps(...)` (replaced by wire_gen-driven construction)", pos.Line, exprToString(sel.X)))
	return stmts, true
}

// trimToProjectRel returns the relative-to-project path for filename,
// trimming everything before "pkg/app/" when present (the only spot the
// codemod operates on right now). Falls back to the basename when the
// prefix isn't present so the manual report still gets a usable label.
func trimToProjectRel(filename string) string {
	if i := strings.Index(filename, "pkg/app/"); i >= 0 {
		return filename[i:]
	}
	return filepath.Base(filename)
}

// codemodHandlersGo walks one handlers/<svc>/handlers.go file and
// removes per-RPC `if s.deps.<Field> == nil { return ... }` guards
// that match the conservative pattern. Returns (auto, manual, err).
//
// The conservative pattern (must match all):
//   - Statement is `if X == nil { return ... }`
//   - X is `s.deps.<UpperCaseField>` (selector on receiver name 's')
//   - The if-body has exactly one statement: a `return` returning a
//     `connect.NewError(connect.CodeFailedPrecondition, ...)` call.
//
// Anything that doesn't match — multi-statement bodies, return values
// without connect.NewError, conditions other than `== nil` — is left
// in place and surfaced via Manual. The codemod errs on the side of
// "leave it for the LLM" rather than risk deleting a legitimate
// optional-dep guard.
func codemodHandlersGo(path, projectDir string) ([]string, []ManualItem, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}

	relPath, _ := filepath.Rel(projectDir, path)
	if relPath == "" {
		relPath = path
	}

	// Build the optional-dep allowlist from the sibling service.go.
	// Fields tagged `// forge:optional-dep` are intentionally allowed
	// to be nil at runtime; per-RPC `if s.deps.X == nil { ... }`
	// guards on them are NOT boilerplate and must NOT be removed by
	// this codemod. ParseServiceDeps reads the marker from the field's
	// doc/inline comment.
	optionalDeps := map[string]bool{}
	if depsFields, depsErr := codegen.ParseServiceDeps(filepath.Dir(path)); depsErr == nil {
		for _, df := range depsFields {
			if df.Optional {
				optionalDeps[df.Name] = true
			}
		}
	}

	var (
		auto    []string
		manual  []ManualItem
		didChange bool
	)

	// Walk every method on a *Service receiver and strip qualifying
	// nil-checks from the function body.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		// Receiver type must be *Service (typed pointer to the
		// service struct in this package).
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "Service" {
			continue
		}
		// Receiver name must be 's' for the codemod's pattern. If the
		// project uses a different convention, skip and let the LLM
		// review.
		if len(fn.Recv.List[0].Names) == 0 || fn.Recv.List[0].Names[0].Name != "s" {
			continue
		}
		if fn.Body == nil {
			continue
		}

		newStmts := make([]ast.Stmt, 0, len(fn.Body.List))
		for _, stmt := range fn.Body.List {
			ifStmt, ok := stmt.(*ast.IfStmt)
			if !ok {
				newStmts = append(newStmts, stmt)
				continue
			}
			fieldName, ok := matchDepsNilCheckCondition(ifStmt.Cond)
			if !ok {
				newStmts = append(newStmts, stmt)
				continue
			}
			if optionalDeps[fieldName] {
				// Field is tagged `// forge:optional-dep` — the guard
				// is intentional, not boilerplate. Leave it untouched
				// and don't surface it for review either.
				newStmts = append(newStmts, stmt)
				continue
			}
			if !ifBodyIsConnectFailedPrecondition(ifStmt.Body) {
				// Has a deps-nil-check shape but the body's not the
				// canonical FailedPrecondition return. Leave it and
				// surface for review.
				pos := fset.Position(ifStmt.Pos())
				manual = append(manual, ManualItem{
					File:   relPath,
					Line:   pos.Line,
					Reason: fmt.Sprintf("nil-check on `s.deps.%s` — body isn't the canonical FailedPrecondition return; review whether this guards an optional dep (keep) or should be moved to validateDeps (delete).", fieldName),
				})
				newStmts = append(newStmts, stmt)
				continue
			}
			// Match — drop the if-statement.
			didChange = true
			pos := fset.Position(ifStmt.Pos())
			auto = append(auto, fmt.Sprintf("%s:%d removed `if s.deps.%s == nil` guard (validateDeps now gates this dep at construction time)", relPath, pos.Line, fieldName))
			manual = append(manual, ManualItem{
				File:   filepath.ToSlash(filepath.Join(filepath.Dir(relPath), "service.go")),
				Reason: fmt.Sprintf("Confirm validateDeps() rejects nil `Deps.%s` — the codemod removed a per-RPC nil-check assuming validateDeps is the gate. If validateDeps doesn't check %s yet, add `if d.%s == nil { return fmt.Errorf(...) }` there.", fieldName, fieldName, fieldName),
			})
		}
		fn.Body.List = newStmts
	}

	if !didChange {
		return nil, manual, nil
	}

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, manual, fmt.Errorf("format rewritten handlers.go: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, manual, err
	}
	return auto, manual, nil
}

// matchDepsNilCheckCondition matches `s.deps.<Field> == nil` and
// returns the field name. ok=false if the expression doesn't match.
func matchDepsNilCheckCondition(cond ast.Expr) (string, bool) {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.EQL {
		return "", false
	}
	// Left side: s.deps.<Field>
	sel, ok := bin.X.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	parent, ok := sel.X.(*ast.SelectorExpr)
	if !ok || parent.Sel.Name != "deps" {
		return "", false
	}
	recv, ok := parent.X.(*ast.Ident)
	if !ok || recv.Name != "s" {
		return "", false
	}
	// Right side: nil
	right, ok := bin.Y.(*ast.Ident)
	if !ok || right.Name != "nil" {
		return "", false
	}
	return sel.Sel.Name, true
}

// ifBodyIsConnectFailedPrecondition returns true when block is exactly
// `{ return ..., connect.NewError(connect.CodeFailedPrecondition, ...) }`.
// We don't inspect the error message — any FailedPrecondition return
// from a nil-check is unambiguous "v0.1-style required-dep guard".
func ifBodyIsConnectFailedPrecondition(block *ast.BlockStmt) bool {
	if block == nil || len(block.List) != 1 {
		return false
	}
	ret, ok := block.List[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}
	// Walk the return values looking for a CodeFailedPrecondition
	// reference. Connect's pattern is variadic — the error may be the
	// 1st or 2nd return slot depending on the RPC's signature
	// (response+error vs error-only for streams).
	for _, v := range ret.Results {
		if exprMentionsFailedPrecondition(v) {
			return true
		}
	}
	return false
}

// exprMentionsFailedPrecondition recursively walks expr looking for a
// `connect.CodeFailedPrecondition` selector or a
// `connect.NewError(connect.CodeFailedPrecondition, ...)` call. Used
// by ifBodyIsConnectFailedPrecondition to keep the match conservative
// without becoming brittle to formatting variations.
func exprMentionsFailedPrecondition(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "CodeFailedPrecondition" {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Name != "connect" {
			return true
		}
		found = true
		return false
	})
	return found
}

// exprToString prints an ast.Expr to source. Used for human-readable
// summary lines in the CodemodReport. Errors render as "<expr>" so
// the summary stays usable even if a wild AST shape sneaks through.
func exprToString(expr ast.Expr) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, token.NewFileSet(), expr); err != nil {
		return "<expr>"
	}
	return buf.String()
}

// scanForLeftoverApplyDeps is a final pass — after the AST rewrite
// runs, we grep the project for any remaining "ApplyDeps" string
// matches (typically inside test helpers, comments, or hand-rolled
// fixtures the codemod didn't touch). Surfaces them as Manual items
// so the LLM has a complete worklist.
//
// Currently unused by the registered codemod — kept here as a hook
// for future iterations of the v0.1 migration. Wired up via
// migrateV01ToV02 if we want a "comprehensive" report; for now the
// AST rewrites cover the common case.
func scanForLeftoverApplyDeps(projectDir string) []ManualItem {
	var items []ManualItem
	_ = filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip vendored / generated dirs.
		if strings.Contains(path, "/gen/") || strings.Contains(path, "/vendor/") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		line := 0
		for scanner.Scan() {
			line++
			if strings.Contains(scanner.Text(), "ApplyDeps") {
				rel, _ := filepath.Rel(projectDir, path)
				items = append(items, ManualItem{
					File:   rel,
					Line:   line,
					Reason: "leftover ApplyDeps reference — codemod expected only Setup() top-level calls; this one needs hand removal (likely a test fixture or comment).",
				})
			}
		}
		return nil
	})
	return items
}
