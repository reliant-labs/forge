// File: internal/linter/forgeconv/no_handler_error_mapping.go
//
// The forgeconv-no-handler-error-mapping analyzer warns when a handler
// package re-rolls the canonical service-error → connect.Error switch
// statement that ships in forge/pkg/svcerr.
//
// Why this exists
//
// The 2026-05-06 dogfood pass of control-plane-next surfaced 4
// byte-identical copies of `toConnectError` in
// handlers/{billing,daemon,llm_gateway,org}/handlers.go. Each copy
// switched on the same internal/svcerr.Err* sentinels and mapped to the
// same connect.Code values. The api/handlers skill at the time
// prescribed the per-service helper, so the duplication was earned —
// not a sloppiness signal but a convention signal. Pairing the new
// pkg/svcerr ship with this lint nudge prevents the next project from
// repeating the pattern.
//
// Heuristic
//
// We flag any function/method declared in a Go file under handlers/
// (or under any directory whose package name suggests handler code)
// that looks like a hand-rolled error-mapping helper. Three signals:
//
//   1. Function name matches a known mapping-helper name (mapServiceError,
//      toConnectError, errToConnect, mapErr, etc.).
//   2. Body constructs `connect.NewError` AND switches on errors.Is /
//      errors.As against multiple sentinels.
//
// Either signal alone is suspicious; combined they are the duplication
// pattern this rule targets. Severity is warning (not error): the
// false-positive risk is non-trivial — some projects legitimately need
// custom mapping for project-specific sentinels — and the cost of a
// stray warning is low.
//
// The remediation message points at `svcerr.Wrap(err)` so the LLM has
// the canonical replacement on screen at the moment of friction.

package forgeconv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LintHandlerErrorMapping walks rootDir for Go files under handlers/
// directories and flags hand-rolled service-error → connect.Error
// helpers. Returns findings in deterministic order.
//
// The walk recognises any directory whose path component is exactly
// "handlers" as the canonical handler tree (matches both the
// project-template `handlers/<svc>/` shape and the rare `handlers/`
// flat layout). Test files (_test.go) are skipped — fixtures and table
// tests sometimes legitimately construct connect errors for assertions.
func LintHandlerErrorMapping(rootDir string) (Result, error) {
	var goFiles []string
	walkErr := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip generated / vendored / tooling subtrees outright so the
			// analyzer stays cheap on real projects.
			base := d.Name()
			if base == "gen" || base == "node_modules" || base == ".git" ||
				base == "testdata" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Path-based filter: only files under a `handlers/` directory.
		// Use slash-form for cross-platform comparison.
		slashPath := filepath.ToSlash(path)
		if !strings.Contains(slashPath, "/handlers/") && !strings.HasPrefix(slashPath, "handlers/") {
			return nil
		}
		goFiles = append(goFiles, path)
		return nil
	})
	if walkErr != nil {
		return Result{}, fmt.Errorf("walk %s: %w", rootDir, walkErr)
	}

	sort.Strings(goFiles)

	var result Result
	for _, p := range goFiles {
		rel, relErr := filepath.Rel(rootDir, p)
		if relErr != nil {
			rel = p
		}
		findings, err := lintHandlerErrorMappingFile(rel, p)
		if err != nil {
			// A parse failure shouldn't blank the report; emit a warning so
			// the user knows the file was skipped.
			result.Findings = append(result.Findings, Finding{
				Rule:     "forgeconv-no-handler-error-mapping",
				Severity: SeverityWarning,
				File:     rel,
				Message:  fmt.Sprintf("could not parse handler file: %v", err),
			})
			continue
		}
		result.Findings = append(result.Findings, findings...)
	}

	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].File != result.Findings[j].File {
			return result.Findings[i].File < result.Findings[j].File
		}
		if result.Findings[i].Line != result.Findings[j].Line {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].Rule < result.Findings[j].Rule
	})

	return result, nil
}

// suspectMapperNames lists the function names the dogfood pass found
// (and a few likely siblings). The check is an OR with the body-shape
// heuristic; either signal triggers, both signals together strengthen
// the message.
var suspectMapperNames = map[string]bool{
	"mapServiceError": true,
	"toConnectError":  true,
	"errToConnect":    true,
	"mapErr":          true,
	"mapError":        true,
	"asConnectError":  true,
	"toConnectErr":    true,
	"connectErrorOf":  true,
}

// lintHandlerErrorMappingFile parses one handler-tree Go file and
// flags candidate mapping helpers. Returns at most one finding per
// candidate function — no per-line spam.
func lintHandlerErrorMappingFile(relPath, absPath string) ([]Finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	// Track whether the file imports svcerr already — if so, the user
	// is already on the canonical path; suppress findings (a hand-rolled
	// helper sitting next to svcerr.Wrap would surface as a separate
	// signal during code review).
	if importsSvcerr(file) {
		return nil, nil
	}

	var findings []Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		nameSuspect := suspectMapperNames[fn.Name.Name]
		bodySuspect, sentinelCount := bodyLooksLikeMapper(fn)

		if !nameSuspect && !bodySuspect {
			continue
		}

		pos := fset.Position(fn.Pos())
		message := mappingFindingMessage(fn.Name.Name, nameSuspect, bodySuspect, sentinelCount)

		findings = append(findings, Finding{
			Rule:     "forgeconv-no-handler-error-mapping",
			Severity: SeverityWarning,
			File:     relPath,
			Line:     pos.Line,
			Message:  message,
			Remediation: "delete this helper and use `svcerr.Wrap(err)` from `github.com/reliant-labs/forge/pkg/svcerr`. " +
				"Define your domain sentinels as `svcerr.ErrNotFound` etc. (or wrap them via `svcerr.NotFound(\"user\")`) " +
				"in `internal/<pkg>/contract.go`; the handler then becomes one line: `return nil, svcerr.Wrap(err)`. " +
				"See skill: api/handlers.",
		})
	}

	return findings, nil
}

// importsSvcerr reports whether the file imports forge/pkg/svcerr.
// Suppression heuristic: if the file is already on the canonical path,
// any leftover helper is intentional (or in-flight migration) — the
// rule should not double-warn.
func importsSvcerr(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, "\"")
		if strings.HasSuffix(path, "/forge/pkg/svcerr") || strings.HasSuffix(path, "/pkg/svcerr") {
			return true
		}
	}
	return false
}

// bodyLooksLikeMapper inspects the function body for the canonical
// "switch on errors.Is(err, sentinel) → connect.NewError(connect.CodeX, err)"
// shape. Returns (true, sentinelCount) when the body matches with at
// least two distinct sentinel arms. The threshold is intentionally
// modest — a one-arm switch could legitimately be a domain-specific
// translation, but a two-or-more-arm switch on errors.Is paired with
// connect.NewError construction is the duplication pattern.
func bodyLooksLikeMapper(fn *ast.FuncDecl) (bool, int) {
	if fn.Body == nil {
		return false, 0
	}

	var (
		sawConnectNewError bool
		sentinelArms       int
		sawErrorsIs        bool
	)

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if isQualifiedCall(x.Fun, "connect", "NewError") {
				sawConnectNewError = true
			}
			if isQualifiedCall(x.Fun, "errors", "Is") || isQualifiedCall(x.Fun, "errors", "As") {
				sawErrorsIs = true
				sentinelArms++
			}
		}
		return true
	})

	if !sawConnectNewError {
		return false, sentinelArms
	}
	if !sawErrorsIs {
		return false, sentinelArms
	}
	if sentinelArms < 2 {
		return false, sentinelArms
	}
	return true, sentinelArms
}

// isQualifiedCall reports whether expr is a selector of the form
// `pkg.fn`. Used to spot connect.NewError / errors.Is without dragging
// in a full type-checker.
func isQualifiedCall(expr ast.Expr, pkg, fn string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel == nil || sel.Sel.Name != fn {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == pkg
}

// mappingFindingMessage composes a context-aware diagnostic — the
// LLM-readable hint scales with how confident the rule is.
func mappingFindingMessage(name string, nameSuspect, bodySuspect bool, arms int) string {
	switch {
	case nameSuspect && bodySuspect:
		return fmt.Sprintf(
			"function %q switches on %d sentinel(s) and constructs connect.NewError — "+
				"this is the per-service mapping helper that pkg/svcerr replaces. "+
				"Use `svcerr.Wrap(err)` instead.",
			name, arms)
	case nameSuspect:
		return fmt.Sprintf(
			"function %q has the canonical service-error mapper name — "+
				"projects on forge >=1.7 use `svcerr.Wrap(err)` from forge/pkg/svcerr instead.",
			name)
	default:
		return fmt.Sprintf(
			"function %q switches on %d sentinel(s) and constructs connect.NewError — "+
				"this looks like a hand-rolled service-error mapper. "+
				"Use `svcerr.Wrap(err)` from forge/pkg/svcerr.",
			name, arms)
	}
}
