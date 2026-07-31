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
// Steps that are advisory in text mode (tests, banners,
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
	// that honor it (the forge-convention proto step's
	// method-auth-annotation rule, and the frontend typecheck lane's
	// "could not run" verdict). Plumbed from `forge lint --strict`.
	strict bool
	// skipFrontends drops the whole frontend lane — both the eslint /
	// stylelint step and the typecheck step. Plumbed from
	// `forge lint --skip-frontends`, for callers that want a backend-only
	// gate without paying for the Node toolchain.
	skipFrontends bool
	paths         []string
	cfg           *config.ProjectConfig
	cwd           string
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
			collect:   collectTypedAccessGuardJSON,
		},

		// 2. Contract interface enforcement.
		{
			name:  "contract linter",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.cfg != nil && !rc.cfg.Features.ContractsEnabled() {
					return false, "contracts feature disabled — skipping contract linter"
				}
				// No availability gate: the analysis runs in-process
				// (contract_inprocess.go) — there is no separate
				// contractlint binary to be missing or stale.
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

		// 5. Frontend linters (eslint / stylelint via npm scripts).
		// TypeScript typechecking is step 5b, not this one — see there.
		{
			name:  "frontend lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.skipFrontends {
					return false, "--skip-frontends — skipping frontend lint"
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runFrontendLinters(rc.ctx, rc.cfg, rc.fix)
			},
			errFormat: "❌ Frontend lint failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, g := collectFrontendLintJSON(rc)
				return fs, g, nil
			},
		},

		// 5b. Frontend TypeScript typecheck. forge generates these
		// frontends, so forge — not the caller's shell loop — owns knowing
		// where they live and how to typecheck them. Its own step (rather
		// than a limb of step 5) so it has a name in the table, its own
		// severity dial (lint.frontend.typecheck), and can be skipped
		// independently; the lane's full rationale is in
		// lint_frontend_typecheck.go.
		//
		// gates=true is the STEP's error-reporting posture. Whether a type
		// error actually fails the run is decided inside the lane by the
		// severity dial, which is what a project downgrades or disables —
		// so `warn` mode returns nil here and never trips this gate.
		{
			name:  "frontend typecheck",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.skipFrontends {
					return false, "--skip-frontends — skipping frontend typecheck"
				}
				if rc.cfg != nil && rc.cfg.Lint.Frontend.EffectiveTypecheck() == "off" {
					return false, "lint.frontend.typecheck is \"off\" — skipping frontend typecheck"
				}
				// `stack.frontend.framework: none` is the project saying
				// forge does not drive a Node toolchain here — the same
				// switch that drops these frontends from `forge build`. The
				// lane honors it because it would otherwise warn on EVERY
				// run of a project that deliberately opted out (its deps are
				// not installed under forge's control, so the typecheck
				// could never run). Step 5's eslint lane needs no such gate:
				// its missing-deps path is already a silent skip.
				if rc.cfg.FrontendToolchainDisabled() && len(rc.cfg.Frontends) > 0 {
					return false, "stack.frontend.framework is \"none\" — skipping frontend typecheck"
				}
				// No declared frontend and no frontends/ directory: a clean
				// silent no-op, not a finding. A backend-only project must
				// not be told about a lane that does not apply to it.
				if len(frontendTypecheckTargets(rc.cfg)) == 0 {
					return false, ""
				}
				return true, ""
			},
			runText:   runFrontendTypecheckText,
			errFormat: "❌ Frontend typecheck failed: %v\n",
			collect:   collectFrontendTypecheckJSON,
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
				// Also runs for a frontend-only project: the forge-owned
				// dotenv rule (collectConventionFindings) needs to see a
				// project that declares a frontend or has a frontends/ dir.
				if dirExists("proto") || dirExists("internal") || dirExists("frontends") {
					return true, ""
				}
				if rc.cfg != nil && len(rc.cfg.Frontends) > 0 {
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

		// 10. Scaffold ownership lint — gen-header errors gate the build;
		// surviving FORGE_SCAFFOLD markers (scaffold-not-customized) are
		// warnings, since a fresh scaffold always carries them.
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

		// 10b. Generated-file drift — hand-edits to files carrying the
		// `Code generated by forge. DO NOT EDIT.` banner. ERROR-gated: the
		// next `forge generate` destroys those edits, and lint is the only
		// check that runs between the edit and that regenerate (rationale
		// in lint_generated_drift.go). Unlike the generate-time stomp
		// guard, this scan is unscoped — every drifted forge-owned file
		// gates, not just the ones this invocation's emitters would touch.
		{
			name:  "generated-file drift lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				// Ownership state is project-scoped: outside a forge project
				// there is nothing forge claims to own. Silent skip.
				if rc.cwd == "" || !fileExists("forge.yaml") {
					return false, ""
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runGeneratedDriftLint(rc.cwd)
			},
			errFormat: "❌ generated-file drift lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectGeneratedDriftJSON(rc.cwd)
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

		// 13e. Enforce-component-observe — every wired component with a Service
		// interface + a canonical New(Deps) Service constructor must make an
		// observability decision: `// forge:constructor` to instrument, or
		// `// forge:no-observe` to opt out. Undecided components are aggregated
		// into ONE gating error naming all three escapes. ERROR-gated; the
		// kill-switch is config.enforce_component_observe: off. Mirrors the
		// enforce_typed_access plumbing (config + gate), but always gates (no
		// warn arm — the point is a forcing-function).
		{
			name:  "enforce-component-observe lint",
			gates: true,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				// off → silent skip (no message), like the typed-config guard's
				// non-warn skip.
				if rc.cfg != nil && !rc.cfg.Config.ComponentObserveGuardEnabled() {
					return false, ""
				}
				if !dirExists("internal") {
					return false, ""
				}
				return rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runEnforceComponentObserveLint(rc.cwd)
			},
			errFormat: "❌ enforce-component-observe lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectEnforceComponentObserveJSON(rc.cwd)
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
