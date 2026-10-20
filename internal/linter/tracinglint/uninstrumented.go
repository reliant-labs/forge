// File: uninstrumented.go — forge-tracing-contract-uninstrumented.
//
// A contract package's generated decorator (middleware_gen.go) is what turns
// each Service method call into a span, so the SQL and outbound calls the
// method makes nest under it. The decorator only exists on the value
// returned by `pkg.New<Concrete>WithForgeMiddleware(pkg.New(…))`. compose.go
// is kept wrapped by `forge generate`; a construction anywhere else — a
// hand-wired provider, a worker building its own collaborator, a second
// binary — is not, and the package's methods vanish from every trace that
// goes through that value.
//
// Flagged: a call to `<alias>.New(…)` of an instrumented package (the same
// decision `forge generate` makes, supplied by the caller) from outside that
// package, whose result is neither passed straight to a …WithForgeMiddleware
// constructor nor bound to a name that the same function later passes to one
// (the fallible `x, err := pkg.New(…)` shape).

package tracinglint

import (
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/linter/finding"
)

// uninstrumentedFindings flags unwrapped constructions in f of instrumented
// packages of this module.
func uninstrumentedFindings(f *goFile, root, module string, instrumented func(dir string) bool) []finding.Finding {
	ownDir := filepath.Dir(f.abs)
	targets := map[string]string{} // local name → package dir
	for name, path := range f.imports {
		if !strings.HasPrefix(path, module+"/") {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, module+"/")))
		if dir == ownDir || !instrumented(dir) {
			continue
		}
		targets[name] = dir
	}
	if len(targets) == 0 {
		return nil
	}
	var out []finding.Finding
	forEachFunc(f.file, func(body ast.Node) {
		wrappedCalls := map[*ast.CallExpr]bool{}
		wrappedNames := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(sel.Sel.Name, middlewareConstructorSuffix) {
				return true
			}
			switch a := call.Args[0].(type) {
			case *ast.CallExpr:
				wrappedCalls[a] = true
			case *ast.Ident:
				wrappedNames[a.Name] = true
			}
			return true
		})
		// Constructions bound to a name the function later wraps.
		boundWrapped := map[*ast.CallExpr]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := as.Lhs[0].(*ast.Ident); ok && wrappedNames[id.Name] {
				boundWrapped[call] = true
			}
			return true
		})
		ast.Inspect(body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || wrappedCalls[call] || boundWrapped[call] {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			dir, ok := targets[id.Name]
			if !ok {
				return true
			}
			wrapper := id.Name + "." + decoratorName(dir) + "(" + id.Name + ".New(…))"
			out = append(out, finding.Finding{
				Rule:     RuleContractUninstrumented,
				Severity: finding.SeverityError,
				File:     f.rel,
				Line:     f.line(call.Pos()),
				Message: fmt.Sprintf("%s.New constructs the %s contract without its generated decorator, so its methods open no span and the work under them is parentless in the trace",
					id.Name, filepath.Base(dir)),
				Remediation: "wrap it: " + wrapper + " — or opt the package out with a package-level `// forge:no-observe`",
			})
			return true
		})
	})
	return out
}

var decoratorRE = regexp.MustCompile(`(?m)^func (New\w*` + middlewareConstructorSuffix + `)\(`)

// memoize caches the instrument decision per package dir for one run.
func memoize(instrumented func(string) bool) func(string) bool {
	memo := map[string]bool{}
	return func(dir string) bool {
		v, ok := memo[dir]
		if !ok {
			v = instrumented(dir)
			memo[dir] = v
		}
		return v
	}
}

// decoratorName is the wrapper constructor middleware_gen.go declares, or the
// canonical name when it cannot be read.
func decoratorName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "middleware_gen.go"))
	if err == nil {
		if m := decoratorRE.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return "NewService" + middlewareConstructorSuffix
}
