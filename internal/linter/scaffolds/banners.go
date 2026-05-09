package scaffolds

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// BannerLintRoot walks templates under root and verifies that each one
// carries the lifecycle banner appropriate for its tier:
//
//   - Tier 1 (machine-only, regenerated every run, gitignored) MUST carry
//     a "// Code generated ... DO NOT EDIT." line in the first ~30 lines.
//     Examples: `*_gen.go.tmpl`, `_gen.ts.tmpl`, `*_gen.yml.tmpl`,
//     auto-generated frontend hooks.
//   - Tier 2 (one-shot scaffolds the user owns after first emit) MUST
//     carry a "forge:scaffold one-shot" marker. Examples:
//     `internal-package/contract.go.tmpl`, `frontend/pages/*.tsx.tmpl`,
//     pack scaffolds shipped once.
//   - Tier 3 (user-owned skeletons like `setup.go.tmpl`, `forge.yaml`,
//     starter `cmd/*.go` files) is intentionally banner-less — `//forge:allow`
//     is the existing user-owned marker and is enforced by other linters.
//
// The walk only visits template files under `internal/templates/` and
// `internal/packs/*/templates/`, so running `forge lint --banners` from
// outside the forge repo is a no-op.
//
// Tier classification is filename-based — see classifyTemplate. Files
// with names that don't slot cleanly into a tier are reported as
// `banner-unclassified` warnings; that's a hint to either bring the
// template into one of the three tiers or to expand the skip-list below.
func BannerLintRoot(root string) (Result, error) {
	var result Result

	templateRoots := []string{
		filepath.Join(root, "internal", "templates"),
		filepath.Join(root, "internal", "packs"),
	}

	for _, troot := range templateRoots {
		if _, err := os.Stat(troot); os.IsNotExist(err) {
			// Outside the forge repo there is no template tree — silently
			// skip rather than emit confusing "no findings" output.
			continue
		}
		err := filepath.WalkDir(troot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if shouldSkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".tmpl") {
				return nil
			}
			// Pack templates only live under packs/*/templates/. Skip
			// packs/<name>/<other-dir>/*.tmpl so we don't traipse into
			// fixtures, e.g. internal/packs/<x>/testdata/.
			if strings.Contains(path, string(filepath.Separator)+"packs"+string(filepath.Separator)) &&
				!strings.Contains(path, string(filepath.Separator)+"templates"+string(filepath.Separator)) {
				return nil
			}
			findings, ferr := lintTemplateBanner(path, root)
			if ferr != nil {
				return ferr
			}
			result.Findings = append(result.Findings, findings...)
			return nil
		})
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

// templateTier captures which lifecycle bucket a template emits into.
type templateTier int

const (
	tierUnclassified templateTier = iota
	tier1Generated                // _gen / regenerated every run
	tier2Scaffold                 // one-shot scaffold, user owns after emit
	tier3UserOwned                // user-owned skeleton; banner-less by design
	tierSkip                      // by-nature banner-less (Makefile, package.json, env files, etc.)
)

// lintTemplateBanner classifies path and emits findings if the required
// banner is missing.
//
// Classification is content-first: a template that already carries the
// canonical Tier-1 banner is treated as Tier 1 regardless of filename
// (this matches reality — pack `_gen` files often have non-`_gen.*`
// names but ship with the canonical header). The same content-first
// rule applies to Tier-2 markers. If neither marker is present, fall
// back to name-based classification.
func lintTemplateBanner(path, root string) ([]Finding, error) {
	rel := relPath(path, root)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Window is generous enough (60 lines) to cover scaffolds whose
	// canonical banner sits below a long doc-comment preamble — e.g.
	// `app_extras.go.tmpl` documents the user-extension pattern in 40+
	// lines of //-comments before declaring `//forge:scaffold one-shot`.
	head := firstLines(data, 60)

	// Content-first overrides:
	//   - the file already declares its tier via the canonical banner,
	//     so the lint has nothing to flag.
	if strings.Contains(head, "Code generated") {
		return nil, nil
	}
	if strings.Contains(head, "forge:scaffold one-shot") {
		return nil, nil
	}
	// `//forge:allow` is the canonical user-owned (Tier-3) marker. Its
	// presence means the template is intentionally banner-less and
	// hand-edited by the user after first emit (e.g. `app_extras.go`,
	// `setup.go`). Treat it as a content-first Tier-3 override so a
	// new user-owned scaffold doesn't have to be added to the
	// classifyTemplate name list before the warning quiets down.
	if strings.Contains(head, "forge:allow") {
		return nil, nil
	}

	// Banner is absent — fall back to name-based classification to decide
	// whether the absence is a finding.
	tier := classifyTemplate(rel)

	switch tier {
	case tierSkip, tier3UserOwned:
		return nil, nil
	case tier1Generated:
		return []Finding{{
			Rule:     "banner-tier1-missing-generated-header",
			Severity: SeverityWarning,
			Path:     rel,
			Message:  `Tier-1 template (regenerated every run) is missing the canonical "Code generated by forge generate. DO NOT EDIT." banner in its first ~30 lines`,
		}}, nil
	case tier2Scaffold:
		return []Finding{{
			Rule:     "banner-tier2-missing-scaffold-header",
			Severity: SeverityWarning,
			Path:     rel,
			Message:  `Tier-2 template (one-shot scaffold) is missing the "forge:scaffold one-shot" banner; users won't know forge writes the file once and never overwrites it`,
		}}, nil
	case tierUnclassified:
		return []Finding{{
			Rule:     "banner-unclassified",
			Severity: SeverityWarning,
			Path:     rel,
			Message:  "template name does not match any known tier classification — extend banners.go's classifyTemplate or skip-list, or rename to fit a known pattern (_gen.* for regenerated, _scaffold for one-shot)",
		}}, nil
	}
	return nil, nil
}

// classifyTemplate buckets a template by its on-disk name + path.
//
// The classifier is intentionally explicit: every known Tier-1 / Tier-2 /
// Tier-3 template is matched by an explicit suffix or path-fragment so
// that a new template added later doesn't silently fall into
// `tierUnclassified` and produce a noisy warning until someone updates
// this map. The "false-positive cost" here is real because lint findings
// are surfaced as advisory output during `forge lint` — every spurious
// warning trains the user to ignore them.
func classifyTemplate(rel string) templateTier {
	base := filepath.Base(rel)
	noTmpl := strings.TrimSuffix(base, ".tmpl")

	// 1) Skip-list: templates that are by-nature banner-less.
	if isSkippedTemplate(noTmpl) {
		return tierSkip
	}

	// 2) Tier 1: _gen.<ext> in the basename, OR an explicit listed
	// regenerated file (cmd-server.go, cmd-cli-main.go, bootstrap*, etc).
	if isGenSuffix(noTmpl) {
		return tier1Generated
	}
	if isKnownTier1(rel, noTmpl) {
		return tier1Generated
	}

	// 3) Tier 2: explicit one-shot scaffolds.
	if isKnownTier2(rel, noTmpl) {
		return tier2Scaffold
	}

	// 4) Tier 3: user-owned skeletons by name.
	if isKnownTier3(rel, noTmpl) {
		return tier3UserOwned
	}

	return tierUnclassified
}

// isSkippedTemplate returns true for templates whose nature precludes a
// header banner: Makefile preamble, .gitignore, language manifests, env
// files, JSON-only configs, etc.
func isSkippedTemplate(noTmpl string) bool {
	switch noTmpl {
	case "Makefile",
		".gitignore",
		".dockerignore",
		"Dockerfile",
		"Taskfile.yml",
		"Taskfile.cli.yml",
		"Taskfile.library.yml",
		"go.mod",
		"go.mod.cli",
		"go.mod.library",
		"gen-go.mod",
		"go.work",
		"package.json",
		"app.json",
		"buf.gen.yaml",
		".env.local",
		"env.example",
		"mcp.json",
		"mcp.json.example",
		"vscode-launch.json",
		"vscode-launch.cli.json",
		"vscode-launch.library.json",
		"k3d.yaml",
		"docker-compose.e2e.yml",
		"kcl.mod",
		"air.toml",
		"air-debug.toml",
		"next.config.ts",
		"reliant.md",
		"reliant-reliant.md",
		"reliant-forge.md":
		return true
	}
	return false
}

// isGenSuffix matches `*_gen.<ext>` for the languages forge emits.
func isGenSuffix(name string) bool {
	for _, suffix := range []string{
		"_gen.go",
		"_gen.ts",
		"_gen.tsx",
		"_gen.k",
		"_gen.yml",
		"_gen.yaml",
		"_gen.sql",
		"_gen.md",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// isKnownTier1 lists templates that are regenerated every `forge generate`
// run but don't carry a `_gen.*` suffix (legacy naming, special files).
func isKnownTier1(rel, noTmpl string) bool {
	// Frontend mocks and hooks: regenerated from the proto descriptor each run.
	if strings.HasSuffix(rel, "frontend/hooks.ts.tmpl") ||
		strings.HasSuffix(rel, "frontend/mocks/mock-data.ts.tmpl") ||
		strings.HasSuffix(rel, "frontend/mocks/mock-transport.ts.tmpl") ||
		strings.HasSuffix(rel, "frontend/nextjs/src/lib/otel.ts.tmpl") {
		return true
	}
	// Project-level cmd/ scaffolds: regenerated; they carry the canonical header.
	switch noTmpl {
	case "cmd-cli-main.go", "cmd-cli-version.go",
		"cmd-shared-main.go", "cmd-shared-service.go",
		"cmd-server.go", "cmd-version.go", "cmd-db.go", "cmd-root.go":
		return true
	case "bootstrap.go", "bootstrap_testing.go",
		"config.go", "migrate.go",
		"alloy-config.alloy":
		return true
	}
	// CI workflows: regenerated wholesale. CODEOWNERS is intentionally
	// not in this list — it's a one-shot scaffold (starter content) and
	// classified as Tier 2 elsewhere.
	if strings.HasPrefix(rel, "internal/templates/ci/") {
		switch noTmpl {
		case "ci.yml", "deploy.yml", "build-images.yml", "e2e.yml",
			"proto-breaking.yml", "dependabot.yml":
			return true
		}
	}
	return false
}

// isKnownTier2 lists one-shot scaffolds: forge writes them once and never
// overwrites them after the first emit.
func isKnownTier2(rel, noTmpl string) bool {
	// internal-package scaffolds (pkg new flow + adapter/interactor flavors).
	if strings.Contains(rel, "internal/templates/internal-package/") {
		switch noTmpl {
		case "service.go", "contract.go", "contract_test.go",
			"client.go", "client_test.go",
			"service_test.go",
			"adapter.go", "adapter_test.go",
			"interactor.go", "interactor_test.go":
			return true
		}
	}
	// Binary scaffolds (forge add binary flow): cobra subcommand + the
	// internal/<name>/ runtime trio. Classified as one-shot because the
	// user owns Run's body and contract.go's Deps from emission onward.
	if strings.Contains(rel, "internal/templates/project/binary/") {
		switch noTmpl {
		case "cmd-binary.go", "binary.go", "binary_test.go", "contract.go", "contract_test.go":
			return true
		}
	}
	// Frontend page scaffolds (forge add page).
	if strings.Contains(rel, "internal/templates/frontend/pages/") {
		return true
	}
	// Pack scaffolds. Anything under packs/<name>/templates/ that emits
	// Go/TS/Markdown/Proto and is NOT a `_gen.*` (already Tier 1) is a
	// one-shot pack scaffold by convention.
	if strings.Contains(rel, "internal/packs/") &&
		strings.Contains(rel, "/templates/") {
		// Skip pack manifests / migrations / static config.
		switch noTmpl {
		case "pack.yaml":
			return false
		}
		// SQL migration files are managed-once by the pack install flow.
		if strings.HasSuffix(noTmpl, ".sql") {
			return true
		}
		// Index re-export files (auth-ui/data-table) and proto contracts.
		if strings.HasSuffix(noTmpl, ".go") ||
			strings.HasSuffix(noTmpl, ".ts") ||
			strings.HasSuffix(noTmpl, ".tsx") ||
			strings.HasSuffix(noTmpl, ".md") ||
			strings.HasSuffix(noTmpl, ".proto") {
			return true
		}
	}
	// Service-package one-shot tests with FORGE_SCAFFOLD markers.
	switch noTmpl {
	case "unit_test.go",
		"handlers_crud_test_gen.go",
		"handlers_crud_integration_test.go":
		return true
	}
	// CI starter files that ship a default that the user is expected to
	// review and own.
	if strings.HasPrefix(rel, "internal/templates/ci/") && noTmpl == "CODEOWNERS" {
		return true
	}
	return false
}

// isKnownTier3 lists user-owned skeletons that are intentionally
// banner-less (the user's `//forge:allow` and contract.go conventions
// are checked by other analyzers).
func isKnownTier3(rel, noTmpl string) bool {
	// Project-level user-owned skeletons.
	switch noTmpl {
	case "setup.go", "tools.go", "app_extras.go", "config.proto",
		"example.proto", "user-example.proto", "entity-example.proto",
		"config-dev.yaml", "config-prod.yaml", "config-test.yaml",
		"docker-compose.yml":
		return true
	case "README.md", "README.cli.md", "README.library.md",
		"CONTRIBUTING.md", "CHANGELOG.md", "CODEOWNERS",
		"db-README.md", "examples-README.md",
		"pkg-app-CONVENTIONS.md", "golangci.yml":
		return true
	}
	// Service-package user-owned files.
	if strings.Contains(rel, "internal/templates/service/") {
		switch noTmpl {
		case "service.go", "handlers.go", "authorizer.go",
			"integration_test.go":
			return true
		}
	}
	// Middleware user-owned skeletons.
	if strings.Contains(rel, "internal/templates/middleware/") {
		switch noTmpl {
		case "auth.go", "auth_validator.go":
			return true
		}
	}
	// Worker scaffolds (worker.go.tmpl / worker_test.go.tmpl + cron variants).
	if strings.Contains(rel, "internal/templates/worker/") ||
		strings.Contains(rel, "internal/templates/worker-cron/") {
		return true
	}
	// Webhook scaffolds (user-owned routes/store/test).
	if strings.Contains(rel, "internal/templates/webhook/") &&
		noTmpl != "webhook_routes_gen.go" {
		return true
	}
	// Operator + CRD shells.
	if strings.Contains(rel, "internal/templates/operator/") ||
		strings.Contains(rel, "internal/templates/crd/") {
		return true
	}
	// E2E tests.
	if strings.Contains(rel, "internal/templates/test/e2e/") {
		return true
	}
	// KCL deploy environments — multi-env infra config; user-owned after first emit.
	if strings.Contains(rel, "internal/templates/deploy/kcl/") {
		return true
	}
	// Frontend Next.js / RN page scaffolds + connect/nav/layout.
	if strings.Contains(rel, "internal/templates/frontend/nextjs/") ||
		strings.Contains(rel, "internal/templates/frontend/react-native/") {
		return true
	}
	return false
}
