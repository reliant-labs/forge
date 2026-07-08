// Config vs. filesystem cross-check — the "loud-by-default" guard.
//
// Pre-2026-06-07 the generate pipeline silently skipped declared entities
// that had no on-disk backing, and silently ignored on-disk entities that
// weren't declared in forge.yaml. The asymmetry was the #1 source of
// "I added a service but generate did nothing" friction:
//
//   - forge.yaml declares services[].name=foo but proto/services/foo/
//     doesn't exist → generate runs, emits nothing for foo, exits 0.
//   - proto/services/foo/ exists on disk but forge.yaml lacks an entry →
//     generate sees the proto but skips bootstrap wiring for it.
//   - packages[].name=foo but internal/foo/contract.go missing → bootstrap
//     codegen emits a broken import the validate step fails on, pointing
//     at generated code instead of the missing contract.
//
// This file walks forge.yaml's declarations and the proto/internal trees
// in parallel and collects every mismatch into a single batched report.
// stepLoadConfig calls validateConfigVsFilesystem after a successful
// LoadStrict so the user sees the asymmetry the moment they run generate,
// not three steps deeper at a confusing "missing import" error.
//
// Opt-out: --skip-config-check. We expose an opt-out (not opt-in) so the
// default path is loud — new adopters get the check unconditionally, and
// scripted callers in pathological setups (e.g. mid-migration where a
// proto dir exists transiently with no forge.yaml entry yet) can pass the
// flag to bypass without changing the steady-state default.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// validateConfigVsFilesystem cross-checks forge.yaml declarations against
// on-disk reality. Returns a non-nil error wrapping every mismatch as a
// single batched cliutil.UserErr — callers (stepLoadConfig) surface this
// at load time so the user fixes all asymmetries in one round-trip.
//
// Skip semantics: when cfg is nil (directory-scan fallback) there's
// nothing to validate against, so we no-op. When the project has no
// proto/ tree at all (CLI / library kind), we also no-op — those project
// shapes legitimately have no services/packages/frontends.
func validateConfigVsFilesystem(projectDir string, cfg *config.ProjectConfig) error {
	if cfg == nil {
		return nil
	}
	var findings []string

	findings = append(findings, checkDeclaredFrontends(projectDir, cfg)...)
	findings = append(findings, checkDeclaredPackages(projectDir, cfg)...)

	if len(findings) == 0 {
		return nil
	}

	sort.Strings(findings)
	var b strings.Builder
	fmt.Fprintf(&b, "%d forge.yaml ↔ filesystem mismatch(es):\n\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "  • %s\n", f)
	}
	return cliutil.UserErr("forge generate (config check)",
		b.String(),
		"forge.yaml",
		"fix the mismatches above, or pass --skip-config-check to bypass for parallel-lane / mid-migration scenarios")
}

// defaultServicePath returns the conventional on-disk path for a
// component entry whose `path:` field was omitted. Mirrors the defaulting
// in loadProjectConfigFrom but expands the rule to cover every kind so
// the cross-check uses the same conventions as the rest of the pipeline.
func defaultServicePath(c config.ComponentConfig) string {
	switch c.EffectiveKind() {
	case config.ComponentKindWorker, config.ComponentKindCron:
		return "internal/workers/" + c.Name
	case config.ComponentKindOperator:
		return "internal/operators/" + c.Name
	case config.ComponentKindBinary:
		// Each binary gets its own cmd/<bin>/ tree (devspace idiom).
		return filepath.Join("cmd", naming.ServicePackage(c.Name), "main.go")
	default:
		return "internal/handlers/" + c.Name
	}
}

// checkDeclaredFrontends walks cfg.Frontends and verifies each entry has
// an on-disk directory at the configured path. Frontends do not have a
// proto-side requirement (they consume the gen/ts/ stubs from services
// they don't own), so the check is one-sided: path must exist.
//
// We skip the check when fe.Path is empty AND the default path doesn't
// exist either — that's a freshly-scaffolded entry the user has declared
// but not run `forge add frontend` against yet. Better to let the
// scaffold step handle it than to error out here.
func checkDeclaredFrontends(projectDir string, cfg *config.ProjectConfig) []string {
	var out []string
	for _, fe := range cfg.Frontends {
		path := fe.Path
		if path == "" {
			path = "frontends/" + fe.Name
		}
		fullPath := filepath.Join(projectDir, path)
		if !dirExists(fullPath) {
			out = append(out, fmt.Sprintf(
				"frontends[name=%s] (type=%s) declared in forge.yaml but path %q does not exist (expected at %s) — run 'forge add frontend %s' to scaffold it",
				fe.Name, fe.Type, path, fullPath, fe.Name))
		}
	}
	return out
}

// checkDeclaredPackages walks cfg.Packages and verifies each entry has a
// matching internal/<pkg>/contract.go on disk. The bootstrap codegen
// template hardcodes references to <pkg>.New(...) for every internal
// package; a missing contract.go produces a bootstrap.go that doesn't
// compile, with the build error pointing at generated code rather than at
// the missing source — exactly the failure mode the loud-by-default
// architecture exists to prevent.
func checkDeclaredPackages(projectDir string, cfg *config.ProjectConfig) []string {
	var out []string
	for _, pkg := range cfg.Packages {
		// internal/<pkg>/contract.go is the canonical location. The
		// package-name normalization (kebab vs snake) follows the same
		// ServicePackage rule.
		canonical := naming.ServicePackage(pkg.Name)
		contractPath := filepath.Join(projectDir, "internal", canonical, "contract.go")
		if _, err := os.Stat(contractPath); err == nil {
			continue
		}
		// Fallback: try the literal name without normalization.
		rawPath := filepath.Join(projectDir, "internal", pkg.Name, "contract.go")
		if _, err := os.Stat(rawPath); err == nil {
			continue
		}
		out = append(out, fmt.Sprintf(
			"packages[name=%s] declared in forge.yaml but contract.go missing (expected at %s) — run 'forge add package %s' to scaffold it",
			pkg.Name, contractPath, pkg.Name))
	}
	return out
}
