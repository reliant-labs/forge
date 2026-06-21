// File: internal/cli/lint_steps.go
//
// The single source of truth for the `forge lint` (no-flag) linter
// pipeline. Historically runAllLinters (text, in lint.go) and
// collectAllLintersJSON (JSON, in lint_json.go) each hand-encoded the
// SAME ordered 14-step sequence — feature gate, directory check, gating
// verdict — and were kept in lockstep by comment ("Step numbers track
// runAllLinters for diffability", "mirrors text mode step 13c"). That
// mirrored dispatch was the single biggest structural debt in the lint
// surface.
//
// This file models each linter as a value (linterStep) and builds ONE
// ordered []linterStep. Each step declares ONCE:
//
//   - name        — stable identity (also the JSON collectErr label)
//   - gates       — whether a hard collection error fails the build
//   - shouldRun   — the feature-gate / dir-exists / tool-on-PATH guard,
//                   returning a skip message when the step is a no-op
//   - runText     — the bespoke human-output action (emoji headers, the
//                   per-linter "✓ passed" lines); returns a gating-or-nil
//                   error exactly as the old inline body did
//   - errFormat   — how runAllLinters reports a non-nil runText error to
//                   stderr (kept byte-identical to the old inline Fprintf)
//   - collect     — the JSON-shaped collector (findings + per-step gated)
//
// The ordered table is then rendered two ways by thin drivers:
// runAllLinters (text) and collectAllLintersJSON (JSON). The output of
// BOTH formats is byte-identical to the pre-refactor code; TestLintHelpSurface
// and the lint_json tests are the guardrail.
//
// Steps that are advisory in text mode (frontend-packs, tests, banners,
// optional-deps-guard, config-deps, check-workarounds) carry gates=false:
// their runText errors print a ⚠️ line but never set hasFailed, and their
// JSON collection errors degrade to a warning finding that never flips ok.

package lint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
)

// lintRunCtx carries the shared inputs every step needs. cwd is resolved
// once up front (text mode historically re-called os.Getwd per cwd-using
// step and skipped that step on error; resolving once and treating an
// empty cwd as "skip" preserves that behavior without the repetition).
type lintRunCtx struct {
	ctx context.Context
	fix bool
	// strict escalates advisory security findings to errors in the steps
	// that honor it (today: the forge-convention proto step's
	// method-auth-annotation rule). Plumbed from `forge lint --strict`.
	strict bool
	paths  []string
	cfg    *config.ProjectConfig
	cwd    string
}

// linterStep is one entry in the ordered `forge lint` pipeline. See the
// file header for the field contract.
type linterStep struct {
	name  string
	gates bool

	// shouldRun reports whether the step executes. When it returns
	// run=false with a non-empty skipMsg, text mode prints "⚠️  "+skipMsg
	// and JSON mode emits a skippedFinding(skipMsg). A false/empty pair
	// means "silently absent" (directory not present) — no output.
	shouldRun func(rc *lintRunCtx) (run bool, skipMsg string)

	// runText executes the bespoke human-output action and returns a
	// gating-or-nil error (the old inline body verbatim).
	runText func(rc *lintRunCtx) error

	// errFormat is the printf format used to report a non-nil runText
	// error to stderr. It must contain exactly one %v for the error and
	// the trailing newline — kept byte-identical to the old inline call.
	errFormat string

	// collect is the JSON collector. The returned bool is the per-step
	// gating verdict (mirrors runText's error-gating for findings-level
	// gating). A non-nil error is a hard collection failure, converted by
	// the JSON driver into an "external" finding whose severity/gating is
	// governed by step.gates.
	collect func(rc *lintRunCtx) ([]lintJSONFinding, bool, error)
}

// lintPipeline returns the ordered linter table. Step numbers in the
// comments are the historical labels (gaps — 3, 6, 12 — are intentional;
// they tracked removed linters and are preserved for diffability against
// git history).
//
//nolint:funlen // declarative 14-entry linter registry, not branching complexity
func lintPipeline() []linterStep {
	return []linterStep{
		// 1. Standard Go linters (golangci-lint).
		{
			name:  "golangci-lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if _, err := exec.LookPath("golangci-lint"); err != nil {
					return false, "golangci-lint not found on PATH — skipping"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runGolangciLint(rc.ctx, rc.fix, rc.paths)
			},
			errFormat: "❌ golangci-lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, g := collectGolangciLintJSON(rc.ctx, rc.paths)
				return fs, g, nil
			},
		},

		// 1b. Typed-config guardrail (forbidigo) — ADVISORY arm of
		// config.enforce_typed_access. Only runs in `warn` mode: there the
		// generated .golangci.yml deliberately omits forbidigo from its
		// gating `linters.enable` list, so this non-gating step surfaces the
		// os.Getenv / os.LookupEnv / os.Environ findings as warnings. In
		// `error` mode forbidigo is enabled in the main gating golangci run
		// (step 1) and this step is skipped; `off` skips it too.
		//
		// Why the warn/error switch lives in .golangci.yml's linters.enable
		// (not here, by having forge own the gating decision): the PRIMARY
		// consumer of the guardrail is CI, which runs `golangci-lint run`
		// DIRECTLY via golangci-lint-action — it never routes through
		// `forge lint`. So `linters.enable` membership is the only thing that
		// can make CI fail. Centralizing the decision in `forge lint` would
		// silently stop gating CI in error mode. This step exists purely to
		// give warn-mode users LOCAL visibility of findings golangci is
		// configured to ignore.
		{
			name:  "typed-config guardrail",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.cfg == nil || rc.cfg.Config.EffectiveEnforceTypedAccess() != config.EnforceTypedAccessWarn {
					return false, ""
				}
				if _, err := exec.LookPath("golangci-lint"); err != nil {
					return false, "golangci-lint not found on PATH — skipping typed-config guardrail"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runTypedAccessGuardAdvisory(rc.ctx, rc.paths)
			},
			errFormat: "⚠️  typed-config guardrail: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectTypedAccessGuardJSON(rc.ctx, rc.paths)
			},
		},

		// 2. Contract interface enforcement.
		{
			name:  "contract linter",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.cfg != nil && !rc.cfg.Features.ContractsEnabled() {
					return false, "contracts feature disabled — skipping contract linter"
				}
				if _, err := resolveContractLintBinary(rc.ctx); err != nil {
					return false, "contractlint not available — skipping"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runContractLinter(rc.ctx, rc.paths, contractExcludesFromConfig(rc.cfg))
			},
			errFormat: "❌ contract linter failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectContractLintJSON(rc.ctx, rc.paths, contractExcludesFromConfig(rc.cfg))
			},
		},

		// 4. Buf lint.
		{
			name:  "buf lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if _, err := exec.LookPath("buf"); err != nil {
					return false, "buf not found on PATH — skipping buf lint"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runBufLint(rc.ctx)
			},
			errFormat: "❌ buf lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, g := collectBufLintJSON(rc.ctx)
				return fs, g, nil
			},
		},

		// 5. Frontend linters (tsc + eslint / npm scripts).
		{
			name:  "frontend lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runFrontendLinters(rc.ctx, rc.cfg)
			},
			errFormat: "❌ Frontend lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, g := collectFrontendLintJSON(rc.ctx, rc.cfg)
				return fs, g, nil
			},
		},

		// 7. SQL migration safety lint.
		{
			name:  "migration safety lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.cfg != nil && !rc.cfg.Features.MigrationsEnabled() {
					return false, "migrations feature disabled — skipping migration safety lint"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runMigrationSafetyLint(rc.cfg)
			},
			errFormat: "❌ Migration safety lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectMigrationSafetyJSON(rc.cfg)
			},
		},

		// 8. Forge convention rules (proto + internal-package contracts).
		// Errors gate the build; warnings are surfaced but tolerated.
		{
			name:  "forge convention lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if dirExists("proto") || dirExists("internal") {
					return true, ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runConventionLint(forgeconv.LintOptions{Strict: rc.strict})
			},
			errFormat: "❌ Forge convention lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectConventionsJSON(forgeconv.LintOptions{Strict: rc.strict})
			},
		},

		// 8b. Authz completeness — the generate-time first line of defense
		// for descriptor-driven authorization. Fails the build when any RPC
		// lacks an explicit authz decision (required_roles / authz_public /
		// service default_roles). Errors gate. Runs only when buf.yaml +
		// proto/ are present and buf is on PATH (the descriptor build needs
		// it); the runtime fail-closed deny in forge/pkg/authz is the
		// backstop.
		{
			name:  "authz completeness lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if !authzCompletenessApplies(".") {
					return false, ""
				}
				if _, err := exec.LookPath("buf"); err != nil {
					return false, "buf not found on PATH — skipping authz completeness lint"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runAuthzCompletenessLint(rc.ctx)
			},
			errFormat: "❌ authz completeness lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectAuthzCompletenessJSON(rc.ctx)
			},
		},

		// 9. Frontend pack convention rules — only meaningful inside the
		// forge repo (where pack sources live). Warnings only; never gates.
		{
			name:  "frontend pack lint",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if dirExists(filepath.Join("internal", "packs")) {
					return true, ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runFrontendPackLint()
			},
			errFormat: "⚠️  Frontend pack lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectFrontendPacksJSON()
				return fs, false, err
			},
		},

		// 10. Scaffold ownership lint — errors gate the build.
		{
			name:  "scaffold ownership lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runScaffoldsLint()
			},
			errFormat: "❌ Scaffold ownership lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectScaffoldsJSON(rc.cwd)
			},
		},

		// 11. Handler-test convention lint — warnings only; never gates.
		{
			name:  "test convention lint",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if dirExists("internal/handlers") {
					return true, ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runTestsLint()
			},
			errFormat: "⚠️  Handler-test lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectTestsJSON(rc.cwd)
				return fs, false, err
			},
		},

		// 11b. Lifecycle-banner lint — forge repo only; warnings only.
		{
			name:  "banner lint",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if dirExists(filepath.Join("internal", "templates")) ||
					dirExists(filepath.Join("internal", "packs")) {
					return true, ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runBannersLint()
			},
			errFormat: "⚠️  Banner lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectBannersJSON(rc.cwd)
				return fs, false, err
			},
		},

		// 13. Wire-coverage — TODO findings are warnings, unresolved
		// forge:placeholder annotations are errors and gate the build.
		{
			name:  "wire-coverage lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if fileExists(filepath.Join("pkg", "app", "wire_gen.go")) ||
					fileExists(filepath.Join("pkg", "app", "app_extras.go")) {
					return rc.cwd != "", ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runWireCoverageLint(rc.cwd)
			},
			errFormat: "❌ wire-coverage lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectWireCoverageJSON(rc.cwd)
			},
		},

		// 13b. Bootstrap-deps-coverage — catches the audit-no-op silent-drop
		// bug class. Gaps gate the build.
		{
			name:  "bootstrap-deps-coverage lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if fileExists(filepath.Join("pkg", "app", "bootstrap.go")) &&
					fileExists(filepath.Join("pkg", "app", "app_extras.go")) {
					return rc.cwd != "", ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runBootstrapDepsCoverageLint(rc.cwd)
			},
			errFormat: "bootstrap-deps-coverage lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectBootstrapCoverageJSON(rc.cwd)
			},
		},

		// 13c. Optional-deps-guard — flags unguarded derefs of
		// `// forge:optional-dep` Deps fields. Warnings only.
		{
			name:  "optional-deps-guard lint",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				// All component trees (handlers/workers/operators + internal
				// packages) now live under internal/, so a single check covers
				// every project that has any wireable component.
				if dirExists("internal") {
					return rc.cwd != "", ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runOptionalDepsGuardLint(rc.cwd)
			},
			errFormat: "⚠️  optional-deps-guard lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectOptionalDepsGuardJSON(rc.cwd)
				return fs, false, err
			},
		},

		// 13d. Config-deps — flags scalar Deps fields (configuration, not
		// collaborators). Warnings only.
		{
			name:  "config-deps lint",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				// All component trees (handlers/workers/operators + internal
				// packages) now live under internal/, so a single check covers
				// every project that has any wireable component.
				if dirExists("internal") {
					return rc.cwd != "", ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runConfigDepsLint(rc.cwd)
			},
			errFormat: "⚠️  config-deps lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectConfigDepsJSON(rc.cwd)
				return fs, false, err
			},
		},

		// 14. Check-workarounds — flags canonical cross-lane workarounds.
		// Warnings only.
		{
			name:  "check-workarounds lint",
			gates: false,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runCheckWorkaroundsLint(rc.cwd)
			},
			errFormat: "⚠️  check-workarounds lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectWorkaroundsJSON(rc.cwd)
				return fs, false, err
			},
		},
	}
}

// lintCwd resolves the working directory for the pipeline, returning ""
// on error. Text mode historically re-called os.Getwd per cwd-using step
// and skipped that step when it failed; resolving once and treating ""
// as "skip the cwd-bound steps" preserves that behavior. (JSON mode
// already hard-fails on a getwd error before the sweep, so an empty cwd
// never reaches the JSON driver.)
func lintCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}
