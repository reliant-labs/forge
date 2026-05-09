// File: internal/linter/forgeconv/internal_pkg_contract.go
//
// The forgeconv-internal-package-contract-names analyzer enforces the
// canonical naming convention every internal-package contract.go must
// follow:
//
//   - `type Service interface { ... }`
//   - `type Deps struct { ... }`
//   - `func New(Deps) Service` OR `func New(Deps) (Service, error)`
//
// The bootstrap codegen template (internal/templates/project/bootstrap.go.tmpl)
// hardcodes references to `<pkg>.Service`, `<pkg>.Deps`, and `<pkg>.New(...)`
// for every entry under `Packages`. When a contract.go uses a different
// interface name (e.g. `Sender`, `Manager`, `Handler`) the generated bootstrap
// references types that don't exist and the project fails to compile —
// with errors pointing at generated code the user shouldn't touch.
//
// This analyzer surfaces the failure mode as an explicit lint finding before
// `forge generate` writes the broken bootstrap.
//
// The two-result form `(Service, error)` was introduced in Day-5 polish
// alongside `validateDeps()` so the bootstrap can surface required-Deps
// gaps once at startup instead of forcing per-RPC nil-checks. The
// scaffold templates emit the two-result form for new packages; the
// single-result form remains accepted so pre-Day-5 packages continue to
// pass lint until they're refactored.

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

// LintInternalContracts walks rootDir/internal/ for contract.go files and
// asserts the canonical Service/Deps/New(Deps) Service shape. The excludes
// argument carries module-relative directory paths (matching forge.yaml
// `contracts.exclude`) that the walk must skip wholesale: those packages
// are not bootstrap-managed (analyzer sub-packages, embed-only packages,
// the cli surface itself), and their contract.go files are allowed to
// declare alternate shapes.
//
// Returns findings in deterministic order (file, then position).
func LintInternalContracts(rootDir string, excludes []string) (Result, error) {
	internalDir := filepath.Join(rootDir, "internal")
	if _, err := os.Stat(internalDir); os.IsNotExist(err) {
		// No internal/ — nothing to check. Common in CLI/library projects.
		return Result{}, nil
	}

	// Mirror config.ContractsConfig.IsExcluded semantics: equality, suffix
	// match against `/pattern`, or substring match. Keeps the two surfaces
	// (lint and bootstrap-skip) in sync without depending on the config
	// package (the analyzer must stay importable from anywhere).
	isExcluded := func(relSlash string) bool {
		for _, pat := range excludes {
			pat = filepath.ToSlash(pat)
			if pat == "" {
				continue
			}
			if pat == relSlash || strings.HasSuffix(relSlash, "/"+pat) || strings.Contains(relSlash, pat) {
				return true
			}
		}
		return false
	}

	// Collect package directories that contain a contract.go. The shape
	// check itself runs across every non-test .go file in the package
	// (Service typically lives in contract.go, Deps + New typically live
	// in service.go / adapter.go / interactor.go / client.go — the
	// `--kind=client` and `--type=adapter|interactor` scaffolds use this
	// split). The lint message points at contract.go since that's the
	// canonical place to document the boundary.
	var pkgDirs []string
	walkErr := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// Skip testdata/ subtrees — fixture contracts, not real packages.
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		// Honor excludes (matched on the module-relative slash path).
		rel, relErr := filepath.Rel(rootDir, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		if isExcluded(relSlash) {
			return filepath.SkipDir
		}
		contractPath := filepath.Join(path, "contract.go")
		if _, statErr := os.Stat(contractPath); statErr == nil {
			pkgDirs = append(pkgDirs, path)
		}
		return nil
	})
	if walkErr != nil {
		return Result{}, fmt.Errorf("walk %s: %w", internalDir, walkErr)
	}

	sort.Strings(pkgDirs)

	var result Result
	for _, dir := range pkgDirs {
		contractPath := filepath.Join(dir, "contract.go")
		relContract, relErr := filepath.Rel(rootDir, contractPath)
		if relErr != nil {
			relContract = contractPath
		}
		findings, err := lintInternalContractPackage(relContract, dir)
		if err != nil {
			result.Findings = append(result.Findings, Finding{
				Rule:     "forgeconv-internal-package-contract-names",
				Severity: SeverityError,
				File:     relContract,
				Message:  fmt.Sprintf("failed to parse package: %v", err),
				Remediation: "fix the syntax error and re-run; the analyzer needs parseable .go files " +
					"to verify Service/Deps/New(Deps) Service",
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

// lintInternalContractPackage walks every non-test .go file in pkgDir
// and asserts the canonical Service/Deps/New(Deps) Service shape lives
// somewhere in the package — not necessarily all in contract.go.
// Pre-2026-05-06 the rule required all three in contract.go itself,
// which false-positived on packages that split Service into contract.go
// and Deps + New into a sibling file (the `--kind=client` and
// `--type=adapter|interactor` scaffolds both do this). The fix moves
// detection to package scope while keeping findings anchored on
// contract.go (so the user knows where the contract is documented).
//
// Returns one finding per missing/wrong piece so a completely-renamed
// contract surfaces all three at once (Service / Deps / New) rather
// than drip-feeding one violation per re-run.
func lintInternalContractPackage(relContractPath, pkgDir string) ([]Finding, error) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pkgDir, err)
	}

	fset := token.NewFileSet()
	var (
		findings            []Finding
		hasServiceInterface bool
		hasDepsStruct       bool
		hasNewFunc          bool
		// Capture the first non-canonical interface and struct names so
		// the error message can be specific ("found 'Sender'/'Config'/'NewSender'").
		firstIfaceName, firstIfacePos   = "", token.Position{}
		firstStructName, firstStructPos = "", token.Position{}
		firstCtorName, firstCtorPos     = "", token.Position{}
	)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// Skip _test.go — tests can declare their own helper structs
		// without participating in the package's contract surface.
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// Skip generated files — `mock_gen.go` and friends are the
		// codegen output OF the contract and shouldn't gate it.
		if strings.HasSuffix(e.Name(), "_gen.go") {
			continue
		}
		fp := filepath.Join(pkgDir, e.Name())
		file, parseErr := parser.ParseFile(fset, fp, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			// Surface the parse error, but continue scanning other
			// files in the package so the user sees the full picture.
			return nil, parseErr
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					switch ts.Type.(type) {
					case *ast.InterfaceType:
						if ts.Name.Name == "Service" {
							hasServiceInterface = true
						} else if firstIfaceName == "" {
							firstIfaceName = ts.Name.Name
							firstIfacePos = fset.Position(ts.Pos())
						}
					case *ast.StructType:
						if ts.Name.Name == "Deps" {
							hasDepsStruct = true
						} else if firstStructName == "" {
							firstStructName = ts.Name.Name
							firstStructPos = fset.Position(ts.Pos())
						}
					}
				}
			case *ast.FuncDecl:
				// Only top-level (no receiver) functions count as the
				// constructor candidate; methods on the impl struct are fine.
				if d.Recv != nil {
					continue
				}
				if d.Name.Name == "New" && isNewDepsServiceSignature(d.Type) {
					hasNewFunc = true
				} else if firstCtorName == "" && strings.HasPrefix(d.Name.Name, "New") {
					firstCtorName = d.Name.Name
					firstCtorPos = fset.Position(d.Name.NamePos)
				}
			}
		}
	}

	// Findings are reported against contract.go (canonical anchor)
	// even when the actually-missing declaration would live in
	// service.go — the user reads contract.go to understand the
	// package boundary, so that's where we point them.
	relPath := relContractPath

	// Synthesize a canonical actionable error message per missing piece.
	// Keep the wording uniform so users can grep the codebase for it.
	const canonical = "internal-package contracts must declare 'type Service interface', " +
		"'type Deps struct', and 'func New(Deps) Service'"

	if !hasServiceInterface {
		var line int
		var found string
		if firstIfaceName != "" {
			line = firstIfacePos.Line
			found = fmt.Sprintf("'%s'", firstIfaceName)
		} else {
			line = 1
			found = "no interface"
		}
		findings = append(findings, Finding{
			Rule:     "forgeconv-internal-package-contract-names",
			Severity: SeverityError,
			File:     relPath,
			Line:     line,
			Message: fmt.Sprintf(
				"forge convention: %s. Found %s — rename to 'Service' (or move out of contract.go) so the bootstrap template can wire it. See skill: contracts.",
				canonical, found),
			Remediation: "rename the interface declaration to `type Service interface { ... }`, " +
				"or move it to a non-contract.go file if it's not the package's primary behavioral surface",
		})
	}

	if !hasDepsStruct {
		var line int
		var found string
		if firstStructName != "" {
			line = firstStructPos.Line
			found = fmt.Sprintf("'%s'", firstStructName)
		} else {
			line = 1
			found = "no struct"
		}
		findings = append(findings, Finding{
			Rule:     "forgeconv-internal-package-contract-names",
			Severity: SeverityError,
			File:     relPath,
			Line:     line,
			Message: fmt.Sprintf(
				"forge convention: %s. Found %s — rename to 'Deps' (or move out of contract.go) so the bootstrap template can wire it. See skill: contracts.",
				canonical, found),
			Remediation: "rename the dependency-set struct to `type Deps struct { ... }` (use `struct{}` if no deps yet)",
		})
	}

	if !hasNewFunc {
		var line int
		var found string
		if firstCtorName != "" {
			line = firstCtorPos.Line
			found = fmt.Sprintf("'%s'", firstCtorName)
		} else {
			line = 1
			found = "no constructor"
		}
		findings = append(findings, Finding{
			Rule:     "forgeconv-internal-package-contract-names",
			Severity: SeverityError,
			File:     relPath,
			Line:     line,
			Message: fmt.Sprintf(
				"forge convention: %s. Found %s — rename to 'New' with signature `func New(Deps) Service` so the bootstrap template can wire it. See skill: contracts.",
				canonical, found),
			Remediation: "rename the constructor to `func New(Deps) Service` (the bootstrap template emits `<pkg>.New(<pkg>.Deps{...})`)",
		})
	}

	return findings, nil
}

// isNewDepsServiceSignature reports whether ft has either of the canonical
// `func(Deps) Service` shapes:
//
//   - `func New(Deps) Service` — pre-Day-5 single-result form
//   - `func New(Deps) (Service, error)` — Day-5+ form with validateDeps()
//
// We don't insist on a parameter name; both `func New(Deps) Service` and
// `func New(d Deps) Service` are accepted. We DO insist on:
//   - exactly one parameter, of type `Deps` (unqualified — same package)
//   - one result of type `Service`, optionally followed by a second
//     result of type `error` (unqualified)
//
// Pointer parameters (`func New(*Deps) Service`) are intentionally rejected —
// the bootstrap template emits `<pkg>.New(<pkg>.Deps{...})` (a value), so
// a pointer receiver shape would compile-fail at the call site.
func isNewDepsServiceSignature(ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil || ft.Results == nil {
		return false
	}
	// Sum names for parameter count: `func(a, b Deps)` declares two even
	// though they share one Field.
	paramCount := 0
	for _, p := range ft.Params.List {
		n := len(p.Names)
		if n == 0 {
			n = 1 // anonymous parameter still counts as one
		}
		paramCount += n
	}
	if paramCount != 1 {
		return false
	}

	// Flatten results into a slice of types so we can pattern-match
	// (Service) vs (Service, error). `func() (a, b T)` shares one Field
	// for both names; treat each name as a distinct result.
	var resultTypes []ast.Expr
	for _, r := range ft.Results.List {
		n := len(r.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			resultTypes = append(resultTypes, r.Type)
		}
	}
	if len(resultTypes) < 1 || len(resultTypes) > 2 {
		return false
	}

	if !isIdent(ft.Params.List[0].Type, "Deps") {
		return false
	}
	if !isIdent(resultTypes[0], "Service") {
		return false
	}
	if len(resultTypes) == 2 && !isIdent(resultTypes[1], "error") {
		return false
	}
	return true
}

// isIdent returns true iff expr is an unqualified identifier with the given name.
func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == name
}
