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
//   - scope       — how the lane honours `forge lint --scope` (by file, by
//                   Go package, or not at all for a whole-project lane);
//                   see lint_structured.go
//
// The ordered table is then rendered by thin drivers: runAllLinters (text)
// and collectLintOutcomes (the structured engine behind --json, --quiet and
// --scope). The output of
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

	"github.com/reliant-labs/forge/internal/codegen"
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
	// scope is `forge lint --scope`; nil is the whole project. Only the
	// structured drivers (collectLintOutcomes) read it — see
	// lint_structured.go.
	scope *lintScope
}

// linterStep is one entry in the ordered `forge lint` pipeline. See the
// file header for the field contract.
type linterStep struct {
	name  string
	gates bool
	// scope declares how the lane honours `forge lint --scope`
	// (scopeByFile / scopeByPackages / scopeWholeProject). Required: the
	// zero value is invalid, so a new lane must decide.
	scope scopeMode

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
			// Takes package arguments, so --scope narrows what it RUNS on
			// (the slow lane), not just what it reports.
			scope: scopeByPackages,
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
				fs, g := collectGolangciLintJSON(rc.ctx, rc.paths, rc.fix)
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
			scope: scopeByPackages,
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
			scope: scopeByPackages,
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
				return runContractLinter(rc.ctx, rc.paths, contractExcludesFromConfig(rc.cfg), contractGateOptions(rc.cfg, rc.strict))
			},
			errFormat: "❌ contract linter failed: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectContractLintJSON(rc.ctx, rc.paths, contractExcludesFromConfig(rc.cfg), contractGateOptions(rc.cfg, rc.strict))
			},
		},

		// 4. Buf lint.
		{
			name:  "buf lint",
			gates: true,
			scope: scopeByFile,
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
			// The frontend's own `npm run lint` over the whole frontend: its
			// output is eslint's grouped report, not file:line findings, so
			// nothing can attribute it to a slice.
			scope: scopeWholeProject,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.skipFrontends {
					return false, "--skip-frontends — skipping frontend lint"
				}
				// No declared frontend and no frontends/ directory: a silent
				// no-op, as the typecheck lane below already treats it. It
				// used to "run" over nothing and count as a gating linter that
				// passed, and a --scope run named it as a lane left unchecked
				// on a project that has no frontend at all. A DECLARED
				// frontend whose directory is missing still runs, so it is
				// reported as could-not-run rather than vanishing.
				if len(frontendDirsForLint()) == 0 {
					return false, ""
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
			scope: scopeByFile,
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
			scope: scopeByFile,
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
			scope: scopeByFile,
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
			scope: scopeByFile,
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
			scope: scopeByFile,
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

		// 11. Test-convention lint. The handler and hook-test-presence
		// rules are warnings, but the hook-test SHAPE rule gates: a
		// scaffolded test asserting the opposite shape from its hook can
		// never pass, so letting it through would be reporting green over
		// a permanently red suite.
		{
			name:  "test convention lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if dirExists("internal/handlers") || dirExists("frontends") {
					return true, ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runTestsLint()
			},
			errFormat: "❌ Test-convention lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectTestsJSON(rc.cwd)
			},
		},

		// 11b. Lifecycle-banner lint — forge repo only; warnings only.
		{
			name:  "banner lint",
			gates: false,
			scope: scopeByFile,
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
			scope: scopeByFile,
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
			scope: scopeByFile,
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

		// 13d-ter. Component-drift — reports components the code declares
		// that deploy/kcl/workloads.k does not (and vice versa). forge does
		// not regenerate that file, so this is what notices when the two
		// drift apart. Warnings only: both directions have legitimate
		// deliberate cases (see lint_component_drift.go).
		{
			name:  "component-drift lint",
			gates: false,
			// Compares the code's components against deploy/kcl/workloads.k:
			// a fact about the two trees together, not about any one file.
			scope: scopeWholeProject,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.cwd == "" || rc.cfg == nil {
					return false, ""
				}
				// Only meaningful once deploy is scaffolded. A project
				// without workloads.k has nothing to drift from.
				if !codegen.WorkloadsKCLExists(rc.cwd) {
					return false, ""
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runComponentDrift(rc.cwd, rc.cfg)
			},
			errFormat: "⚠️  component-drift lint: %v\n",
			collect:   collectComponentDriftJSON,
		},

		// 13d-quater. Hosted-image-base — flags a host-bearing image on a
		// forge.OnHosted item, which the control plane admits only from its
		// own registry (ADR-0003 F1). Warnings only: `forge env render <env>`
		// performs the authoritative check against the real render, and this
		// text scan under-reports by construction, so it must not gate.
		{
			name:  "hosted-image-base lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if rc.cwd == "" {
					return false, ""
				}
				// Nothing to judge without a deploy tree.
				if !dirExists(filepath.Join(rc.cwd, deployKCLDirFor(rc.cfg))) {
					return false, ""
				}
				return true, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runHostedImageBaseLint(rc.cwd, rc.cfg)
			},
			errFormat: "⚠️  hosted-image-base lint: %v\n",
			collect:   collectHostedImageBaseJSON,
		},

		// 13d-ter-bis. PDB-blocks-drain — a PodDisruptionBudget that can
		// never allow a disruption (see lint_pdb.go). Warnings only.
		{
			name:  "pdb-blocks-drain lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return rc.cwd != "" && dirExists(filepath.Join(rc.cwd, deployKCLDirFor(rc.cfg))), ""
			},
			runText: func(rc *lintRunCtx) error {
				return runPDBLint(rc.ctx, rc.cwd, rc.cfg)
			},
			errFormat: "⚠️  pdb-blocks-drain lint: %v\n",
			collect:   collectPDBJSON,
		},

		// 13d-bis. Column-markers — flags a COMMENT ON COLUMN/CONSTRAINT
		// whose text contains forge: but matches no known column marker
		// (see lint_column_markers.go). Warnings only.
		{
			name:  "column-markers lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				if dirExists(migrationsDirFor(rc.cfg)) {
					return true, ""
				}
				return false, ""
			},
			runText: func(rc *lintRunCtx) error {
				return runColumnMarkersLint(rc.cfg)
			},
			errFormat: "⚠️  column-markers lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectColumnMarkersJSON(rc.cfg)
				return fs, false, err
			},
		},

		// 13d-quinquies. ShellBuild-tokens — flags a retired forge
		// substitution token (${TARGETARCH}, ${IMAGE}, …) left in a
		// ShellBuild cmd. GATES, unlike its advisory neighbours, because
		// `GOARCH=${TARGETARCH}` becomes `GOARCH=` and silently builds for
		// the host arch — see lint_shellbuild_tokens.go.
		{
			name:  "shellbuild-tokens lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				// No env KCL means no ShellBuild to check.
				return dirExists(deployKCLDirDefault), ""
			},
			runText: func(rc *lintRunCtx) error {
				return runShellBuildTokensLint(deployKCLDirDefault)
			},
			errFormat: "✗ shellbuild-tokens lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectShellBuildTokensJSON(deployKCLDirDefault)
			},
		},

		// 13d-ter. Proto-markers — the same check one layer up: flags a
		// .proto comment whose text contains forge: but matches no known
		// proto marker (see lint_proto_markers.go). A misspelled marker
		// compiles fine and does nothing, so this is the only place it
		// surfaces. Warnings only.
		{
			name:  "proto-markers lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				// No proto tree at all (CLI / library projects) → silent
				// skip, matching the column check's missing-migrations arm.
				return dirExists(protoDirDefault), ""
			},
			runText: func(rc *lintRunCtx) error {
				return runProtoMarkersLint(protoDirDefault)
			},
			errFormat: "⚠️  proto-markers lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectProtoMarkersJSON(protoDirDefault)
				return fs, false, err
			},
		},

		// 13d-quater. Create-nullability — a field's `optional` label must
		// agree between an entity message and its Create<Entity>Request.
		// The Create request is the one envelope that FLATTENS the entity,
		// so it re-declares the label, and a label lost in the
		// re-declaration collapses absent and zero: the create writes ""
		// into a nullable column and postgres answers with a foreign-key
		// violation naming a constraint rather than the proto line that
		// caused it. GATES when forge generates the create (those are the
		// op's failure modes); a hand-written create owns what omission
		// means, so it warns or passes (lint_create_nullability.go).
		{
			name:  "create-nullability lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return dirExists(protoDirDefault), ""
			},
			runText: func(rc *lintRunCtx) error {
				return runCreateNullabilityLint(protoDirDefault, rc.cwd)
			},
			errFormat: "❌ create-nullability lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectCreateNullabilityJSON(protoDirDefault, rc.cwd)
			},
		},

		// 13d-quinquies. Computed-fields — a `// forge:computed` field is a
		// DECLARED obligation that something derives the value. Nothing
		// forge generates does, so an unmet obligation means the insert
		// takes the column default: $0.00 money with no error anywhere,
		// found only by a human reading a screen.
		//
		// GATES, at least as hard as its read-only twin: computed is the
		// stronger promise ("my app derives this"). It used to warn on the
		// theory that a marker added before its hook is a legitimate
		// intermediate state; the deterministic form of that state —
		// forge's own unwired stubs still in the service — is pending-stub
		// mode, which holds the finding at warning and names the stubs
		// (lint_pending_stubs.go). Everything else fails.
		{
			name:  "computed-fields lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return dirExists(protoDirDefault) && rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runComputedFieldsLint(rc.cwd)
			},
			errFormat: "❌ computed-fields lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectComputedFieldsJSON(rc.cwd)
				return fs, anyErrorFinding(fs), err
			},
		},

		// 13d-sexies. Read-only-fields — the twin of computed-fields for the
		// marker where the same failure is SILENT. `// forge:read-only`
		// strips a field from every write envelope; when nothing else
		// populates the column and its DEFAULT is the type's zero, every
		// row ships as 0 — no constraint violated, no test failed (the born
		// CRUD test derives its fixtures from the same schema and agrees
		// with the defect by construction), no log line. Schema-gated to
		// stay quiet on GENERATED columns, managed timestamps, and real
		// defaults.
		//
		// GATES, like its computed-field twin. An unwritten read-only
		// column is a shipped defect whose ONLY symptom is a human reading
		// $0.00 on a screen. A warning inside a hundred-line lint run is
		// very close to the "no log line anywhere" the rule exists to fix,
		// and the audited run caught its two only because someone was
		// deliberately looking for them. The one exception is pending-stub
		// mode: right after `forge scaffold` the rpc that will write the
		// column is still forge's own unwired stub, so the finding is a
		// warning naming those stubs until they are implemented
		// (lint_pending_stubs.go).
		//
		// Gating is defensible because every exclusion is pinned by a test
		// (GENERATED, managed timestamps, real defaults, NOT NULL-without-
		// DEFAULT, forge:fill=, forge:computed), and because the two write
		// paths the Go scan structurally could not see — a trigger body and
		// a column-named UPDATE — are now read out of the migrations and
		// the Go SQL literals. See columnsWrittenBySQL.
		{
			name:  "read-only-fields lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return dirExists(protoDirDefault) && rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runReadOnlyFieldsLint(rc.cwd, migrationsDirFor(rc.cfg))
			},
			errFormat: "❌ read-only-fields lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectReadOnlyFieldsJSON(rc.cwd, migrationsDirFor(rc.cfg))
				return fs, anyErrorFinding(fs), err
			},
		},

		// 13d-sexies-bis. Fixture-drift — scaffold-once lifecycle tests
		// born with LITERAL fixture SQL, aging out of a schema the author
		// keeps hardening. A later migration that makes a column GENERATED,
		// adds a UNIQUE, a foreign key or a one-way status CHECK turns a
		// seeded row into a pq error in test SETUP, naming neither the
		// fixture nor the migration.
		//
		// The lane EXECUTES each fixture statement against the shadow
		// schema in a rolled-back transaction and reports postgres's own
		// verdict per statement — every failure class, not the few a text
		// matcher anticipated. It used to be three matchers in two lanes
		// (this one and crud-fixtures); a CHECK added after birth matched
		// none of them, so the lanes reported clean over fixtures postgres
		// rejects. Newly scaffolded lifecycle tests carry no literal SQL
		// (they call the regenerated create-request factories), so for them
		// this lane costs nothing and opens no database.
		//
		// Warnings only, matching guarded-fields: the fix is an edit to a
		// file forge does not own and may legitimately be mid-edit, so
		// gating would be a generator holding a user's file hostage.
		{
			name:  "fixture-drift lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runFixtureDriftLint(rc.cwd, rc.cfg)
			},
			errFormat: "⚠️  fixture-drift lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectFixtureDriftJSON(rc.cwd, rc.cfg)
				return fs, false, err
			},
		},

		// 13d-sexies-ter. Time-bucketing — the reporting-query twin of
		// read-only-fields, and the same silent-wrong-data class. forge
		// maps google.protobuf.Timestamp to TIMESTAMPTZ, which is right,
		// but a two-argument `date_trunc` over one truncates in the
		// SESSION timezone — which the driver sets from the CLIENT HOST.
		// So the same rows bucket differently on a UTC CI box and a
		// UTC-5 laptop, and every reported total is attributed to the
		// wrong day by a fraction of one.
		//
		// Nothing fails: no constraint, no type error, no failing test,
		// no log line. The chart renders and the bars are simply wrong,
		// which reads as flakiness rather than as a timezone bug. It is
		// also squarely in forge's path — the generated ORM cannot
		// express GROUP BY / SUM, so forge itself routes authors to raw
		// SQL for any reporting screen, which is exactly where this
		// construct lives.
		//
		// Warnings only, matching the fixture lanes: a local-time bucket
		// can be deliberate for a shift report or a business-day rollup,
		// and naming the zone explicitly is how the author says so.
		{
			name:  "time-bucketing lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runTimeBucketingLint(rc.cwd, rc.cfg)
			},
			errFormat: "⚠️  time-bucketing lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectTimeBucketingJSON(rc.cwd, rc.cfg)
				return fs, false, err
			},
		},

		// 13d-septies. Guarded-fields — the marker's blind spot. The page
		// generator honours `forge:guards` for every page it emits, but
		// pages are WRITE-IF-ABSENT: the natural order (scaffold CRUD,
		// write the custom rpc, then add the marker) leaves an edit page
		// whose update_mask still names the guarded column, and no
		// regenerate will ever touch it again. The frontend typechecks,
		// nothing logs, and the symptom is a user saving a form and
		// getting a 500 from the raw CHECK the rpc exists to avoid.
		// Warnings only — the file is the user's, so forge must not
		// rewrite it.
		{
			name:  "guarded-fields lint",
			gates: false,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return dirExists(protoDirDefault) && rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runGuardedFieldsLint(rc.cwd, frontendDirsForLint())
			},
			errFormat: "⚠️  guarded-fields lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectGuardedFieldsJSON(rc.cwd, frontendDirsForLint())
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
			scope: scopeByFile,
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

		// 14b. No-dotenv — forge projects declare config in KCL and keep
		// secret VALUES in the secret store. A .env file bypasses
		// both (it is injected wholesale, so nothing has to be
		// declared), so its presence is an error, not a warning.
		{
			name:  "no-dotenv lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return rc.cwd != "", ""
			},
			runText: func(rc *lintRunCtx) error {
				return runNoDotenvLint(rc.cwd)
			},
			errFormat: "❌ no-dotenv lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				fs, err := collectNoDotenvJSON(rc.cwd)
				return fs, len(fs) > 0, err
			},
		},

		// 14c. Commit policy — generated code is committed; machine-local
		// state (.forge-kcl/, a frontend's dev public/config.js) is not.
		// ERROR-gated: an ignored generated file breaks every fresh clone
		// and CI checkout, and `forge ci verify-generated` (git status)
		// cannot see it — this is the check that can. See
		// internal/commitpolicy.
		{
			name:  "commit-policy lint",
			gates: true,
			scope: scopeByFile,
			shouldRun: func(rc *lintRunCtx) (bool, string) {
				return rc.cwd != "" && fileExists("forge.yaml"), ""
			},
			runText: func(rc *lintRunCtx) error {
				return runCommitPolicyLint(rc.cwd)
			},
			errFormat: "❌ commit-policy lint: %v\n",
			collect: func(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
				return collectCommitPolicyJSON(rc.cwd)
			},
		},

		// 14. Check-workarounds — flags canonical cross-lane workarounds.
		// Warnings only.
		{
			name:  "check-workarounds lint",
			gates: false,
			scope: scopeByFile,
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
