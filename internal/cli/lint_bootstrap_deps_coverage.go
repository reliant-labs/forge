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

	"github.com/reliant-labs/forge/internal/codegen"
)

// lint_bootstrap_deps_coverage.go — `forge lint --bootstrap-deps-coverage`.
//
// inspectComponentDepsShape (in internal/codegen/generator.go) only
// auto-wires a package Deps field into bootstrap.go when AppExtras has
// a same-name field AND the types match EXACTLY. When the names match
// but the types diverge — for example, AppExtras.Repo is typed
// *db.PostgresRepository (the concrete DAO) but funding.Deps.Repo is
// typed funding.Repository (a narrow interface) — the wire is silently
// skipped. The package constructs with a nil Repo and the feature
// no-ops in production. This is the audit-log-silently-dropped bug
// class surfaced by the cp-forge v2 migration.
//
// This lint walks every internal/<pkg>/ that declares a Deps struct,
// checks each field name against AppExtras, and reports a finding
// when the name matches but the type doesn't. Logger and Config are
// skipped — those are handled by HasLogger / HasConfig in the
// bootstrap template.
//
// Setup.go re-construction detection: the lint also walks
// pkg/app/setup.go AST. When the user re-constructs the affected
// package in setup.go with the mismatched field explicitly assigned
// to a non-nil expression — e.g. `audit.New(audit.Deps{Repo: app.Repo})`
// — the runtime hole is closed and the finding is cleared. This makes
// the lint match runtime reality: the original hint pointed users at
// setup.go re-construction as the fix, but the previous purely-static
// check ignored it and kept reporting after they followed the advice.
//
// Why this lives in cli/ rather than internal/linter/forgeconv/:
// forgeconv is for proto-aware analyzers against user-authored source.
// This rule is a post-codegen completeness check against AppExtras +
// Deps shape — same neighborhood as runWireCoverageLint, which lives
// here for the same reason.

type bootstrapCoverageFinding struct {
	Package  string
	Field    string
	DepsType string
	AppType  string
}

// runBootstrapDepsCoverageLint reads pkg/app/app_extras.go (via
// codegen.ParseAppFields), iterates each internal/<pkg>/ that declares
// a Deps struct (via codegen.ParseServiceDeps), and reports any
// name-match-but-type-mismatch as an error. Findings cleared by
// setup.go re-construction (scanSetupReconstructions) drop out before
// reporting. Returns nil when nothing is reported.
//
// Missing pkg/app or internal/ is a no-op success — projects in early
// scaffold or library shape just have nothing to lint.
func runBootstrapDepsCoverageLint(projectDir string) error {
	fmt.Println("Running bootstrap-deps-coverage lint...")
	findings, skipReason, err := collectBootstrapCoverageFindings(projectDir)
	if err != nil {
		return err
	}
	if skipReason != "" {
		fmt.Println("  " + skipReason)
		return nil
	}

	formatBootstrapCoverage(os.Stdout, findings)
	if len(findings) > 0 {
		return fmt.Errorf("%d bootstrap-deps-coverage gap(s) — see output above", len(findings))
	}
	return nil
}

// collectBootstrapCoverageFindings computes the bootstrap-coverage gap
// set without printing — the shared engine behind
// runBootstrapDepsCoverageLint (text) and `forge lint --json`. A
// non-empty skipReason means the project has nothing to lint (missing
// pkg/app or internal/) and findings is nil.
func collectBootstrapCoverageFindings(projectDir string) (findings []bootstrapCoverageFinding, skipReason string, err error) {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if _, statErr := os.Stat(appDir); os.IsNotExist(statErr) {
		return nil, "no pkg/app — skipping", nil
	}

	appFields, err := codegen.ParseAppFields(appDir)
	if err != nil {
		return nil, "", fmt.Errorf("parse pkg/app: %w", err)
	}
	appByName := map[string]string{}
	for _, f := range appFields {
		appByName[f.Name] = f.Type
	}

	internalDir := filepath.Join(projectDir, "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "no internal/ — skipping", nil
		}
		return nil, "", fmt.Errorf("read internal: %w", err)
	}

	// Scan setup.go for re-construction patterns. Missing file is fine —
	// many projects never customize setup.go, so the wired set is empty
	// and every static mismatch reports.
	setupWired, err := scanSetupReconstructions(filepath.Join(appDir, "setup.go"))
	if err != nil {
		return nil, "", fmt.Errorf("parse pkg/app/setup.go: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkgDir := filepath.Join(internalDir, e.Name())
		deps, err := codegen.ParseServiceDeps(pkgDir)
		if err != nil || len(deps) == 0 {
			continue
		}
		for _, d := range deps {
			// Logger / Config are gated by HasLogger / HasConfig in the
			// bootstrap template; they're never name-matched against
			// AppExtras and are not part of this lint's contract.
			if d.Name == "Logger" || d.Name == "Config" {
				continue
			}
			appType, hasName := appByName[d.Name]
			if !hasName {
				continue
			}
			if appType == d.Type {
				continue // auto-wired by inspectComponentDepsShape
			}
			// Manual re-construction in setup.go closes the runtime hole.
			// The wired set is keyed by package import name (the selector
			// in `<pkg>.Deps{...}`) since that's what the AST gives us,
			// and matches the conventional internal/<dir> == <pkg> layout.
			if fields, ok := setupWired[e.Name()]; ok && fields[d.Name] {
				continue
			}
			findings = append(findings, bootstrapCoverageFinding{
				Package:  e.Name(),
				Field:    d.Name,
				DepsType: d.Type,
				AppType:  appType,
			})
		}
	}

	return findings, "", nil
}

// scanSetupReconstructions parses pkg/app/setup.go and returns the set
// of `<pkg>.Deps{ <Field>: <non-nil-expr> }` re-constructions found in
// the file. The returned map is keyed by package selector (matches the
// internal/<dir> == <pkg> conventional layout) → set of field names
// that received a non-nil value at construction time.
//
// Missing file is not an error — the caller treats it as an empty set.
//
// Detection shape: any composite literal of the form `X.Deps{...}`
// where X is an *ast.Ident (i.e. a package import name) and at least
// one keyed element names the field of interest with a non-nil value.
// Nil literals and bare `<Field>:` (zero value) do NOT count as wired
// — the entire point of the lint is to catch silently-nil deps.
//
// The detection is intentionally broad — it accepts any non-nil
// expression on the right-hand side (e.g. `app.Repo`, a local
// variable, a function call). Verifying that the expression actually
// satisfies the narrow interface is a type-check problem that go vet /
// the compiler already solves; if setup.go compiles AND the field
// receives a non-nil value, the runtime hole is closed.
func scanSetupReconstructions(setupPath string) (map[string]map[string]bool, error) {
	wired := map[string]map[string]bool{}
	src, err := os.ReadFile(setupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return wired, nil
		}
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, setupPath, src, parser.SkipObjectResolution)
	if err != nil {
		// Parse errors shouldn't sink the whole lint — the project's
		// own build will report them. Return what we have (nothing)
		// and let the static check fire its (now-honest) findings.
		return wired, nil
	}

	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Deps" {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pkgName := pkgIdent.Name
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			keyIdent, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if isNilOrZeroExpr(kv.Value) {
				continue
			}
			if wired[pkgName] == nil {
				wired[pkgName] = map[string]bool{}
			}
			wired[pkgName][keyIdent.Name] = true
		}
		return true
	})
	return wired, nil
}

// isNilOrZeroExpr returns true when the expression is a bare `nil`
// identifier. Other forms (selector exprs like `app.Repo`, function
// calls, composite literals, variables) are treated as live values —
// the point of the lint is to catch silently-nil deps, so anything
// that *might* be non-nil counts as wired.
//
// We deliberately don't try to evaluate constant expressions or chase
// variable definitions. Setup.go is small, hand-written code; the
// noise from missing a clever zero would outweigh the noise from
// accepting a paranoid wire. If setup.go compiles AND the field gets
// a non-nil value at runtime, validateDeps will catch any leftover
// gap at startup.
func isNilOrZeroExpr(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "nil"
}

// formatBootstrapCoverage writes one error per finding in the canonical
// `forge lint` shape. Empty findings print a single success line.
func formatBootstrapCoverage(w io.Writer, findings []bootstrapCoverageFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  bootstrap deps coverage clean — every name-matched AppExtras field is wired (auto or via setup.go)")
		return
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Package != findings[j].Package {
			return findings[i].Package < findings[j].Package
		}
		return findings[i].Field < findings[j].Field
	})
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ✗ [forge-bootstrap-deps-coverage] internal/%s/contract.go\n", f.Package)
		_, _ = fmt.Fprintf(w, "      %s matches AppExtras.%s by name but the types diverge\n", f.Field, f.Field)
		_, _ = fmt.Fprintf(w, "      Deps.%s        = %s\n", f.Field, f.DepsType)
		_, _ = fmt.Fprintf(w, "      AppExtras.%s   = %s\n", f.Field, f.AppType)
		_, _ = fmt.Fprintf(w, "      → align AppExtras.%s to %s, OR re-construct %s.New(%s.Deps{%s: ...}) in pkg/app/setup.go (the lint detects setup.go re-construction with a non-nil value and clears the finding)\n", f.Field, f.DepsType, f.Package, f.Package, f.Field)
	}
	_, _ = fmt.Fprintf(w, "\n%d bootstrap-deps-coverage gap(s) in pkg/app/app_extras.go.\n", len(findings))
	_, _ = fmt.Fprintln(w, "(errors — bootstrap silently drops these wires; the feature no-ops at runtime)")
}
