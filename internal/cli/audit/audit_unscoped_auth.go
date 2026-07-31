// File: internal/cli/audit/audit_unscoped_auth.go
//
// The `unscoped_auth` audit category: an RPC whose proto declares
// auth_required: true, whose handler body never resolves the caller.
//
// Why this is a category and not a comment
//
// The CRUD shim template already says the thing. Above every delegating
// method it emits, verbatim:
//
//	AUTHENTICATED, UNSCOPED: this RPC's proto declares auth_required: true,
//	so the interceptor rejected any caller without a valid token before this
//	ran. Nothing below reads WHO they are — call middleware.GetUser(ctx) above
//	the delegation and scope the rows it touches to those claims.
//
// A measured run read that comment, wrote a plan that repeated it back
// ("later work can add authorization/row scoping in owned CRUD
// delegations"), and shipped sixteen delegations that read no caller. One
// signed-in user could read, list, update and delete another user's rows;
// the update path let them reassign a row to themselves outright. Every
// gate the run ran was green, because a comment cannot fail. That is the
// defect this file addresses: not that forge failed to diagnose the hole,
// but that its diagnosis was unfalsifiable.
//
// What it derives from
//
// Both sides of the comparison are computed by the producer, never
// grepped out of rendered prose:
//
//   - The AUTHENTICATED set comes from gen/forge_descriptor.json —
//     protoc-gen-forge's own projection of (forge.v1.method).auth_required,
//     the same field that drives pkg/middleware/procedures_gen.go and the
//     interceptor's fail-closed set. Reading it through
//     codegen.ParseServicesFromProtos means this category and the
//     interceptor can never disagree about which RPCs are authenticated.
//   - The READS-THE-CALLER set comes from the go/ast of the user-owned
//     handler files, resolved through the SAME auth seam the scaffold
//     names (codegen.CRUDAuthSeamFunc / CRUDAuthSeamPkg) plus the claims
//     accessors that seam re-exports. Renaming the seam moves this check
//     with it; nothing here hardcodes the string "GetUser".
//
// A handler counts as reading the caller if its body reaches the seam
// directly, OR passes something derived from it into a callee, OR calls a
// helper in its own package that itself reaches the seam (one hop —
// enough for the common `s.requireOwner(ctx)` shape without pretending to
// do interprocedural analysis).
//
// False positives cost more than false negatives here
//
// A legitimately global authenticated RPC exists: an admin list, a lookup
// already keyed by something caller-scoped, an RPC whose scoping lives in
// a row-level-security policy. Those are not defects and must not be
// reported forever, so the author can say so IN CODE:
//
//	// forge:auth-unscoped-ok: operator console list; every caller of this
//	// RPC is an admin, so there is no narrower scope to apply.
//	func (s *Service) ListAuditEvents(...)
//
// A directive with no reason after the colon does not count — an
// acknowledgement that says nothing is the comment problem again. It
// lives in the handler file rather than a config file specifically so it
// shows up in the diff that introduces it, next to the code it excuses.
//
// Status is warn, never error
//
// A freshly-scaffolded project is ENTIRELY unscoped by construction: forge
// emits delegations, the user writes the scoping. Erroring would make
// forge's own output fail forge's own gate, which is the invariant
// TestE2EFreshScaffoldLintExitsZero pins. warn surfaces the set without
// making a green birth impossible. Where it bites is later — the audit a
// reviewer or a workflow phase reads after handlers are written.

package audit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/cli/audittype"
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// AuthUnscopedOKDirective is the in-code acknowledgement that an
// authenticated RPC is intentionally global. It must be followed by a
// reason: the directive alone re-creates the unfalsifiable comment this
// category exists to replace.
const AuthUnscopedOKDirective = "forge:auth-unscoped-ok:"

// unscopedRPC is one authenticated RPC whose handler never reaches the
// auth seam.
type unscopedRPC struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	File    string `json:"file"`
	// Delegating is true when the body is a bare forge CRUD delegation
	// (the shape the scaffold emits and the run shipped unchanged).
	// Consumers can use it to separate "never touched" from "written by
	// hand and still unscoped".
	Delegating bool `json:"delegating"`
}

// acknowledgedRPC is one authenticated RPC the author has explicitly
// declared global, with the reason they gave.
type acknowledgedRPC struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	File    string `json:"file"`
	Reason  string `json:"reason"`
}

// auditUnscopedAuth reports authenticated RPCs whose handlers never
// resolve the caller.
//
// It is a no-op (status ok, explicit summary) for any project that has no
// Connect services, no descriptor, or no handler tree — a worker-only
// project, a CLI, a library. Those are not "clean", they are not subject,
// and the summary says which.
func auditUnscopedAuth(projectDir string) audittype.Category {
	services, err := codegen.ParseServicesFromProtos("", projectDir)
	if err != nil {
		return audittype.Category{
			Status:  audittype.StatusWarn,
			Summary: fmt.Sprintf("could not read the forge descriptor: %v", err),
			Details: map[string]any{"hint": fmt.Sprintf("run `%s generate` to produce gen/forge_descriptor.json", cmdutil.Name())},
		}
	}
	if len(services) == 0 {
		return audittype.Category{
			Status:  audittype.StatusOK,
			Summary: "no Connect services declared (n/a)",
			Details: map[string]any{"authenticated_rpcs": 0},
		}
	}

	handlersRoot := filepath.Join(projectDir, "internal", "handlers")
	if !dirExists(handlersRoot) {
		return audittype.Category{
			Status:  audittype.StatusOK,
			Summary: "no internal/handlers/ directory (n/a)",
			Details: map[string]any{"authenticated_rpcs": 0},
		}
	}

	var (
		unscoped     []unscopedRPC
		acknowledged []acknowledgedRPC
		authTotal    int
		scopedTotal  int
		// declaredAuth counts authenticated RPCs the DESCRIPTOR knows
		// about, independent of whether a handler was found for them.
		// The gap between it and authTotal is how this category detects
		// that its own derivation broke — see the guard below.
		declaredAuth   int
		unresolvedSvcs []string
	)

	for _, svc := range services {
		// Only RPCs the proto declares authenticated are in scope. A
		// public RPC reading no claims is correct by construction, and
		// the scaffold says so in its own PUBLIC branch.
		authMethods := map[string]bool{}
		for _, m := range svc.Methods {
			if m.AuthRequired {
				authMethods[m.Name] = true
			}
		}
		if len(authMethods) == 0 {
			continue
		}
		declaredAuth += len(authMethods)

		// The descriptor names the PROTO service ("StorefrontService");
		// the handler package on disk is naming.ServicePackage's form of
		// it ("storefront"), which is what the scaffolders create. Going
		// through naming here — rather than lowercasing the proto name —
		// is why a multi-word service (AdminServerService →
		// internal/handlers/admin_server) resolves at all.
		dir, derr := codegen.ResolveComponentDir(projectDir, "internal/handlers", naming.ServicePackage(svc.Name))
		if derr != nil || !dirExists(dir.Dir) {
			// The service declares authenticated RPCs but has no handler
			// directory on disk yet (pre-scaffold). Nothing to inspect.
			unresolvedSvcs = append(unresolvedSvcs, svc.Name)
			continue
		}

		handlers, herr := scanHandlerAuthUse(dir.Dir)
		if herr != nil {
			unresolvedSvcs = append(unresolvedSvcs, svc.Name)
			continue
		}

		for name := range authMethods {
			h, ok := handlers[name]
			if !ok {
				// No handler method for this RPC in the user-owned tree —
				// an unwired stub, or a method forge has not scaffolded.
				// orphan_stubs owns that condition; this category does not
				// double-report it.
				continue
			}
			authTotal++
			rel := relPath(projectDir, h.File)
			switch {
			case h.AckReason != "":
				acknowledged = append(acknowledged, acknowledgedRPC{
					Service: svc.Name, Method: name, File: rel, Reason: h.AckReason,
				})
			case h.ReadsCaller:
				scopedTotal++
			default:
				unscoped = append(unscoped, unscopedRPC{
					Service: svc.Name, Method: name, File: rel, Delegating: h.Delegating,
				})
			}
		}
	}

	sortUnscoped(unscoped)
	sortAcknowledged(acknowledged)

	seam := codegen.CRUDAuthSeam()
	details := map[string]any{
		"authenticated_rpcs": authTotal,
		"scoped_rpcs":        scopedTotal,
		"unscoped_rpcs":      unscoped,
		"acknowledged_rpcs":  acknowledged,
		"auth_seam":          seam,
		"acknowledge_marker": AuthUnscopedOKDirective,
		"hint": fmt.Sprintf(
			"resolve the caller with %s(ctx) and scope what the handler touches to those claims; "+
				"if an RPC is intentionally global, say so in code above it with `// %s <reason>`",
			seam, AuthUnscopedOKDirective),
	}

	// FAIL LOUDLY ON AN EMPTY DERIVATION. The project declares
	// authenticated RPCs, and this category matched a handler for NONE of
	// them — so every assertion below would pass vacuously and the audit
	// would report a clean auth surface it never actually looked at.
	//
	// This is not hypothetical: the first cut of this file resolved the
	// handler directory from the descriptor's proto service name
	// ("StorefrontService") instead of naming.ServicePackage's on-disk
	// form ("storefront"). It found zero handlers and reported
	// `"status": "ok"` against the very project whose IDOR it was written
	// to catch. A silent zero is the exact failure mode this category
	// exists to eliminate, so it reports warn and says what broke.
	if declaredAuth > 0 && authTotal == 0 {
		sort.Strings(unresolvedSvcs)
		details["declared_authenticated_rpcs"] = declaredAuth
		details["unresolved_services"] = unresolvedSvcs
		return audittype.Category{
			Status: audittype.StatusWarn,
			Summary: fmt.Sprintf(
				"%d authenticated RPC(s) declared, but no handler was matched for ANY of them — this check inspected nothing (services: %s)",
				declaredAuth, strings.Join(unresolvedSvcs, ", ")),
			Details: details,
		}
	}

	if len(unscoped) == 0 {
		summary := fmt.Sprintf("all %d authenticated RPC(s) resolve the caller", authTotal)
		if authTotal == 0 {
			summary = "no authenticated RPCs with handlers to inspect (n/a)"
		}
		return audittype.Category{Status: audittype.StatusOK, Summary: summary, Details: details}
	}

	return audittype.Category{
		Status: audittype.StatusWarn,
		// Deliberately domain-neutral: this category also fires on
		// streaming and batch RPCs that touch no rows at all, and a
		// summary that says "rows" would read as a CRUD-only finding.
		Summary: fmt.Sprintf(
			"%d of %d authenticated RPC(s) never resolve the caller — every signed-in caller is treated identically (%s)",
			len(unscoped), authTotal, seam),
		Details: details,
	}
}

func sortUnscoped(v []unscopedRPC) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].Service != v[j].Service {
			return v[i].Service < v[j].Service
		}
		return v[i].Method < v[j].Method
	})
}

func sortAcknowledged(v []acknowledgedRPC) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].Service != v[j].Service {
			return v[i].Service < v[j].Service
		}
		return v[i].Method < v[j].Method
	})
}

func relPath(projectDir, path string) string {
	rel, err := filepath.Rel(projectDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// handlerAuthUse is what the AST pass learned about one handler method.
type handlerAuthUse struct {
	File        string
	ReadsCaller bool
	Delegating  bool
	AckReason   string
}

// scanHandlerAuthUse parses every non-test .go file in a handler package
// and reports, per method on the service receiver, whether its body
// reaches the auth seam.
//
// Two passes: the first records which package-level funcs/methods reach
// the seam directly, the second re-checks each handler allowing one hop
// through those helpers. That is deliberately shallow — it catches the
// `s.callerOrders(ctx)` factoring without claiming to be a call-graph
// analysis, and a deeper indirection reports as unscoped, which is the
// safe direction for a warn-level category the author can acknowledge.
func scanHandlerAuthUse(dir string) (map[string]handlerAuthUse, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	type parsedFile struct {
		path string
		file *ast.File
	}
	var files []parsedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			// A handler package that does not parse is a build problem,
			// not an auth finding. Skip the file rather than guess.
			continue
		}
		files = append(files, parsedFile{path: path, file: f})
	}

	// Pass 1: which funcs in this package touch the seam directly?
	seamReaching := map[string]bool{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if bodyReachesAuthSeam(fn.Body) {
				seamReaching[fn.Name.Name] = true
			}
		}
	}

	out := map[string]handlerAuthUse{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			if !fn.Name.IsExported() {
				continue
			}
			use := handlerAuthUse{
				File:       pf.path,
				Delegating: bodyIsCRUDDelegation(fn.Body),
				AckReason:  ackReason(fn.Doc),
			}
			use.ReadsCaller = bodyReachesAuthSeam(fn.Body) ||
				bodyCallsSeamReachingHelper(fn.Body, seamReaching, fn.Name.Name)
			out[fn.Name.Name] = use
		}
	}
	return out, nil
}

// ackReason extracts the reason from a `forge:auth-unscoped-ok:` doc
// comment. Returns "" when the directive is absent OR carries no reason —
// a bare directive is not an acknowledgement, it is the unfalsifiable
// comment again.
func ackReason(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	for _, c := range doc.List {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Text), "//"))
		rest, ok := strings.CutPrefix(text, AuthUnscopedOKDirective)
		if !ok {
			continue
		}
		if reason := strings.TrimSpace(rest); reason != "" {
			return reason
		}
	}
	return ""
}

// bodyReachesAuthSeam reports whether a function body names the auth seam
// or one of the claims accessors it re-exports. The identifiers come from
// codegen, which is what stamps the seam into the scaffold — renaming it
// there moves this check.
func bodyReachesAuthSeam(body *ast.BlockStmt) bool {
	seamFuncs := codegen.CRUDAuthSeamFuncs()
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if seamFuncs[sel.Sel.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}

// bodyCallsSeamReachingHelper reports whether the body calls a function in
// the same package that itself reaches the seam. Self-recursion is
// excluded so a method cannot vouch for itself.
func bodyCallsSeamReachingHelper(body *ast.BlockStmt, seamReaching map[string]bool, self string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			// s.helper(...) — the receiver-method factoring.
			name = fn.Sel.Name
		}
		if name != "" && name != self && seamReaching[name] {
			found = true
			return false
		}
		return true
	})
	return found
}

// bodyIsCRUDDelegation reports whether a body is exactly the shape the
// CRUD shim template emits: a single return of a crud.Handle*(...) call.
// It is metadata on the finding, never the finding itself — a hand-written
// unscoped handler is just as exposed as a delegating one.
func bodyIsCRUDDelegation(body *ast.BlockStmt) bool {
	if len(body.List) != 1 {
		return false
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	outer, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	inner, ok := outer.Fun.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == codegen.CRUDDelegatePkgName() && strings.HasPrefix(sel.Sel.Name, "Handle")
}
