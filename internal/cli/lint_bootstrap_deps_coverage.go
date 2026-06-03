package cli

import (
	"fmt"
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
// name-match-but-type-mismatch as an error. Returns nil when nothing
// is reported.
//
// Missing pkg/app or internal/ is a no-op success — projects in early
// scaffold or library shape just have nothing to lint.
func runBootstrapDepsCoverageLint(projectDir string) error {
	fmt.Println("Running bootstrap-deps-coverage lint...")
	appDir := filepath.Join(projectDir, "pkg", "app")
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		fmt.Println("  no pkg/app — skipping")
		return nil
	}

	appFields, err := codegen.ParseAppFields(appDir)
	if err != nil {
		return fmt.Errorf("parse pkg/app: %w", err)
	}
	appByName := map[string]string{}
	for _, f := range appFields {
		appByName[f.Name] = f.Type
	}

	internalDir := filepath.Join(projectDir, "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("  no internal/ — skipping")
			return nil
		}
		return fmt.Errorf("read internal: %w", err)
	}

	var findings []bootstrapCoverageFinding
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
			findings = append(findings, bootstrapCoverageFinding{
				Package:  e.Name(),
				Field:    d.Name,
				DepsType: d.Type,
				AppType:  appType,
			})
		}
	}

	formatBootstrapCoverage(os.Stdout, findings)
	if len(findings) > 0 {
		return fmt.Errorf("%d bootstrap-deps-coverage gap(s) — see output above", len(findings))
	}
	return nil
}

// formatBootstrapCoverage writes one error per finding in the canonical
// `forge lint` shape. Empty findings print a single success line.
func formatBootstrapCoverage(w io.Writer, findings []bootstrapCoverageFinding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "  bootstrap deps coverage clean — every name-matched AppExtras field is wired")
		return
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Package != findings[j].Package {
			return findings[i].Package < findings[j].Package
		}
		return findings[i].Field < findings[j].Field
	})
	for _, f := range findings {
		fmt.Fprintf(w, "  ✗ [forge-bootstrap-deps-coverage] internal/%s/contract.go\n", f.Package)
		fmt.Fprintf(w, "      %s matches AppExtras.%s by name but the types diverge\n", f.Field, f.Field)
		fmt.Fprintf(w, "      Deps.%s        = %s\n", f.Field, f.DepsType)
		fmt.Fprintf(w, "      AppExtras.%s   = %s\n", f.Field, f.AppType)
		fmt.Fprintf(w, "      → align AppExtras.%s to %s, OR wire manually in pkg/app/setup.go after Bootstrap returns\n", f.Field, f.DepsType)
	}
	fmt.Fprintf(w, "\n%d bootstrap-deps-coverage gap(s) in pkg/app/app_extras.go.\n", len(findings))
	fmt.Fprintln(w, "(errors — bootstrap silently drops these wires; the feature no-ops at runtime)")
}
