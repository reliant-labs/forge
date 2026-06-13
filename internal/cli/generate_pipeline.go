// Package cli — typed step plan for `forge generate`.
//
// Pre-2026-05-06, runGeneratePipeline was a 584-line procedural function
// holding 25 numbered ordered steps gated by 93 Features.*Enabled() checks.
// New steps were 30 lines of boilerplate appended to a numbered-comment
// sequence (Step 0a, 0b, 0b.1, ..., 8d-iii, 8f.1) — the numbering itself
// pleaded for a data structure (FORGE_REVIEW_CODEBASE.md Tier 1.1).
//
// This file replaces that procedural blob with a typed []GenStep plan.
// Each step is a small named function operating on a shared
// pipelineContext. The pipeline becomes a loop over the slice that:
//   - calls step.Gate(ctx) — pure, side-effect-free predicate; false skips
//   - calls step.Run(ctx) — the action, returning an error to abort
//
// The shape unblocks several downstream wins documented in the codebase
// review:
//   - --plan / --explain print the plan without executing it
//   - `forge dev` watch loops re-run only steps whose Tag matches changes
//   - per-step unit tests against a synthetic pipelineContext
//   - one-time parse of services/entities into ctx (avoids re-parsing
//     ParseEntityProtos 3× — see Tier 2.5)
//
// As of 2026-05-07 the entire pre-refactor pipeline is now flat: every
// numbered legacy step has a dedicated stepXxx entry below, and
// runMidPipelineLegacy is gone. The single remaining shared-state hop
// (parse services + module path once for steps 4-6) is its own GenStep.
//
// (2026-05-06 polish-phase, completed 2026-05-07) — closes
// FORGE_REVIEW_CODEBASE.md Tier 1.1.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
)

// GenStep is one ordered unit of the generate pipeline. Steps are pure
// data plus two functions: Gate decides whether the step should run
// for the current pipelineContext (must be side-effect free), and Run
// executes the step. Tag categorizes the step for filtering by future
// callers (--plan output, `forge dev` watch-mode dispatch).
type GenStep struct {
	// Name is the human-readable label printed by --plan / --explain
	// and used in error wrapping. Replaces the legacy "Step Nx" comment
	// numbering — make these stable; tests pin the order.
	Name string

	// Gate returns true when the step should execute for this context.
	// Gates MUST be pure: no I/O, no mutation. They're called by
	// stability tests and may be called repeatedly by future watch-mode
	// dispatchers.
	Gate func(*pipelineContext) bool

	// GateReason is the human-readable explanation rendered when Gate
	// returns false. Printed by `--plan` `[SKIP]` lines and by the
	// `--verbose` `⏩ skipped:` lines instead of the function-name-derived
	// `gate: <gateName>` label, which carried no semantic signal for
	// users debugging "why didn't generate touch X?".
	//
	// Conventions:
	//   - Phrase as the FALSE condition ("no services in forge.yaml",
	//     "features.codegen=false") so the user reads the skip reason
	//     directly without having to mentally invert a positive statement.
	//   - Keep under ~60 chars to fit a single `[SKIP] <name> (<reason>)`
	//     line at the column width `--plan` already uses (44 + reason).
	//   - Empty string falls back to the legacy "gate <gateName> returned
	//     false" rendering — useful during incremental adoption.
	GateReason string

	// Run is the action. It may mutate ctx (parsed services, derived
	// flags, checksums) so subsequent steps can reuse the work. Errors
	// abort the pipeline; warnings should be logged in-step.
	Run func(*pipelineContext) error

	// Tag categorizes the step for future filtering. Conventional
	// values: "config", "proto", "codegen", "migrations", "frontend",
	// "deploy", "tools", "validate".
	Tag string
}

// pipelineContext is the shared state passed between steps. Steps may
// read from it freely and write to it via the explicit fields. Anything
// derived from the project (services, entity defs, module path, has*
// flags) lives here so we parse once at step 0 instead of 3× across
// the pipeline (see FORGE_REVIEW_CODEBASE.md Tier 2.5).
type pipelineContext struct {
	// Inputs (set in newPipelineContext).
	ProjectDir string
	AbsPath    string
	Force      bool
	// Accept is the DEPRECATED --accept alias: disown every drifted
	// Tier-1 file (one-way transfer to user ownership), then proceed
	// with the rest of the pipeline. See stepCheckTier1Drift for the
	// full guard logic; prefer `forge disown <path>... --reason`.
	Accept bool
	// AcceptReason is the --reason text recorded into
	// .forge/friction.jsonl for every path Accept disowns this run. See
	// pipelineFlags.AcceptReason for the design-feedback rationale.
	AcceptReason string

	// SkipValidate suppresses the final `go build ./...` step. The
	// validate step is all-or-nothing — a single broken file in package
	// A blocks the validate gate for any unrelated change in package B.
	// During multi-lane migrations a tree spends extended periods in a
	// partial-build state; the `--skip-validate` flag lets the user run
	// `forge generate` when they know their lane is internal (no
	// proto/contract delta) and the rest of the tree's brokenness is
	// being worked on elsewhere. FRICTION 2026-06-02: cp-forge
	// per-commit port 8050178.
	SkipValidate bool

	// SkipPreChecks bypasses the pre-codegen Step 0c contract-shape
	// check (LintInternalContracts). Same motivation as SkipValidate:
	// during parallel-lane work an unrelated `internal/<other-lane>/
	// contract.go` violation would otherwise block regen across every
	// lane. Distinct from SkipValidate — that one gates the FINAL
	// `go build`; this one gates the PRE-codegen guard. Both are
	// available so the user can opt-out of either side independently.
	SkipPreChecks bool

	// SkipConfigCheck bypasses the forge.yaml ↔ filesystem cross-check
	// that stepLoadConfig runs after a successful LoadStrict. See the
	// SkipConfigCheck field on pipelineFlags for the rationale.
	SkipConfigCheck bool

	// Strict promotes "Warning: ... failed" sites into hard errors via
	// the warnOrFail helper. See pipelineFlags.Strict for the rationale.
	Strict bool

	// Verbose toggles per-step skip messages. See pipelineFlags.Verbose
	// for the rationale; consumed by the gate-loop in runGeneratePipelineFlags.
	Verbose bool

	// ForceCleanup opts in to the destructive stale-artifact sweep. The
	// historical behavior (delete every manifest-recorded path the run
	// did not re-emit) was load-bearing for cp-forge-style projects but
	// surprised users running `forge generate` while iterating on a
	// service rename — a single mistimed run could nuke files the user
	// hadn't realized were stale. The default is now report-only:
	// stepCleanupStale walks the same candidates and prints a warning
	// listing what WOULD be deleted, but leaves the files in place.
	// Pass --force-cleanup to actually delete.
	ForceCleanup bool

	// TemplatesOnly narrows the pipeline to template-driven render
	// steps. See the matching pipelineFlags.TemplatesOnly field for the
	// full rationale and the templatesOnlyStepAllow allowlist.
	TemplatesOnly bool

	// ExplainDrift means: on Tier-1 drift, don't abort at the guard —
	// redirect drifted paths' writes to .forge/render/ side renders,
	// run the pipeline, diff on-disk vs fresh render at the end, then
	// fail with the standard drift report. Explains, never approves.
	// Mechanics live in generate_explain_drift.go.
	ExplainDrift bool

	// ExplainDriftEntries is populated by prepareExplainDrift when
	// ExplainDrift triggers: the drift set the guard detected. The
	// drifted files themselves are never touched (their writes are
	// side-render redirected), so no state snapshot is needed — the
	// truth lives in the files.
	ExplainDriftEntries []checksums.Tier1DriftEntry

	// LegacyUnverified is populated by the one-time legacy-manifest
	// migration (generate_legacy_migrate.go): paths whose on-disk bytes
	// matched nothing the legacy manifest recorded (the fr-9a54388f0b
	// different-lane corruption). Their writes are side-render
	// redirected this run; finishLegacyMigration adjudicates each at
	// the end (fresh-render body match → pristine, stamp; otherwise →
	// unverified-legacy sentinel + drift report).
	LegacyUnverified []string

	// Cfg may be nil — that's the directory-scan fallback path.
	Cfg *config.ProjectConfig

	// Checksums is loaded once at step 0b and saved on pipeline exit
	// by the caller. Steps mutate this in-place via WriteGeneratedFile
	// helpers and the Tier-1 emitters.
	Checksums *generator.FileChecksums

	// Proto-tree presence flags — populated once in stepDetectProtoDirs.
	HasServices  bool
	HasAPI       bool
	HasDB        bool
	HasConfig    bool
	HasWorkers   bool
	HasOperators bool

	// Parsed once and reused by mid-pipeline steps. Nil until populated.
	Services     []codegen.ServiceDef
	ModulePath   string
	EntityDefs   []codegen.EntityDef
	ConfigFields map[string]bool

	// registry is the memoized parse of the user-owned
	// pkg/app/services.go registration file (what this binary serves —
	// see generate_serve.go). Lazily populated via
	// ctx.serviceRegistry(); registryErr is a sticky parse failure so
	// every consuming step reports the same pointed error.
	registry       *serviceRegistry
	registryErr    error
	registryLoaded bool

	// PriorExports holds the pre-codegen snapshot of each Tier-1 Go
	// file's exported top-level identifier names. Populated by
	// stepSnapshotTier1Exports before any codegen step runs; consumed
	// by stepDetectRenamedExports after the codegen passes to diff
	// against the freshly written files. The diff drives the rename-
	// detection warnings (callers of a dropped name may be orphaned).
	// FRICTION 2026-06-02: cp-forge `forgedb.Migrations()` →
	// `forgedb.MigrationsFS` rename left internal/db/migrations.go
	// orphaned with a silent compile error two runs later.
	PriorExports map[string]tier1Exports
}

// tier1Exports is the per-path snapshot captured pre-codegen:
// the file's package name + the sorted list of public top-level
// identifiers. PkgName drives `pkg.Name` search patterns when looking
// for stale callers; Names is the diff-source list.
type tier1Exports struct {
	PkgName string
	Names   []string
}

// newPipelineContext builds the initial context. The caller (the cobra
// RunE) wires projectDir + force + accept; everything else is filled by
// the early steps so a unit test can construct a synthetic context
// without touching disk.
func newPipelineContext(projectDir string, force, accept bool) (*pipelineContext, error) {
	return newPipelineContextWithOpts(projectDir, force, accept, false)
}

// newPipelineContextWithOpts builds the initial context with explicit
// control over every flag. Exposed so the cobra RunE can plumb through
// flags like --skip-validate without growing the older newPipelineContext
// signature that test fixtures rely on.
func newPipelineContextWithOpts(projectDir string, force, accept, skipValidate bool) (*pipelineContext, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project dir: %w", err)
	}
	return &pipelineContext{
		ProjectDir:   projectDir,
		AbsPath:      abs,
		Force:        force,
		Accept:       accept,
		SkipValidate: skipValidate,
	}, nil
}

// newPipelineContextWithFlags is the typed-flags variant. New optional
// pipeline toggles land on the pipelineFlags struct rather than as a new
// positional argument — keeps the call-site self-documenting and avoids
// churning every test fixture when a flag is added.
func newPipelineContextWithFlags(projectDir string, flags pipelineFlags) (*pipelineContext, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project dir: %w", err)
	}
	return &pipelineContext{
		ProjectDir:      projectDir,
		AbsPath:         abs,
		Force:           flags.Force,
		Accept:          flags.Accept,
		AcceptReason:    flags.AcceptReason,
		ExplainDrift:    flags.ExplainDrift,
		SkipValidate:    flags.SkipValidate,
		SkipPreChecks:   flags.SkipPreChecks,
		SkipConfigCheck: flags.SkipConfigCheck,
		ForceCleanup:    flags.ForceCleanup,
		TemplatesOnly:   flags.TemplatesOnly,
		Strict:          flags.Strict,
		Verbose:         flags.Verbose,
	}, nil
}

// generateSteps returns the ordered step plan. The slice is the
// authoritative ordering — comments alongside each entry replace the
// pre-refactor "Step Nx" numbering. Reorder cautiously; the
// TestGenerateStepsPlanStable test pins the exact sequence.
//
// EXTRACTION STATUS (2026-05-07): complete. Every pre-refactor numbered
// step is now its own GenStep entry; runMidPipelineLegacy has been
// removed. The only step that touches shared state across step
// boundaries (stepParseServicesAndModule) is documented inline.
func generateSteps() []GenStep {
	return []GenStep{
		{Name: "load project config", Gate: always, Run: stepLoadConfig, Tag: "config"},
		{Name: "load checksums", Gate: always, Run: stepLoadChecksums, Tag: "config"},
		// One-time conversion off the dead global manifest. Must run
		// between state load and the Tier-1 stomp guard: pristine files
		// get their forge:hash marker stamped here so the guard reads
		// the right ownership story. See generate_legacy_migrate.go.
		{Name: "migrate legacy checksums manifest", Gate: always, Run: stepMigrateLegacyManifest, Tag: "config"},
		{Name: "check Tier-1 file-stomp guard", Gate: always, Run: stepCheckTier1Drift, Tag: "validate"},
		{Name: "snapshot Tier-1 exports", Gate: always, Run: stepSnapshotTier1Exports, Tag: "validate"},
		{Name: "sync forge/pkg dev replace", Gate: always, Run: stepSyncDevForgePkg, Tag: "config"},
		{Name: "announce project", Gate: always, Run: stepAnnounceProject, Tag: "config"},
		{Name: "pre-codegen contract check", Gate: gatePreChecksNotSkipped, GateReason: "--skip-pre-checks was passed", Run: stepPreCodegenContractCheck, Tag: "validate"},
		{Name: "detect proto directories", Gate: always, Run: stepDetectProtoDirs, Tag: "proto"},
		{Name: "ensure gen/go.mod", Gate: always, Run: stepEnsureGenModule, Tag: "config"},
		{Name: "buf generate (Go stubs)", Gate: gateCodegenEnabled, GateReason: "features.codegen=false", Run: stepBufGenerateGo, Tag: "proto"},
		{Name: "descriptor extraction", Gate: gateCodegenEnabled, GateReason: "features.codegen=false", Run: stepDescriptorGenerate, Tag: "proto"},
		{Name: "OpenAPI specs (protoc-gen-connect-openapi)", Gate: gateOpenAPIEnabled, GateReason: "api.openapi=false or features.codegen=false", Run: stepOpenAPIGenerate, Tag: "proto"},
		{Name: "frontend workspaces scaffold", Gate: gateFrontendEnabled, GateReason: "features.frontend=false or no forge.yaml", Run: stepFrontendWorkspaces, Tag: "frontend"},
		{Name: "TypeScript stubs (frontends)", Gate: gateFrontendEnabled, GateReason: "features.frontend=false or no forge.yaml", Run: stepFrontendBufTS, Tag: "frontend"},
		{Name: "config loader (proto/config)", Gate: gateCodegenHasConfig, GateReason: "no proto/config/ directory or features.codegen=false", Run: stepConfigLoader, Tag: "codegen"},
		{Name: "parse services + module path", Gate: gateNeedsServices, GateReason: "no services/workers/operators or features.codegen=false", Run: stepParseServicesAndModule, Tag: "codegen"},
		{Name: "frontend hooks", Gate: gateFrontendHasServices, GateReason: "no services in forge.yaml or features.frontend=false", Run: stepFrontendHooks, Tag: "frontend"},
		{Name: "ensure frontend components", Gate: gateFrontendHasFrontends, GateReason: "no frontends in forge.yaml or features.frontend=false", Run: stepFrontendComponents, Tag: "frontend"},
		{Name: "frontend CRUD pages", Gate: gateFrontendHasServices, GateReason: "no services in forge.yaml or features.frontend=false", Run: stepFrontendPages, Tag: "frontend"},
		{Name: "frontend nav + dashboard", Gate: gateFrontendHasFrontends, GateReason: "no frontends in forge.yaml or features.frontend=false", Run: stepFrontendNav, Tag: "frontend"},
		{Name: "service stubs", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepServiceStubs, Tag: "codegen"},
		{Name: "internal/db/ ORM (entity-driven)", Gate: gateORMHasServices, GateReason: "no proto/services/ directory or features.orm=false", Run: stepInternalDBORM, Tag: "codegen"},
		{Name: "CRUD handlers", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepCRUDHandlers, Tag: "codegen"},
		{Name: "authorizer", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepAuthorizer, Tag: "codegen"},
		{Name: "service mocks", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepServiceMocks, Tag: "codegen"},
		{Name: "internal package contracts", Gate: gateContractsEnabled, GateReason: "features.contracts=false", Run: stepInternalContracts, Tag: "codegen"},
		{Name: "auth middleware", Gate: gateAuthProviderConfigured, GateReason: "auth.provider unset or 'none'", Run: stepAuthMiddleware, Tag: "codegen"},
		{Name: "tenant middleware (auto-enable + emit)", Gate: gateCodegenHasServicesCfg, GateReason: "no services or no forge.yaml or features.codegen=false", Run: stepTenantMiddleware, Tag: "codegen"},
		{Name: "webhook routes", Gate: gateCodegenHasCfg, GateReason: "no forge.yaml or features.codegen=false", Run: stepWebhookRoutes, Tag: "codegen"},
		{Name: "MCP manifest", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepMCPManifest, Tag: "codegen"},
		{Name: "go mod tidy (pre-wiring)", Gate: gateCodegenHasAnyEntrypoint, GateReason: "no services/workers/operators or features.codegen=false", Run: stepGoModTidyPreWiring, Tag: "tools"},
		{Name: "pkg/app/bootstrap.go", Gate: gateCodegenHasAnyEntrypoint, GateReason: "no services/workers/operators or features.codegen=false", Run: stepBootstrap, Tag: "codegen"},
		{Name: "per-service subcommands (cmd/services_gen.go)", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepCmdSubcommands, Tag: "codegen"},
		{Name: "pkg/app/testing.go", Gate: gateCodegenHasAnyEntrypoint, GateReason: "no services/workers/operators or features.codegen=false", Run: stepBootstrapTesting, Tag: "codegen"},
		{Name: "pkg/app/migrate.go", Gate: gateMigrateHasDriver, GateReason: "database.driver unset or features.migrations=false", Run: stepBootstrapMigrate, Tag: "codegen"},
		{Name: "sqlc generate", Gate: always, Run: stepSqlcGenerate, Tag: "tools"},
		{Name: "go mod tidy (gen/)", Gate: always, Run: stepGoModTidyGen, Tag: "tools"},
		{Name: "CI workflows", Gate: gateCIWorkflows, GateReason: "no forge.yaml or features.ci=false", Run: stepCIWorkflows, Tag: "deploy"},
		{Name: "pack generate hooks", Gate: gateHasPacks, GateReason: "no packs installed or features.packs=false", Run: stepPackGenerateHooks, Tag: "codegen"},
		{Name: "regenerate infra files", Gate: gateDeployEnabled, GateReason: "features.deploy=false", Run: stepRegenerateInfra, Tag: "deploy"},
		{Name: "per-env deploy config", Gate: gateDeployHasConfig, GateReason: "no proto/config/ directory or features.deploy=false", Run: stepPerEnvDeployConfig, Tag: "deploy"},
		{Name: "ingress k3d ports fragment", Gate: gateIngressEnabled, GateReason: "features.ingress=false or features.deploy=false", Run: stepIngressK3dPorts, Tag: "deploy"},
		{Name: "Grafana dashboards", Gate: gateObservabilityHasCfg, GateReason: "no forge.yaml or features.observability=false", Run: stepGrafanaDashboards, Tag: "deploy"},
		{Name: "entity-aware seed data", Gate: gateMigrationsHasDBOrServices, GateReason: "no proto/db or proto/services or features.migrations=false", Run: stepEntitySeeds, Tag: "migrations"},
		{Name: "frontend mocks + transport", Gate: gateFrontendHasFrontends, GateReason: "no frontends in forge.yaml or features.frontend=false", Run: stepFrontendMocks, Tag: "frontend"},
		{Name: "agent skills (.claude/skills)", Gate: always, Run: stepAgentSkills, Tag: "tools"},
		{Name: "go mod tidy (root)", Gate: always, Run: stepGoModTidyRoot, Tag: "tools"},
		{Name: "goimports on generated Go", Gate: always, Run: stepGoimports, Tag: "tools"},
		{Name: "cleanup stale codegen", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepCleanupStale, Tag: "codegen"},
		{Name: "rehash tracked files", Gate: always, Run: stepRehashTracked, Tag: "tools"},
		{Name: "post-gen validation", Gate: always, Run: stepPostGenValidate, Tag: "validate"},
		{Name: "detect renamed Tier-1 exports", Gate: always, Run: stepDetectRenamedExports, Tag: "validate"},
		{Name: "check disowned-sibling dangling refs", Gate: always, Run: stepCheckDisownedDanglingRefs, Tag: "validate"},
		// Runs after the codegen steps so gen/ reflects the current protos.
		// One-shot scaffold tests are user-owned and never regenerated;
		// `go build` (the validate step) skips _test.go files, so a stale
		// pb reference there would otherwise surface only at the user's
		// next `go test`. See generate_stale_scaffold.go.
		{Name: "check stale scaffold tests", Gate: gateCodegenHasServices, GateReason: "no proto/services/ directory or features.codegen=false", Run: stepCheckStaleScaffoldTests, Tag: "validate"},
		{Name: "go build (validate generated code)", Gate: gateValidateNotSkipped, GateReason: "--skip-validate was passed", Run: stepGoBuildValidate, Tag: "validate"},
	}
}

// stepPresetAllowlist maps a pipelineFlags.Steps value (a named "step
// preset") to the set of step.Name values the runner is allowed to
// execute under that preset. An empty Steps value (the historical
// default) bypasses this map entirely and runs every step that passes
// its Gate.
//
// Naming note: this used to be called "scope", but the word "scope" is
// overloaded across forge — it's load-bearing for the file-ownership
// concept addressed by the Tier-1 inspector in internal/checksums/
// inspector.go. The pipeline-step concept got renamed to "step preset"
// so "scope" stays free for the file-ownership concept where it
// carries weight.
//
// Adding a new step preset: pick a stable name, enumerate the
// step.Name values the preset covers, and document the caller's intent
// in the comment. Step names must match generateSteps() exactly — the
// TestStepPresetAllowlistMembersExist test verifies this so a typo or
// a step rename doesn't silently produce a no-op pipeline.
//
// The "bootstrap-only" preset covers `forge add worker`: scaffold a
// new worker, regenerate ONLY pkg/app/{bootstrap,testing,migrate}.go
// and the validation tail, then exit. Sibling Tier-1 files
// (.github/workflows/ci.yml, cmd/server.go, frontend mocks,
// pkg/config/config.go) stay untouched so a sibling agent or hand-
// curated comment isn't stomped. FRICTION 2026-06-03: cp-forge
// port-workers ran `forge add worker` 7× and watched regen rewrite 5
// unrelated files per call.
var stepPresetAllowlist = map[string]map[string]bool{
	"bootstrap-only": {
		"load project config":                true,
		"load checksums":                     true,
		"check Tier-1 file-stomp guard":      true,
		"snapshot Tier-1 exports":            true,
		"sync forge/pkg dev replace":         true,
		"announce project":                   true,
		"detect proto directories":           true,
		"ensure gen/go.mod":                  true,
		"parse services + module path":       true,
		"go mod tidy (pre-wiring)":           true,
		"pkg/app/bootstrap.go":               true,
		"pkg/app/testing.go":                 true,
		"pkg/app/migrate.go":                 true,
		"go mod tidy (gen/)":                 true,
		"go mod tidy (root)":                 true,
		"goimports on generated Go":          true,
		"rehash tracked files":               true,
		"post-gen validation":                true,
		"detect renamed Tier-1 exports":      true,
		"check disowned-sibling dangling refs": true,
		"go build (validate generated code)": true,
	},
	// The "mocks" step preset covers the fast-path "I just edited
	// contract.go, regenerate mock_gen.go" workflow. Mocks live behind a
	// "DO NOT EDIT" banner and are deterministic from contract.go + the
	// service proto; they cannot stomp any Tier-1 file. So:
	//
	//   - Skip "check Tier-1 file-stomp guard" entirely. The guard exists
	//     to protect Tier-1 files from being overwritten by codegen
	//     emitters; the mock emitter only touches internal/<svc>/mock_gen.go,
	//     which is itself Tier-1 owned by forge. Forcing the user to
	//     reconcile unrelated Tier-1 drift (a hand-edited workflow yaml,
	//     a touched cmd/server.go) before they can regen a mock has no
	//     payoff — there's no file the mock step could clobber that the
	//     unrelated edits put at risk.
	//
	//   - Run only the strictly required prereqs for stepServiceMocks:
	//     load config + checksums (so the writer has somewhere to record),
	//     detect proto directories (HasServices feeds the gate),
	//     ensure gen/go.mod (parser-side dependency), parse services
	//     + module path (the mock emitter walks ctx.Services), and the
	//     mock step itself. Goimports + rehash tail keeps the just-
	//     written file consistent with the audit machinery, but we skip
	//     the validate `go build ./...` and the post-gen heuristic
	//     warnings — the user already has a tight inner-loop ("contract
	//     change → regen mock → run unit tests") that runs `go build`
	//     itself.
	//
	// FRICTION 2026-06-04: two downstream projects reported that they
	// could not regen mock_gen.go after a contract.go change without
	// first reconciling unrelated Tier-1 file edits sitting in their
	// tree (e.g. modified .github/workflows/ci.yml). The mocks-only
	// step preset makes the inner-loop hermetic.
	"mocks": {
		"load project config":          true,
		"load checksums":               true,
		"detect proto directories":     true,
		"ensure gen/go.mod":            true,
		"parse services + module path": true,
		"service mocks":                true,
		"goimports on generated Go":    true,
		"rehash tracked files":         true,
	},
}

// templatesOnlyStepAllow is the set of step.Name values that run when
// pipelineFlags.TemplatesOnly is set. The list is the "template-driven"
// subset: every step whose work is rendering a forge-owned template
// (Tier-1 codegen, Tier-2 scaffolds, infra/CI yaml, frontend hooks/nav/
// mocks, KCL per-env config). Everything else — drift guards,
// validation, external subprocess generators (buf/protoc/sqlc/
// goimports/go mod tidy/KCL render), and the cleanup sweep — is
// excluded.
//
// Use case: a forge template change (e.g. bootstrap.go.tmpl gets a
// louder warning) needs to land in a project with WIP that can't
// tolerate a full regen. The full pipeline would either trip the
// Tier-1 drift guard on the user's WIP edits, or shell out to
// tooling the partial tree can't build, or surprise-delete a stale
// file that the WIP migration hasn't re-created yet. Filtering the
// pipeline down to just the template-emit steps lets the new template
// propagate without disturbing the rest of the tree.
//
// Excluded categories (with the step.Name values dropped):
//
//   - Drift / Tier-1 guards: "check Tier-1 file-stomp guard",
//     "snapshot Tier-1 exports", "detect renamed Tier-1 exports",
//     "check disowned-sibling dangling refs".
//   - Validation: "pre-codegen contract check", "post-gen validation",
//     "go build (validate generated code)".
//   - External generators: "buf generate (Go stubs)",
//     "descriptor extraction", "OpenAPI specs (protoc-gen-connect-openapi)",
//     "ORM generate (proto/db)", "TypeScript stubs (frontends)",
//     "sqlc generate", "go mod tidy (gen/)", "go mod tidy (root)",
//     "goimports on generated Go", "refresh ORM output mtimes",
//     "ingress k3d ports fragment" (calls `kcl run`).
//   - Cleanup: "cleanup stale codegen", "rehash tracked files" (the
//     rehash only matters when goimports has run; without goimports
//     it would mis-stamp the just-written files).
//   - Migration scaffolding: "initial migration scaffold",
//     "entity-aware migration", "entity-aware seed data" — these
//     mutate db/migrations and would be unsafe to fire mid-WIP.
//
// Composes with stepPresetAllowlist (--steps): when both are set, a
// step must pass BOTH allowlists to run (intersection). That makes
// `--steps=bootstrap-only --templates-only` a well-defined narrower
// run rather than a precedence riddle.
var templatesOnlyStepAllow = map[string]bool{
	"load project config":                    true,
	"load checksums":                         true,
	"migrate legacy checksums manifest":      true,
	"sync forge/pkg dev replace":             true,
	"announce project":                       true,
	"detect proto directories":               true,
	"ensure gen/go.mod":                      true,
	"frontend workspaces scaffold":           true,
	"config loader (proto/config)":           true,
	"parse services + module path":           true,
	"frontend hooks":                         true,
	"ensure frontend components":             true,
	"frontend CRUD pages":                    true,
	"frontend nav + dashboard":               true,
	"service stubs":                          true,
	"internal/db/ ORM (entity-driven)":       true,
	"CRUD handlers":                          true,
	"authorizer":                             true,
	"service mocks":                          true,
	"internal package contracts":             true,
	"auth middleware":                        true,
	"tenant middleware (auto-enable + emit)": true,
	"webhook routes":                         true,
	"MCP manifest":                           true,
	"pkg/app/bootstrap.go":                   true,
	"per-service subcommands (cmd/services_gen.go)": true,
	"pkg/app/testing.go":                     true,
	"pkg/app/migrate.go":                     true,
	"CI workflows":                           true,
	"pack generate hooks":                    true,
	"regenerate infra files":                 true,
	"per-env deploy config":                  true,
	"Grafana dashboards":                     true,
	"frontend mocks + transport":             true,
}

// knownStepPresetNames returns a comma-joined string of every
// registered step preset for error messages. Kept as a helper so the
// error in runGeneratePipelineFlags doesn't have to inline a sort +
// join.
func knownStepPresetNames() string {
	names := make([]string, 0, len(stepPresetAllowlist))
	for k := range stepPresetAllowlist {
		names = append(names, k)
	}
	// Tiny sort to keep the error message deterministic.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return strings.Join(names, ", ")
}

// always is the trivial gate. Used for steps that are unconditional
// (config loading, checksums, build validation) or whose internal
// no-op-when-not-applicable behavior matches the pre-refactor shape.
func always(_ *pipelineContext) bool { return true }

// gateValidateNotSkipped suppresses the final `go build ./...` step
// when the user passed `--skip-validate`. See the SkipValidate field
// doc on pipelineContext for the per-lane-migration rationale.
func gateValidateNotSkipped(ctx *pipelineContext) bool {
	return !ctx.SkipValidate
}

// gatePreChecksNotSkipped suppresses the pre-codegen Step 0c contract-
// shape check when --skip-pre-checks is set. The caller already prints a
// warn line about the bypass in runGeneratePipelineFlags so users see
// the skip in their generate output.
func gatePreChecksNotSkipped(ctx *pipelineContext) bool {
	return !ctx.SkipPreChecks
}

// Gate helpers. Pure predicates over ctx — no I/O. Tests assert these
// don't mutate ctx by calling them twice and comparing field-wise.

func gateCodegenEnabled(ctx *pipelineContext) bool {
	return ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()
}

// gateOpenAPIEnabled fires the OpenAPI spec step. Off by default — the
// flag is opt-in (api.openapi: true in forge.yaml) so existing projects
// regenerate byte-identically until a user opts in. Requires both the
// codegen feature and the explicit flag because the spec consumes the
// same proto inputs as the Go-stub buf step and there's no value in
// emitting a spec if we're not also emitting the handlers it documents.
func gateOpenAPIEnabled(ctx *pipelineContext) bool {
	if ctx.Cfg == nil {
		return false
	}
	if !ctx.Cfg.Features.CodegenEnabled() {
		return false
	}
	return ctx.Cfg.API.OpenAPI
}

func gateORMHasDB(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.ORMEnabled()) && ctx.HasDB
}

func gateORMHasServices(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.ORMEnabled()) && ctx.HasServices
}

func gateCIWorkflows(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.CIEnabled()) && ctx.Cfg != nil
}

func gateHasPacks(ctx *pipelineContext) bool {
	// Pack generate hooks only fire when the project has packs installed
	// AND the packs feature is on. Disabling the feature with packs
	// already installed skips the regenerate-on-generate step silently —
	// the user opted out of the subsystem, codegen respects it. The
	// installed packs themselves stay on disk; flipping `features.packs:
	// true` later resumes the generate hooks without losing state.
	return ctx.Cfg != nil &&
		ctx.Cfg.Features.PacksEnabled() &&
		len(ctx.Cfg.Packs) > 0
}

func gateDeployEnabled(ctx *pipelineContext) bool {
	return ctx.Cfg == nil || ctx.Cfg.Features.DeployEnabled()
}

// gateIngressEnabled controls Gateway API codegen — the k3d-ports
// fragment and (in later phases) other ingress-derived artifacts.
// Off when features.ingress is explicitly false OR features.deploy
// is off (no cluster, no k3d, no ingress to wire up).
func gateIngressEnabled(ctx *pipelineContext) bool {
	if ctx.Cfg == nil {
		return false
	}
	return ctx.Cfg.Features.DeployEnabled() && ctx.Cfg.Features.IngressEnabled()
}

func gateDeployHasConfig(ctx *pipelineContext) bool {
	return ctx.Cfg != nil && ctx.Cfg.Features.DeployEnabled() && ctx.HasConfig
}

func gateObservabilityHasCfg(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.ObservabilityEnabled()) && ctx.Cfg != nil
}

func gateMigrationsHasDBOrServices(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.MigrationsEnabled()) && (ctx.HasDB || ctx.HasServices)
}

func gateFrontendEnabled(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.FrontendEnabled()) && ctx.Cfg != nil
}

func gateFrontendHasFrontends(ctx *pipelineContext) bool {
	return ctx.Cfg != nil && ctx.Cfg.Features.FrontendEnabled() && len(ctx.Cfg.Frontends) > 0
}

func gateFrontendHasServices(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.FrontendEnabled()) && ctx.Cfg != nil && ctx.HasServices
}

func gateCodegenHasConfig(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()) && ctx.HasConfig
}

func gateCodegenHasServices(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()) && ctx.HasServices
}

func gateCodegenHasServicesCfg(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()) && ctx.Cfg != nil && ctx.HasServices
}

func gateCodegenHasCfg(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()) && ctx.Cfg != nil
}

func gateContractsEnabled(ctx *pipelineContext) bool {
	return ctx.Cfg == nil || ctx.Cfg.Features.ContractsEnabled()
}

func gateAuthProviderConfigured(ctx *pipelineContext) bool {
	if ctx.Cfg == nil {
		return false
	}
	if !ctx.Cfg.Features.CodegenEnabled() {
		return false
	}
	return ctx.Cfg.Auth.Provider != "" && ctx.Cfg.Auth.Provider != "none"
}

func gateCodegenHasAnyEntrypoint(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()) && (ctx.HasServices || ctx.HasWorkers || ctx.HasOperators)
}

func gateMigrateHasDriver(ctx *pipelineContext) bool {
	if ctx.Cfg == nil {
		return false
	}
	return ctx.Cfg.Features.MigrationsEnabled() && ctx.Cfg.Database.Driver != ""
}

// gateNeedsServices triggers the parse-services step. We need to parse
// when the project has either proto/services/ (handlers, mocks, CRUD,
// auth, bootstrap all read ctx.Services) OR workers/operators (bootstrap
// still needs ModulePath, even if Services is empty). Codegen-disabled
// projects skip it: the consumers of ctx.Services are all themselves
// codegen-gated.
func gateNeedsServices(ctx *pipelineContext) bool {
	if ctx.Cfg != nil && !ctx.Cfg.Features.CodegenEnabled() {
		return false
	}
	return ctx.HasServices || ctx.HasWorkers || ctx.HasOperators
}

// ──────────────────────────────────────────────────────────────────────
// Step implementations.
//
// Each step is a small function over *pipelineContext returning error.
// The pre-refactor numbered comments are inlined here as the step's
// doc comment, then dropped from the call-site so generate.go's loop
// stays uncluttered.
// ──────────────────────────────────────────────────────────────────────

// stepLoadConfig — was Step 0a.
// Loads forge.yaml. Missing-file is treated as the directory-scan
// fallback path (ctx.Cfg stays nil); other errors abort.
//
// After a successful load, runs validateConfigVsFilesystem to cross-
// check declarations against on-disk reality (loud-by-default
// architecture). Pass --skip-config-check to bypass.
func stepLoadConfig(ctx *pipelineContext) error {
	cfg, err := loadProjectConfigFrom(filepath.Join(ctx.ProjectDir, defaultProjectConfigFile))
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		return fmt.Errorf("failed to load project config: %w", err)
	}
	if errors.Is(err, ErrProjectConfigNotFound) {
		// Announce the fallback so users running outside a forge project
		// (or in a tree where forge.yaml was accidentally deleted) don't
		// silently get the directory-scan path. The historical behavior
		// was a fully-silent fallback that turned every "I deleted
		// forge.yaml by mistake" into "why is my generate emitting
		// nothing structural?" — see B2 in the loud-by-default backlog.
		fmt.Fprintln(os.Stderr, "ℹ️  No forge.yaml found; using directory convention scanning (proto/, proto/services/, proto/api/, proto/db/)")
		ctx.Cfg = nil
		return nil
	}
	ctx.Cfg = cfg

	// Loud-by-default config ↔ filesystem cross-check. Failures here are
	// hard errors so the user fixes the asymmetry at load time, not at a
	// confusing downstream "missing import" failure. See
	// generate_config_check.go for the rule set.
	if !ctx.SkipConfigCheck {
		if err := validateConfigVsFilesystem(ctx.ProjectDir, cfg); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "⚠️  --skip-config-check: forge.yaml ↔ filesystem cross-check bypassed")
	}
	return nil
}

// stepLoadChecksums — was Step 0b.
// Loads the .forge ownership state (disowned.json + hashes.json) and
// stamps the active forge version. The
// caller (runGeneratePipeline) is responsible for calling SaveChecksums
// on exit so partial pipeline runs still persist what they wrote.
//
// Also resets the per-run written-this-run set in the checksums package
// — pipeline-run state, not persistent state, so it must start empty on
// every invocation.
func stepLoadChecksums(ctx *pipelineContext) error {
	cs, err := generator.LoadChecksums(ctx.AbsPath)
	if err != nil {
		return fmt.Errorf("failed to load checksums: %w", err)
	}
	cs.ForgeVersion = buildinfo.Version()
	ctx.Checksums = cs
	checksums.ResetSkipWrite()
	return nil
}

// stepCheckTier1Drift is the pre-pipeline file-stomp guard. Before any
// codegen step runs, scan the project for self-certifying Tier-1 files
// (embedded forge:hash markers) and report every one whose marker fails
// verification — positive evidence of a hand-edit (LLM or human). The
// check is purely local to each file, so it gives the same answer in a
// fresh clone, a parallel work lane, or a partially-committed tree. On
// any hit, abort the pipeline with a single batched report listing
// every conflict — the user gets one error to react to, not N runs to
// discover N conflicts.
//
// Escape hatches:
//
//   - move the customization into the designated extension point (or any
//     Tier-2 file) — the preferred answer, regeneration keeps working.
//   - --force: skip the check and let codegen overwrite. Documents the
//     "throw my changes away and regenerate from the templates" intent.
//   - `forge disown <path> --reason "<why>"`: one-way transfer to user
//     ownership. (--accept is the deprecated whole-set alias.)
//
// Tier-2 files are intentionally not checked: Tier-2 means "scaffold
// once, never overwrite", so a user edit IS the steady state.
func stepCheckTier1Drift(ctx *pipelineContext) error {
	if ctx.Checksums == nil {
		return nil
	}
	if ctx.Force {
		// Scope --force before anything else: an installed-but-empty
		// scope makes force INERT until this guard widens it to the
		// exact drift set it reports below. --force means "discard the
		// edits the guard told me about", never "overwrite anything any
		// emitter touches this run" — journey fr-a04f8c0609 watched a
		// --force recovery from one Tier-1 trip clobber unrelated
		// files. (Presets that deliberately exclude this guard keep the
		// legacy unscoped force: no scope is installed at all.)
		checksums.SetForceScope(nil)
	}
	allDrift := scanProjectDrift(ctx.AbsPath, ctx.Checksums)
	// Paths quarantined by the legacy-manifest migration are this run's
	// side-render rescue candidates — finishLegacyMigration adjudicates
	// them after the emitters have produced fresh renders to compare
	// against. Reporting them here would abort before the rescue runs.
	if len(ctx.LegacyUnverified) > 0 {
		quarantined := make(map[string]bool, len(ctx.LegacyUnverified))
		for _, p := range ctx.LegacyUnverified {
			quarantined[p] = true
		}
		kept := allDrift[:0]
		for _, d := range allDrift {
			if !quarantined[d.Path] {
				kept = append(kept, d)
			}
		}
		allDrift = kept
	}
	if len(allDrift) == 0 {
		return nil
	}
	// The scope filter's gates consult ctx.HasServices / HasWorkers /
	// HasOperators, which stepDetectProtoDirs only populates LATER in
	// the pipeline. Populate them here first — otherwise every gate
	// that branches on component presence reads false at guard time
	// and in-scope drift (pkg/app/wire_gen.go on a full run!) gets
	// waved through as out-of-scope, letting the emitters silently
	// stomp the user's hand edits.
	populateComponentPresence(ctx)
	// Scope drift to files this run's enabled emitters would actually
	// touch. Out-of-scope drift is announced as a warning (so a parallel
	// lane's hand-edit doesn't go fully silent) but does not block the
	// pipeline. See generate_tier1_scope.go for the registry rationale.
	drift, outOfScope := filterTier1DriftInScope(ctx, allDrift,
		func(d checksums.Tier1DriftEntry) string { return d.Path })
	if len(outOfScope) > 0 {
		// Friendly heads-up rather than a bare warning: out-of-scope drift
		// is the common case when iterating with `--steps=…` and the user
		// needs to know (a) what "drifted" means here and (b) how to
		// resolve it. Two escape hatches mirror the in-scope branch.
		fmt.Fprintf(os.Stderr, "ℹ️  Tier-1 drift detected in %d file(s) — skipped because their emitter step is not in this run's scope:\n", len(outOfScope))
		for _, d := range outOfScope {
			fmt.Fprintf(os.Stderr, "   - %s\n", d.Path)
		}
		fmt.Fprintf(os.Stderr, "   To regenerate them, re-run without `--steps=…` (or run `forge disown <path> --reason \"<why>\"` to permanently take ownership).\n")
	}
	if len(drift) == 0 {
		return nil
	}

	if ctx.Force {
		// Widen the force scope to EXACTLY the in-scope drift set — the
		// files this message names are the only ones --force may
		// clobber. Out-of-scope drift stays outside the scope too: its
		// emitters won't run this invocation, and the warning above
		// already routed the user to a scope-complete re-run.
		paths := make([]string, 0, len(drift))
		for _, d := range drift {
			paths = append(paths, d.Path)
		}
		checksums.SetForceScope(paths)
		fmt.Fprintf(os.Stderr, "⚠️  --force: overwriting %d hand-edited Tier-1 file(s) (and nothing else):\n", len(drift))
		for _, d := range drift {
			fmt.Fprintf(os.Stderr, "   - %s\n", d.Path)
		}
		return nil
	}

	if ctx.Accept {
		// DEPRECATED alias: --accept disowns the entire drifted set —
		// each file flips to Tier-2 (user-owned, disowned marker) and
		// forge never touches it again. The cobra layer already printed
		// the deprecation line and enforced --reason.
		disowned := make([]string, 0, len(drift))
		for _, d := range drift {
			disowned = append(disowned, d.Path)
		}
		if err := ctx.Checksums.DisownPaths(ctx.AbsPath, disowned, ctx.AcceptReason); err != nil {
			return fmt.Errorf("disown Tier-1 drift: %w", err)
		}
		fmt.Fprintf(os.Stderr, "📝 --accept (deprecated): disowned %d Tier-1 file(s) — they are user-owned now:\n", len(disowned))
		for _, p := range disowned {
			fmt.Fprintf(os.Stderr, "   - %s\n", p)
			if err := checksums.CleanSideRenders(ctx.AbsPath, p); err != nil {
				fmt.Fprintf(os.Stderr, "   warning: could not clean side renders for %s: %v\n", p, err)
			}
		}
		fmt.Fprintf(os.Stderr, "   Forge will NEVER update them again. To re-adopt one later: delete the file and run `forge generate`.\n")
		// Disowns are design feedback — record one friction entry per
		// path NOW, while the why (--reason) is still fresh.
		// Best-effort and never interactive; see friction_disown.go.
		recordDisownFriction(ctx.AbsPath, "generate --accept", ctx.AcceptReason, disowned, os.Stderr)
		return nil
	}

	// --explain-drift: don't abort — redirect drifted paths to side
	// renders, let the pipeline produce fresh content to diff against,
	// and fail with the report at the end of the run. The explicit flag
	// outranks the mid-merge heuristic below. Mechanics + rationale in
	// generate_explain_drift.go.
	if ctx.ExplainDrift {
		prepareExplainDrift(ctx, drift)
		return nil
	}

	// Mid-merge detection: when git is in the middle of a merge /
	// cherry-pick / rebase, the Tier-1 drift we're seeing is almost
	// always upstream changes the operation brought in — NOT a real
	// hand-edit. Surface a friendlier message that points the user at
	// the right escape hatches.
	if state := detectGitMergeState(ctx.AbsPath); state != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "Tier-1 stomp guard tripped while git is mid-%s.\n\n", state)
		fmt.Fprintf(&b, "%d Tier-1 file(s) drifted from their recorded checksum:\n", len(drift))
		for _, d := range drift {
			fmt.Fprintf(&b, "  • %s\n", d.Path)
		}
		fmt.Fprintf(&b, "\nDuring a mid-%s state this is almost always upstream changes the merge brought in (not real hand-edits). Two options:\n", state)
		fmt.Fprintf(&b, "  1. Resolve the %s first (`git status`), then re-run `forge generate`.\n", state)
		fmt.Fprintf(&b, "  2. Run `forge generate --force` to regenerate the drifted files from the current templates after the merge content lands.\n")
		return errMidMergeTier1Drift{state: state, msg: b.String()}
	}

	// Default: error with a batched report. The FIRST line must stand
	// alone — file names + remedies — because truncating consumers
	// (agent harnesses, wrap-and-rethrow callers) often surface only
	// the first line of an error (journey fr-a04f8c0609 saw a bare
	// header with no file and no remedy). The body lives in
	// generate_drift_hints.go — it leads with each file's designated
	// extension point and documents `forge disown` as a permanent,
	// one-way ownership transfer, so giving up regeneration never
	// looks like the path of least resistance for agents. The "Tier-1
	// file-stomp guard" phrase reaches the user via the step-name wrap
	// (`step "check Tier-1 file-stomp guard": …`); repeating it here
	// produced the confusing 'guard: guard:' first line.
	return fmt.Errorf("%s\n%s", tier1DriftSummaryLine(drift), formatTier1DriftReport(drift))
}

// errMidMergeTier1Drift is the typed error returned by
// stepCheckTier1Drift when it has detected the host repo is mid-merge /
// mid-cherry-pick / mid-rebase. Callers that want to render a different
// fixup hint can type-assert with errors.As — the default cobra error
// path just prints msg.
type errMidMergeTier1Drift struct {
	state string // "merge" / "cherry-pick" / "rebase"
	msg   string
}

func (e errMidMergeTier1Drift) Error() string { return e.msg }

// GitMergeState reports the in-flight git operation state, if any.
// Returns "" when no merge / cherry-pick / rebase is in progress.
func (e errMidMergeTier1Drift) GitMergeState() string { return e.state }

// detectGitMergeState returns the in-flight git operation if any:
// "merge", "cherry-pick", "rebase", or "" when none. The probe order
// matches `git status`'s — MERGE_HEAD wins over CHERRY_PICK_HEAD which
// wins over the rebase directories. .git is also accepted as a regular
// file (worktrees use a gitfile pointing at the main repo's .git/
// worktrees/<name>/ — the merge-state files still live in that
// per-worktree directory, so stat-ing relative to <projectDir>/.git
// works there too when .git is a directory).
func detectGitMergeState(projectDir string) string {
	gitDir := resolveGitDir(projectDir)
	if gitDir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		return "merge"
	}
	if _, err := os.Stat(filepath.Join(gitDir, "CHERRY_PICK_HEAD")); err == nil {
		return "cherry-pick"
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); err == nil && fi.IsDir() {
		return "rebase"
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "rebase-apply")); err == nil && fi.IsDir() {
		return "rebase"
	}
	return ""
}

// resolveGitDir locates the .git directory for the project. Returns
// .git when it's a regular directory; for git-worktree checkouts (.git
// is a file containing `gitdir: <path>`) it follows the redirect.
// Returns "" when neither shape applies.
func resolveGitDir(projectDir string) string {
	gitPath := filepath.Join(projectDir, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return gitPath
	}
	// .git is a file — typically a worktree pointer.
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	prefix := "gitdir:"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		dir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(projectDir, dir)
		}
		return dir
	}
	return ""
}

// short truncates a hex digest for human-readable error messages. Full
// digests live in the files' own forge:hash markers for the user to grep.
func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// stepSyncDevForgePkg — was Step 0b.1.
// Vendor a sibling forge/pkg checkout into .forge-pkg/ when go.mod has
// a host-absolute replace pointing at it. Lets `docker build` see the
// same source the host's `go build` resolves. See dev_pkg_replace.go
// for the full design. Best-effort: failure here logs a warning; the
// pipeline continues so users can still iterate without docker.
func stepSyncDevForgePkg(ctx *pipelineContext) error {
	vendored, vendorErr := syncDevForgePkgReplace(ctx.ProjectDir)
	_ = vendored
	return ctx.warnOrFail("forge/pkg dev-mode vendor sync", vendorErr)
}

// stepAnnounceProject prints the "📦 Generating code for project: …"
// banner. Was inline at lines 145-161 of the pre-refactor pipeline.
// Also emits the forge_version mismatch warning when applicable.
func stepAnnounceProject(ctx *pipelineContext) error {
	if ctx.Cfg != nil {
		if warning := forgeVersionMismatchWarning(ctx.Cfg.ForgeVersion, buildinfo.Version()); warning != "" {
			// Per-binary-path sentinel keeps the nudge from spamming every
			// `forge generate` invocation — fires once per shell session
			// (approximated via $TMPDIR).
			binPath, _ := os.Executable()
			if shouldEmitVersionWarn(warning, binPath) {
				fmt.Fprintln(os.Stderr, warning)
			}
		}
		fmt.Printf("📦 Generating code for project: %s\n\n", ctx.Cfg.Name)
		return nil
	}
	// Directory-scan fallback: verify we're in a forge project at all.
	if _, err := os.Stat(filepath.Join(ctx.ProjectDir, "proto")); os.IsNotExist(err) {
		return fmt.Errorf("no 'proto' directory found. Are you in a forge project?")
	}
	fmt.Println("📦 Generating code (directory-scan mode)")
	fmt.Println()
	return nil
}

// populateComponentPresence fills the proto-tree presence flags
// (HasServices/HasAPI/HasDB/HasConfig) and the worker/operator presence
// flags on ctx. Returns the RAW operator presence (before the
// experimental-feature suppression) so stepDetectProtoDirs can print
// its one-line skip message exactly once.
//
// Idempotent and cheap (a few ReadDirs) — called from BOTH
// stepDetectProtoDirs (its original home) and stepCheckTier1Drift. The
// stomp guard's scope filter consults gates that branch on these flags
// (gateCodegenHasAnyEntrypoint, gateCodegenHasServices, …), and the
// guard runs BEFORE the detect step. Pre-fix, the flags were all false
// at guard time, so EVERY registered pkg/app/handlers/middleware path
// was misclassified as out-of-scope on a full run — the guard waved the
// drift through and the emitters silently stomped the user's hand
// edits (caught by the fixture-corpus fork round-trip).
//
// discover errors are deferred here: presence only needs the has-any
// flag, and the bootstrap step re-runs discovery and surfaces the
// disk-first resolution error with full context.
func populateComponentPresence(ctx *pipelineContext) (rawHasOperators bool) {
	ctx.HasServices = dirExists(filepath.Join(ctx.ProjectDir, "proto/services"))
	ctx.HasAPI = dirExists(filepath.Join(ctx.ProjectDir, "proto/api"))
	ctx.HasDB = dirExists(filepath.Join(ctx.ProjectDir, "proto/db"))
	ctx.HasConfig = dirExists(filepath.Join(ctx.ProjectDir, "proto/config"))
	workers, _ := discoverWorkers(ctx.ProjectDir)
	ctx.HasWorkers = len(workers) > 0
	// Operators are experimental — when the feature isn't opted in we
	// suppress the codegen path entirely. We still detect on-disk
	// operator dirs so stepDetectProtoDirs can print a one-line skip
	// message; the pipeline gate functions branch on ctx.HasOperators
	// so flipping it to false elides every operator step at the same
	// point.
	operators, _ := discoverOperators(ctx.ProjectDir)
	rawHasOperators = len(operators) > 0
	if rawHasOperators && ctx.Cfg != nil && !ctx.Cfg.Features.OperatorsEnabled() {
		ctx.HasOperators = false
	} else {
		ctx.HasOperators = rawHasOperators
	}
	return rawHasOperators
}

// stepPreCodegenContractCheck — was Step 0c.
// Asserts internal-package contract.go files use the canonical
// Service/Deps/New(Deps) Service shape BEFORE any generators emit
// files. Catches the "errors point at generated code" failure mode at
// validation time, with a clear error pointing at the user's
// contract.go rather than at the bootstrap.go that would otherwise
// fail to compile.
func stepPreCodegenContractCheck(ctx *pipelineContext) error {
	return preCodegenContractCheck(ctx.ProjectDir, ctx.Cfg)
}

// stepDetectProtoDirs populates the proto-tree presence flags
// (HasServices/HasAPI/HasDB/HasConfig) and worker/operator presence.
// Was inline at lines 188-213 of the pre-refactor pipeline. Also
// short-circuits with an error when the directory-scan fallback is
// active and there are no proto files anywhere.
func stepDetectProtoDirs(ctx *pipelineContext) error {
	rawHasOperators := populateComponentPresence(ctx)
	if rawHasOperators && !ctx.HasOperators {
		fmt.Println("[generate] operator scaffolds detected but features.experimental.operators is off — skipping operator codegen")
	}

	if ctx.Cfg == nil && !ctx.HasServices && !ctx.HasAPI && !ctx.HasDB && !ctx.HasConfig {
		return fmt.Errorf("no proto files found in proto/api, proto/services, proto/db, or proto/config")
	}
	if ctx.Cfg == nil {
		fmt.Println("🔍 Detected proto directories:")
		if ctx.HasAPI {
			fmt.Println("  ✓ proto/api/ (API messages)")
		}
		if ctx.HasServices {
			fmt.Println("  ✓ proto/services/ (Service definitions)")
		}
		if ctx.HasDB {
			fmt.Println("  ✓ proto/db/ (Database models)")
		}
		if ctx.HasConfig {
			fmt.Println("  ✓ proto/config/ (Config definitions)")
		}
		fmt.Println()
	}
	return nil
}

// stepEnsureGenModule bootstraps a missing `gen/go.mod` before any step
// that runs `buf generate` / `go list` / `go build` fires. Fresh git
// worktrees can carry a `go.work` that declares `use gen` but lack the
// actual `gen/go.mod` file (it's gitignored in some setups), which makes
// every Go-tooling invocation fail with "cannot load module gen". Doing
// the synthesis here — once, before the pipeline hits the proto/tools
// steps — keeps the rest of the pipeline ignorant of the bootstrap
// concern. Best-effort: see ensureGenGoMod for the no-op fallthrough
// conditions.
func stepEnsureGenModule(ctx *pipelineContext) error {
	return ensureGenGoMod(ctx.ProjectDir)
}

// stepBufGenerateGo — was Step 1.
// Runs `buf generate` to produce Go stubs (protoc-gen-go +
// protoc-gen-connect-go). Failure aborts: every downstream step
// depends on the *.pb.go shapes.
func stepBufGenerateGo(ctx *pipelineContext) error {
	if err := runBufGenerateGo(ctx.ProjectDir); err != nil {
		return fmt.Errorf("buf generate (Go) failed: %w", err)
	}
	return nil
}

// stepDescriptorGenerate — was Step 1b.
// Extracts services/entities/configs into forge_descriptor.json. The
// descriptor is read by lots of downstream consumers (lint, audit,
// migrations preview); a failure here is logged but non-fatal so the
// rest of the pipeline can still emit code.
func stepDescriptorGenerate(ctx *pipelineContext) error {
	return ctx.warnOrFail("descriptor generation", runDescriptorGenerate(ctx.ProjectDir))
}

// stepOpenAPIGenerate is the api.openapi: true projection. Runs
// protoc-gen-connect-openapi over each proto/services/<svc>/ to emit
// `openapi/<service>.yaml`. Best-effort: a missing plugin or a single
// failing service surfaces as a warning so the rest of the pipeline
// still completes. The hard "plugin not on PATH" case bubbles up as an
// error because that's an actionable misconfiguration (the user opted
// in via forge.yaml but didn't install the binary).
func stepOpenAPIGenerate(ctx *pipelineContext) error {
	if err := runOpenAPIGenerate(ctx.ProjectDir, ctx.Cfg); err != nil {
		return fmt.Errorf("openapi generation: %w", err)
	}
	return nil
}

// stepFrontendWorkspaces emits the project-level pnpm-workspace
// scaffolding (pnpm-workspace.yaml + packages/api + packages/hooks)
// when the project opted into `frontend.workspaces: true`. The
// underlying writer is idempotent — re-running `forge generate` after
// the user edits e.g. packages/api/package.json does NOT clobber the
// changes. When workspaces is false the writer no-ops.
//
// Runs before the TypeScript-stubs step so packages/api/ exists for
// buf to emit into when (in a future cycle) the workspace-mode buf
// gen target lands.
func stepFrontendWorkspaces(ctx *pipelineContext) error {
	if !ctx.Cfg.IsFrontendWorkspacesEnabled() {
		return nil
	}
	if err := ctx.warnOrFail("frontend workspace scaffold",
		generator.WriteFrontendWorkspaceFiles(ctx.ProjectDir, ctx.Cfg.Name, true)); err != nil {
		return err
	}
	// The native primitives package only matters when there's an RN
	// frontend to consume it — gating on HasReactNativeFrontend keeps
	// projects with only a Next.js or Vite SPA frontend from getting
	// useless files under packages/ui-native/.
	if ctx.Cfg.HasReactNativeFrontend() {
		layout := generator.NewFrontendWorkspaceLayout(ctx.Cfg.Name)
		if err := ctx.warnOrFail("ui-native package scaffold",
			generator.WriteUINativePackageFiles(ctx.ProjectDir, layout)); err != nil {
			return err
		}
	}
	return nil
}

// stepFrontendBufTS — was Step 3.
// Per-frontend `buf generate` for TypeScript stubs. Only runs for
// nextjs / react-native frontends. Best-effort per-frontend.
//
// In workspaces mode the per-frontend buf step is replaced by a single
// shared invocation that emits into packages/api/src/gen — see
// runBufGenerateTypeScriptWorkspace. Without the early-return below the
// per-frontend loop would call the workspace helper N times, which is
// idempotent but spams the output.
func stepFrontendBufTS(ctx *pipelineContext) error {
	if ctx.Cfg.IsFrontendWorkspacesEnabled() {
		return runBufGenerateTypeScriptWorkspace(ctx.Cfg, ctx.ProjectDir)
	}
	for _, fe := range ctx.Cfg.Frontends {
		if strings.EqualFold(fe.Type, "nextjs") || strings.EqualFold(fe.Type, "react-native") || strings.EqualFold(fe.Type, "vite-spa") {
			if err := ctx.warnOrFail(fmt.Sprintf("TypeScript generation for %s", fe.Name),
				runBufGenerateTypeScript(fe, ctx.Cfg, ctx.ProjectDir)); err != nil {
				return err
			}
		}
	}
	return nil
}

// stepConfigLoader — was Step 3b.
// Generate `pkg/config/config_gen.go` from proto/config annotations.
// Populates ctx.ConfigFields so the bootstrap step (6) can wire env-
// var loading without re-parsing the descriptor.
func stepConfigLoader(ctx *pipelineContext) error {
	var features config.FeaturesConfig
	var authProvider string
	if ctx.Cfg != nil {
		features = ctx.Cfg.Features
		// Threaded into cmd/server.go so the generated runServer calls
		// middleware.InstallGeneratedAuth for forge.yaml-declared
		// providers (the generated-but-unwired GeneratedAuthInterceptor
		// class of dishonesty).
		authProvider = ctx.Cfg.Auth.Provider
	}
	configFields, cfgErr := generateConfigLoader(ctx.ProjectDir, features, authProvider, ctx.Checksums)
	if cfgErr != nil {
		return fmt.Errorf("config loader generation failed: %w", cfgErr)
	}
	ctx.ConfigFields = configFields
	return nil
}

// stepParseServicesAndModule parses proto/services/ once and stashes
// the result on ctx for the half-dozen mid-pipeline steps that
// previously each re-parsed it (or assumed a sibling step had).
//
// Pre-refactor this lived between steps 3b and 3c as a "parse services
// and module path once for steps 4-6" comment — making it a real GenStep
// keeps the contract explicit: all subsequent steps that read
// ctx.Services or ctx.ModulePath assume this step has run, and its gate
// (gateNeedsServices) is the union of those consumers' needs.
//
// Module-path resolution has two paths: ParseServicesFromProtos already
// reads it for each ServiceDef, so we hoist it from the first parsed
// service. When there are no proto services but there ARE workers or
// operators, fall back to a direct GetModulePath() call.
func stepParseServicesAndModule(ctx *pipelineContext) error {
	var services []codegen.ServiceDef
	var modulePath string
	var err error

	if ctx.HasServices {
		services, err = codegen.ParseServicesFromProtos(filepath.Join(ctx.ProjectDir, "proto/services"), ctx.ProjectDir)
		if err != nil {
			return fmt.Errorf("failed to parse service protos: %w", err)
		}
		// ParseServicesFromProtos already reads the module path and sets it on each ServiceDef.
		// Extract it from the first service to avoid a redundant GetModulePath() call.
		if len(services) > 0 {
			modulePath = services[0].ModulePath
		} else {
			modulePath, err = codegen.GetModulePath(ctx.ProjectDir)
			if err != nil {
				return fmt.Errorf("failed to read module path: %w", err)
			}
		}
	}

	// Resolve module path for workers/operators if not already set (no proto services)
	if modulePath == "" && (ctx.HasWorkers || ctx.HasOperators) {
		modulePath, err = codegen.GetModulePath(ctx.ProjectDir)
		if err != nil {
			return fmt.Errorf("failed to read module path: %w", err)
		}
	}

	ctx.Services = services
	ctx.ModulePath = modulePath
	return nil
}

// stepFrontendHooks — was Step 3c.
// Emit React Query hooks for each Connect-driven service. Requires
// services to be parsed (gate ensures HasServices).
func stepFrontendHooks(ctx *pipelineContext) error {
	if len(ctx.Services) == 0 {
		return nil
	}
	return ctx.warnOrFail("frontend hooks generation", generateFrontendHooks(ctx.Cfg, ctx.Services, ctx.ProjectDir))
}

// stepFrontendComponents — was Step 3d.
// Idempotent: copies the shared shadcn/ui component set into each
// frontend if missing. No-op when components are already present.
func stepFrontendComponents(ctx *pipelineContext) error {
	return ctx.warnOrFail("frontend components install", ensureFrontendComponents(ctx.Cfg, ctx.ProjectDir))
}

// stepFrontendPages — was Step 3e.
// Generate CRUD pages per service. Pages require entity descriptors
// matching the service so generated pages reference real fields rather
// than RPC-name-derived guesses; gated on len(entities) > 0 here.
func stepFrontendPages(ctx *pipelineContext) error {
	if len(ctx.Services) == 0 {
		return nil
	}
	pageEntities, _ := codegen.ParseEntityProtos(ctx.ProjectDir)
	if len(pageEntities) == 0 {
		return nil
	}
	return ctx.warnOrFail("frontend page generation",
		generateFrontendPages(ctx.Cfg, ctx.Services, ctx.ProjectDir, pageEntities, ctx.Checksums))
}

// stepFrontendNav — was Step 3f.
// Re-render nav and dashboard with current entity data. Tier-1 files
// (nav_gen.tsx / dashboard_gen.tsx) are owned by forge; sibling Tier-2
// nav.tsx / dashboard.tsx files are user-owned scaffolds.
func stepFrontendNav(ctx *pipelineContext) error {
	// Parse the SAME entity set stepFrontendPages gates page emission on,
	// so nav_gen/dashboard_gen never advertise a route whose page was
	// never written. Parse errors degrade to an empty set — identical to
	// how stepFrontendPages treats them (no pages → no routes).
	navEntities, _ := codegen.ParseEntityProtos(ctx.ProjectDir)
	return ctx.warnOrFail("frontend nav generation",
		generateFrontendNav(ctx.Cfg, ctx.Services, ctx.ProjectDir, navEntities, ctx.Checksums))
}

// stepCleanupStale — was Step 3z.
// Sweep of stale codegen artifacts after a service is removed or
// renamed. Marker-driven: only files carrying a forge:hash
// certification marker (or a scoped .forge/hashes.json record) are
// candidates; unmarked paths are user-owned and inherently safe.
//
// Positioned LATE in the plan (after every emit step) so the
// per-run `WrittenThisRun` set captures every path the current run
// touched. A certified file NOT in that set is what we delete.
// Pre-2026-06-05 the step ran early and re-derived the "expected"
// set from forge.yaml — that re-derivation disagreed with on-disk
// snake_case proto layouts and deleted user code.
//
// Only runs when len(services) > 0 — on a freshly-scaffolded project
// the descriptor may be empty (or buf hasn't run successfully yet);
// the empty-services short-circuit keeps the legacy contract.
func stepCleanupStale(ctx *pipelineContext) error {
	if len(ctx.Services) == 0 {
		return nil
	}
	candidates, handEdited, cerr := cleanupStaleArtifacts(ctx)
	if cerr != nil {
		return ctx.warnOrFail("stale-artifact cleanup", cerr)
	}
	if len(candidates) > 0 {
		if ctx.ForceCleanup {
			fmt.Println("\n🧹 Removed stale codegen artifacts:")
			for _, p := range candidates {
				rel, _ := filepath.Rel(ctx.ProjectDir, p)
				if rel == "" {
					rel = p
				}
				fmt.Printf("  - %s\n", rel)
			}
		} else {
			fmt.Fprintf(os.Stderr, "\n⚠️  forge generate found %d stale generated file(s). Run with --force-cleanup to delete them, or run `%s audit` for details:\n", len(candidates), Name())
			for _, p := range candidates {
				rel, _ := filepath.Rel(ctx.ProjectDir, p)
				if rel == "" {
					rel = p
				}
				fmt.Fprintf(os.Stderr, "  - %s\n", rel)
			}
		}
	}
	if len(handEdited) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Stale generated file(s) with hand-edits — no emitter writes them anymore, but the bytes are yours so forge won't delete them (remove by hand, or `forge disown` to keep them deliberately):\n")
		for _, rel := range handEdited {
			fmt.Fprintf(os.Stderr, "  - %s\n", rel)
		}
	}
	return nil
}

// stepServiceStubs — was Step 4.
// Non-destructive scaffold of service.go / handlers.go / wrapper.go
// for each service. Existing service directories get only missing-RPC
// handler stubs. CRUD method names are precomputed so the stub
// generator doesn't double-up on names the CRUD handler step (4b)
// will emit.
func stepServiceStubs(ctx *pipelineContext) error {
	// Tombstoned services (mentioned only in a pkg/app/services.go
	// comment — types-only) get NO handlers/<svc>/ scaffold — announce
	// each skip so "why is my handlers dir missing?" has a loud answer
	// in the generate output. Registered AND unlisted (newly added)
	// services both scaffold; the unlisted ones additionally get a
	// register-me notice so the user knows the binary won't serve them
	// until they add the row.
	reg, err := ctx.serviceRegistry()
	if err != nil {
		return err
	}
	rows, tombstoned := splitServiceDefs(reg, ctx.Services)
	for _, svc := range tombstoned {
		fmt.Printf("  ⏭️  Skipped handlers/%s/ (types-only — not registered in %s)\n", naming.ServicePackage(svc.Name), serviceRegistryRelPath)
	}
	for _, svc := range rows {
		if reg.state(svc.Name) == registrationUnlisted {
			fmt.Printf("  📝 %s is generated but NOT served: add `%s(app, cfg, logger, opts...),` to RegisteredServices in %s\n",
				svc.Name, codegen.ServiceRowFuncName(svc.Name), serviceRegistryRelPath)
		}
	}
	crudMethodNames := collectCRUDMethodNames(rows, ctx.ProjectDir)
	if err := generateServiceStubs(ctx.Cfg, rows, ctx.ProjectDir, crudMethodNames, ctx.Checksums); err != nil {
		return fmt.Errorf("service stub generation failed: %w", err)
	}
	return nil
}

// stepInternalDBORM — was Step 4a.
// Emit internal/db/<entity>_orm.go — entity struct + CRUD helpers —
// projected from the APPLIED schema (the entity's introspected
// Columns). The entity struct lives in the same file as its ORM; the
// old internal/db/types.go proto-alias file is gone (entities are no
// longer proto messages).
func stepInternalDBORM(ctx *pipelineContext) error {
	entities, entErr := codegen.ParseEntityProtos(ctx.ProjectDir)
	if entErr != nil {
		return ctx.warnOrFail("entity parsing for ORM generation", entErr)
	}
	if len(entities) == 0 {
		return nil
	}
	svcName := ""
	for _, e := range entities {
		if n := codegen.ServiceNameFromProtoFile(e.ProtoFile); n != "" {
			svcName = n
			break
		}
	}
	planEntities := codegen.EntityDefsToPlanEntities(entities)
	if err := ctx.warnOrFail("ORM generation",
		generator.GeneratePlanORM(ctx.ProjectDir, ctx.ModulePath, svcName, planEntities, ctx.Checksums)); err != nil {
		return err
	}
	// The alias file from the proto-entity era; remove so stale aliases
	// to deleted pb types can't shadow the generated structs.
	_ = os.Remove(filepath.Join(ctx.ProjectDir, "internal", "db", "types.go"))
	fmt.Printf("  ✅ Generated internal/db/ (%d entity ORM files)\n", len(entities))
	return nil
}

// stepCRUDHandlers — was Step 4b.
// Emits handler implementations for the CRUD method shapes detected
// against entity descriptors. Best-effort warning so a single
// unsupported entity doesn't brick the rest of generate.
func stepCRUDHandlers(ctx *pipelineContext) error {
	rows, err := ctx.rowServiceDefs()
	if err != nil {
		return err
	}
	return ctx.warnOrFail("CRUD handler generation",
		generateCRUDHandlers(rows, ctx.ModulePath, ctx.ProjectDir, ctx.Checksums))
}

// stepAuthorizer — was Step 4c.
// Emits the role-mapping authorizer_gen.go from forge proto annotations.
// Best-effort warning.
func stepAuthorizer(ctx *pipelineContext) error {
	// The tombstoned-dir skip set keeps the orphan-dir sweep inside
	// GenerateAuthorizer from re-emitting authorizer_gen.go into a
	// retired (types-only) handlers dir — re-emitting would hide the
	// dir's tracked files from the stale-cleanup sweep.
	rows, err := ctx.rowServiceDefs()
	if err != nil {
		return err
	}
	skips, err := ctx.tombstonedHandlerDirSkips()
	if err != nil {
		return err
	}
	return ctx.warnOrFail("authorizer generation",
		codegen.GenerateAuthorizer(rows, ctx.ModulePath, ctx.ProjectDir, skips, ctx.Checksums))
}

// stepServiceMocks — was Step 5.
// Always regenerate. Mocks are Tier-1 files; their content is owned by
// forge and is the test seam frontends import statically.
func stepServiceMocks(ctx *pipelineContext) error {
	rows, err := ctx.rowServiceDefs()
	if err != nil {
		return err
	}
	if err := generateServiceMocks(rows, ctx.ProjectDir); err != nil {
		return fmt.Errorf("mock generation failed: %w", err)
	}
	return nil
}

// stepInternalContracts — was Step 5b.
// Emit `<pkg>/contracts_gen.go` for every internal package. Reads the
// canonical Service / Deps / New(Deps) shape from contract.go and
// surfaces it as the bootstrap step's source of truth.
func stepInternalContracts(ctx *pipelineContext) error {
	if err := generateInternalPackageContracts(ctx.ProjectDir, ctx.Cfg, ctx.Checksums); err != nil {
		return fmt.Errorf("internal package contract generation failed: %w", err)
	}
	return nil
}

// stepAuthMiddleware — was Step 5c.
// Emit `auth_gen.go` shim wiring forge/pkg/auth to the configured
// provider (clerk, jwt, etc.). Gated on cfg.Auth.Provider being set
// to a real provider.
func stepAuthMiddleware(ctx *pipelineContext) error {
	// Registered-only: the auth interceptor's skip-list enumerates
	// procedures this binary mounts; a service without a row in
	// pkg/app/services.go is not mounted here (its RPCs are either
	// served by a sibling binary or awaiting registration) and must not
	// be enumerated.
	registered, err := ctx.registeredServiceDefs()
	if err != nil {
		return err
	}
	if err := generateAuthMiddleware(ctx.Cfg, registered, ctx.ModulePath, ctx.ProjectDir, ctx.Checksums); err != nil {
		return fmt.Errorf("auth middleware generation failed: %w", err)
	}
	return nil
}

// stepTenantMiddleware — was Step 5d.
//
// Two responsibilities glued together:
//
//  1. Auto-enable cfg.Auth.MultiTenant if any entity has tenant_key
//     fields. The rewrite happens both in-memory (mutates ctx.Cfg) and
//     on-disk (forge.yaml). After this step, cfg.Auth.MultiTenant.Enabled
//     reflects the runtime truth.
//  2. Always emit `tenant_gen.go` — even when multi-tenant is disabled
//     it provides the ContextWithTenantID / TenantIDFromContext /
//     RequireTenantID helpers that pkg/app/testing.go references
//     unconditionally. Disabled-mode emits a ~10-line shim.
//
// Forge.yaml-rewrite propagation: ctx.Cfg is a pointer; every later
// step that reads `cfg.Auth.MultiTenant.Enabled` (notably
// stepBootstrapTesting) reads through that same pointer and sees the
// just-set value. The on-disk write is for persistence across runs;
// in-process state propagation is handled by the in-place mutation,
// not by re-reading. We picked this over a "RereadsConfig: true" flag
// because the write is a strict superset of the in-memory mutation —
// nothing else in cfg changes on disk that the runtime cares about.
func stepTenantMiddleware(ctx *pipelineContext) error {
	entities, _ := codegen.ParseEntityProtos(ctx.ProjectDir)
	hasTenantEntities := false
	for _, e := range entities {
		if e.HasTenant {
			hasTenantEntities = true
			break
		}
	}
	if hasTenantEntities {
		if ctx.Cfg.Auth.MultiTenant == nil {
			ctx.Cfg.Auth.MultiTenant = &config.MultiTenantConfig{}
		}
		if !ctx.Cfg.Auth.MultiTenant.Enabled {
			ctx.Cfg.Auth.MultiTenant.Enabled = true
			configPath := filepath.Join(ctx.ProjectDir, defaultProjectConfigFile)
			writeErr := generator.WriteProjectConfigFile(ctx.Cfg, configPath)
			if writeErr == nil {
				fmt.Println("  ✅ Auto-enabled multi-tenant config (entities use tenant_key)")
			} else if err := ctx.warnOrFail("persist multi-tenant config", writeErr); err != nil {
				return err
			}
		}
	}
	if err := generateTenantMiddleware(ctx.Cfg, ctx.ProjectDir, ctx.Checksums); err != nil {
		return fmt.Errorf("tenant middleware generation failed: %w", err)
	}
	return nil
}

// stepWebhookRoutes — was Step 5e.
// Emit webhook router from any webhook annotations in the descriptor.
// No-op when no webhooks are declared.
func stepWebhookRoutes(ctx *pipelineContext) error {
	reg, err := ctx.serviceRegistry()
	if err != nil {
		return err
	}
	if err := generateWebhookRoutes(ctx.Cfg, reg, ctx.ProjectDir, ctx.Checksums); err != nil {
		return fmt.Errorf("webhook route generation failed: %w", err)
	}
	return nil
}

// stepMCPManifest emits gen/mcp/manifest.json: a JSON manifest mapping
// every RPC in the project to an MCP tool schema. Lets MCP-aware agent
// hosts discover the project's Connect RPCs as callable tools without
// per-project wiring. See internal/codegen/mcp_gen.go for the schema.
//
// Loud-by-default: fires unconditionally as part of the standard
// pipeline whenever the project has services. Skips silently when
// there are no services — the inner emitter no-ops on an empty slice.
// A best-effort warning surfaces any rare write failure rather than
// aborting the whole pipeline; the manifest is a static descriptor
// artifact, not a build-blocker.
func stepMCPManifest(ctx *pipelineContext) error {
	projectName := ""
	if ctx.Cfg != nil {
		projectName = ctx.Cfg.Name
	}
	// Registered-only: advertising an unregistered service's RPCs as MCP
	// tools would be false — this binary doesn't serve them. The audit
	// shape keeps the unserved RPCs discoverable (served: false,
	// additive).
	registered, err := ctx.registeredServiceDefs()
	if err != nil {
		return err
	}
	return ctx.warnOrFail("MCP manifest generation",
		codegen.GenerateMCPManifest(codegen.MCPGenInput{
			ProjectDir:  ctx.ProjectDir,
			ProjectName: projectName,
			Services:    registered,
			Checksums:   ctx.Checksums,
		}))
}

// deriveOrmEnabled is the probe for "should bootstrap.go include the
// ORM wire-up?". Extracted so the rule can be golden-tested
// independently of the pipeline.
//
// Entities are projections of the applied schema (db/migrations →
// internal/db/*_orm.go via stepInternalDBORM, which runs upstream), so
// the ORM is enabled exactly when internal/db contains generated ORM
// output: any *_orm.go file, or the legacy internal/db/types.go from
// older projects.
//
// Returns an error only when the internal/db scan itself fails (e.g.
// I/O permission). A missing internal/db directory is simply "off".
func deriveOrmEnabled(projectDir string) (bool, error) {
	dbDir := filepath.Join(projectDir, "internal", "db")
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("scan internal/db for ORM output: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), "_orm.go") || e.Name() == "types.go" {
			return true, nil
		}
	}
	return false, nil
}

// stepBootstrap — was Step 6 (HIGHEST RISK in the polish-phase
// extraction; see TestDeriveOrmEnabledMatrix for the golden test).
//
// Emits pkg/app/bootstrap.go. ormEnabled is the contentious bit; see
// deriveOrmEnabled for the probe rules and rationale.
func stepBootstrap(ctx *pipelineContext) error {
	var dbDriver string
	if ctx.Cfg != nil {
		dbDriver = ctx.Cfg.Database.Driver
	}
	ormEnabled, err := deriveOrmEnabled(ctx.ProjectDir)
	if err != nil {
		return err
	}
	// Diagnostics / strict-wiring feature toggles flow from forge.yaml
	// straight into the bootstrap template so the diagnostics.Default.Boot
	// call (and its StrictEmitter wrap) is only emitted when the project
	// opted in. Default off — existing projects don't suddenly start
	// logging warns on regen.
	var bootstrapFeatures codegen.BootstrapFeatures
	if ctx.Cfg != nil {
		bootstrapFeatures.DiagnosticsEnabled = ctx.Cfg.Features.DiagnosticsEnabled()
		bootstrapFeatures.StrictWiringEnabled = ctx.Cfg.Features.StrictWiringEnabled()
	}
	// The COMPLETE service inventory (including tombstoned types-only
	// services) renders into BootstrapOnly's registration guard, which
	// errors helpfully when an unregistered service name is passed to
	// the `server [services...]` name filter.
	bootstrapFeatures.AllServiceNames = allServiceRuntimeNames(ctx.Services)
	// Row services only (registered + newly-added unlisted): the
	// serviceRow constructors, wire_gen, and diagnostics rows exist per
	// service whose handlers scaffold lives in this repo. Which of those
	// rows the binary SERVES is pkg/app/services.go's call, consumed by
	// the bootstrap template via RegisteredServices.
	rows, err := ctx.rowServiceDefs()
	if err != nil {
		return err
	}
	if err := generateBootstrap(rows, ctx.ModulePath, dbDriver, ormEnabled, ctx.ProjectDir, ctx.ConfigFields, bootstrapFeatures, ctx.Checksums); err != nil {
		return fmt.Errorf("bootstrap generation failed: %w", err)
	}
	return nil
}

// stepCmdSubcommands regenerates cmd/services_gen.go — one cobra
// subcommand per REGISTERED service, the cmd-side projection of the
// same registration table (pkg/app/services.go rows) bootstrap
// consumes — and ensures the user-owned cmd/commands.go extension
// point exists (the generated cmd/main.go calls userCommands(), so a
// missing file would break the build of any pre-M6 project on its
// first regenerate).
//
// Skipped silently when cmd/server.go doesn't exist: CLI/library kinds
// and codegen-less trees have no runServer to delegate to.
func stepCmdSubcommands(ctx *pipelineContext) error {
	if _, err := os.Stat(filepath.Join(ctx.ProjectDir, "cmd", "server.go")); err != nil {
		return nil
	}
	if err := codegen.GenerateCmdCommands(ctx.ProjectDir); err != nil {
		return fmt.Errorf("scaffold cmd/commands.go: %w", err)
	}
	// REGISTERED only — a row constructor without a registration line is
	// not served by this binary and must not get a subcommand (it would
	// boot a server that warns "unknown service" and mounts nothing).
	registered, err := ctx.registeredServiceDefs()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(registered))
	for _, svc := range registered {
		names = append(names, svc.Name)
	}
	if err := codegen.GenerateCmdServices(names, ctx.ProjectDir, ctx.Checksums); err != nil {
		return fmt.Errorf("per-service subcommand generation failed: %w", err)
	}
	fmt.Println("  ✅ Generated cmd/services_gen.go (subcommands projected from RegisteredServices rows)")
	return nil
}

// stepBootstrapTesting — was Step 6b.
// Emits pkg/app/testing.go. Reads cfg.Auth.MultiTenant.Enabled which
// stepTenantMiddleware may have just flipped on (via in-place mutation
// of ctx.Cfg). The order is load-bearing: if 6b ran before 5d, the
// generated testing.go could disagree with the on-disk forge.yaml.
func stepBootstrapTesting(ctx *pipelineContext) error {
	mtEnabled := ctx.Cfg != nil && ctx.Cfg.Auth.MultiTenant != nil && ctx.Cfg.Auth.MultiTenant.Enabled
	rows, err := ctx.rowServiceDefs()
	if err != nil {
		return err
	}
	if err := generateBootstrapTesting(rows, ctx.ModulePath, mtEnabled, ctx.ProjectDir, ctx.Checksums); err != nil {
		return fmt.Errorf("bootstrap testing generation failed: %w", err)
	}
	return nil
}

// stepBootstrapMigrate — was Step 6c.
// Emits pkg/app/migrate.go when a database driver is configured. Uses
// cfg.ModulePath rather than ctx.ModulePath because ctx.ModulePath may
// be empty for projects with no proto/services (CLI / library kinds);
// migrate-gen is fine with cfg.ModulePath alone.
func stepBootstrapMigrate(ctx *pipelineContext) error {
	if err := generateMigrate(ctx.ProjectDir, ctx.Cfg.ModulePath, ctx.Checksums); err != nil {
		return fmt.Errorf("migrate generation failed: %w", err)
	}
	return nil
}

// stepSqlcGenerate — was Step 7.
// Runs `sqlc generate` if sqlc.yaml exists. Best-effort: failure
// surfaces as a warning so projects without sqlc on PATH (or with a
// transient sqlc misconfig) can still complete generate.
func stepSqlcGenerate(ctx *pipelineContext) error {
	return ctx.warnOrFail("sqlc generate", runSqlcGenerate(ctx.ProjectDir))
}

// stepGoModTidyGen — was Step 8.
// `go mod tidy` inside gen/ (the standalone module that holds proto
// stubs). PROMOTED 2026-06-07 from "best-effort warn" to hard error
// (regardless of --strict): a failed tidy in gen/ guarantees the next
// `go build ./...` validate step will fail with confusing missing-import
// errors that point at GENERATED code, not at the missing-go-sum-entry
// that caused them. Failing loudly here puts the actionable signal
// (gen/go.mod or gen/go.sum needs attention) right at the point of
// failure.
//
// If a project legitimately can't tidy gen/ (e.g. mid-migration with an
// unresolvable replace directive) the workaround is to fix go.mod / go.sum
// directly rather than asking forge to ignore the failure — silent skip
// would just push the failure surface to a different generated file later.
func stepGoModTidyGen(ctx *pipelineContext) error {
	if err := runGoModTidyGen(ctx.ProjectDir); err != nil {
		return fmt.Errorf("go mod tidy in gen/ failed: %w (subsequent `go build ./...` will fail with confusing missing-import errors; fix gen/go.mod or gen/go.sum)", err)
	}
	return nil
}

// stepGoModTidyPreWiring runs `go mod tidy` (gen/ first, then root)
// BEFORE the pkg/app wiring emitters. The bootstrap/testing/wire_gen
// generators use go/packages type loads (deps-assignability matcher,
// cross-package auto-stub synthesis); on a cold tree — fresh scaffold,
// no go.sum, replace-only forge/pkg requirement — those loads fail
// until the FIRST tidy runs, which historically happened AFTER the
// wiring emitters. Net effect: generate run 1 rendered the degraded
// shape (TODO stubs, unproven matches) and run 2 rendered the resolved
// shape — `forge generate` output was not a pure function of the
// project's source state (the fixture-corpus idempotency assertion
// caught pkg/app/testing.go flipping between runs).
//
// Best-effort by design: a mid-edit tree whose imports don't resolve
// yet must degrade exactly as before (warn + emit the degraded shape),
// not abort. The authoritative tidy steps later in the pipeline stay
// loud. On a converged tree both tidies are fast no-ops.
func stepGoModTidyPreWiring(ctx *pipelineContext) error {
	if err := runGoModTidyGen(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  pre-wiring go mod tidy (gen/) failed (continuing; wiring codegen may degrade to its unproven/TODO fallbacks): %v\n", err)
	}
	if err := runGoModTidyRoot(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  pre-wiring go mod tidy (root) failed (continuing; wiring codegen may degrade to its unproven/TODO fallbacks): %v\n", err)
	}
	return nil
}

// stepCIWorkflows — was Step 8b.
// Renders .github/workflows/*.yml from forge.yaml. Non-fatal: CI/CD
// is opt-in and most local-dev iterations don't need it regenerated.
func stepCIWorkflows(ctx *pipelineContext) error {
	fmt.Println("\n🔧 Generating CI/CD workflows...")
	if err := generateCIWorkflows(ctx.AbsPath, ctx.Cfg, ctx.Checksums, ctx.Force); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  CI/CD generation warning: %v\n", err)
	}
	return nil
}

// stepPackGenerateHooks — was Step 8c.
// Re-renders any `generate:` hook listed in an installed pack's
// manifest. Pack hooks emit per-pack codegen (e.g. stripe webhook
// handlers, audit-log middleware glue). Non-fatal so a single
// misbehaving pack doesn't brick the whole pipeline.
func stepPackGenerateHooks(ctx *pipelineContext) error {
	if err := runPackGenerateHooks(ctx.ProjectDir, ctx.Cfg); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  Pack generate hooks warning: %v\n", err)
	}
	return nil
}

// stepRegenerateInfra — was Step 8d.
// Regenerates Tier-1 infrastructure files (Dockerfile, Taskfile,
// .dockerignore, .gitignore, deploy/k3d.yaml, etc.) from templates.
// Fatal: the Tier-1 emitter is the source of truth for these files;
// silent failure produces drift that bites at deploy time.
func stepRegenerateInfra(ctx *pipelineContext) error {
	fmt.Println("\n── Regenerating infrastructure files ──")
	// Tracked variant: routes every write through the checksums
	// chokepoint so forked entries are honored (side-rendered, not
	// stomped) and the manifest records what this run emitted.
	if err := generator.RegenerateInfraFilesTracked(ctx.AbsPath, ctx.Cfg, ctx.Checksums); err != nil {
		return fmt.Errorf("regenerate infrastructure files: %w", err)
	}
	return nil
}

// stepPerEnvDeployConfig — was Step 8d-0.
// For each environment in forge.yaml, projects merged per-env config
// onto ConfigFieldOptions annotations and emits
// `deploy/kcl/<env>/config_gen.k`. The hand-edited `main.k` for the
// env imports this module and concatenates into env_vars.
func stepPerEnvDeployConfig(ctx *pipelineContext) error {
	return ctx.warnOrFail("per-env deploy config generation",
		generatePerEnvDeployConfig(ctx.ProjectDir, ctx.Cfg, ctx.Checksums))
}

// stepIngressK3dPorts derives the k3d host→cluster port mappings
// from the dev env's KCL gateway listeners and writes
// `deploy/k3d-ports.yaml`. `forge dev cluster up` merges this
// fragment into the user-owned `deploy/k3d.yaml` at create time —
// see internal/codegen/ingress_k3d_gen.go for the rendered shape
// and the why behind the merge-at-create model.
//
// When the dev env has no gateways (e.g. the project just enabled
// features.ingress but hasn't authored ingress.k yet), the existing
// fragment is removed so a stale port-mapping doesn't linger.
func stepIngressK3dPorts(ctx *pipelineContext) error {
	listeners, err := collectDevGatewayListeners(ctx)
	if err != nil {
		// KCL render failures here are non-fatal by default — the project
		// may not have a dev env yet, or kcl may not be installed locally.
		// --strict promotes the warning to fatal.
		return ctx.warnOrFail("k3d-ports.yaml generation (dev gateway listener collection)", err)
	}
	if len(listeners) == 0 {
		return ctx.warnOrFail("remove stale k3d-ports.yaml", codegen.RemoveK3dPorts(ctx.ProjectDir))
	}
	return ctx.warnOrFail("write deploy/k3d-ports.yaml", codegen.GenerateK3dPorts(codegen.K3dPortsGenInput{
		ProjectDir: ctx.ProjectDir,
		Listeners:  listeners,
		Checksums:  ctx.Checksums,
	}))
}

// collectDevGatewayListeners evaluates the dev env's KCL and projects
// the Gateway listener set into the shape ingress_k3d_gen expects.
// Returns (nil, nil) when the dev env isn't configured yet — that's
// a normal first-scaffold state.
func collectDevGatewayListeners(ctx *pipelineContext) ([]codegen.K3dListener, error) {
	devKCL := filepath.Join(ctx.ProjectDir, "deploy", "kcl", "dev")
	if _, err := os.Stat(devKCL); err != nil {
		return nil, nil
	}
	entities, err := RenderKCL(context.Background(), ctx.ProjectDir, "dev")
	if err != nil {
		return nil, err
	}
	if entities == nil {
		return nil, nil
	}
	var out []codegen.K3dListener
	for _, gw := range entities.Gateways {
		for _, l := range gw.Listeners {
			out = append(out, codegen.K3dListener{
				GatewayName:  gw.Name,
				ListenerName: l.Name,
				Port:         l.Port,
			})
		}
	}
	return out, nil
}

// stepGrafanaDashboards — was Step 8d-i.
// Emits Grafana dashboard JSON. Non-fatal: dashboards are operator
// candy, not a build-blocker.
func stepGrafanaDashboards(ctx *pipelineContext) error {
	return ctx.warnOrFail("Grafana dashboard generation",
		generator.GenerateGrafanaDashboards(ctx.Cfg.Name, ctx.AbsPath))
}

// stepEntitySeeds — was Step 8d-ii.
// Generates entity-aware seed data. Parses entity protos and writes
// SQL/Go fixtures keyed off entity field metadata. The pre-refactor
// version stashed the parsed entityDefs into a local that step 8d-iii
// then reused; we now stash on ctx so the order is explicit.
func stepEntitySeeds(ctx *pipelineContext) error {
	entityDefs, parseErr := codegen.ParseEntityProtos(ctx.ProjectDir)
	if parseErr != nil {
		return ctx.warnOrFail("entity proto parsing for seeds", parseErr)
	}
	ctx.EntityDefs = entityDefs
	if len(entityDefs) == 0 {
		return nil
	}
	seedEntities := generator.EntityDefsToSeedEntities(entityDefs)
	return ctx.warnOrFail("entity seed generation",
		generator.GenerateEntitySeeds(seedEntities, ctx.AbsPath))
}

// stepFrontendMocks — was Step 8d-iii.
// Always runs when frontends are configured even with no entities or
// services: the inner emitter falls back to a no-op `mock-transport.ts`
// stub so connect.ts's static `require('@/lib/mock-transport')` resolves
// at build time. Without that stub, `npm run build` fails during the
// `/_not-found` prerender with a webpack module-resolution error that
// doesn't finger-point at the missing file.
func stepFrontendMocks(ctx *pipelineContext) error {
	return ctx.warnOrFail("frontend mock generation",
		generateFrontendMocks(ctx.Cfg, ctx.Services, ctx.EntityDefs, ctx.ProjectDir))
}

// stepGoModTidyRoot — was Step 8e.
// `go mod tidy` in the project root. PROMOTED 2026-06-07 from "best-effort
// warn" to hard error (regardless of --strict) for the same reason as
// stepGoModTidyGen: a failed tidy guarantees the subsequent `go build
// ./...` validate step fails with missing-import errors that look like
// codegen bugs but are actually go.mod skew.
func stepGoModTidyRoot(ctx *pipelineContext) error {
	if err := runGoModTidyRoot(ctx.ProjectDir); err != nil {
		return fmt.Errorf("go mod tidy in project root failed: %w (subsequent `go build ./...` will fail with confusing missing-import errors; fix go.mod or go.sum)", err)
	}
	return nil
}

// stepGoimports — was Step 8f.
// Runs goimports on every generated Go file to canonicalize import
// groups. Resolves the module path lazily because some projects
// (library/CLI kinds with no proto services) won't have set ctx.ModulePath
// in the mid-pipeline cluster.
func stepGoimports(ctx *pipelineContext) error {
	if ctx.ModulePath == "" {
		ctx.ModulePath, _ = codegen.GetModulePath(ctx.ProjectDir)
	}
	if ctx.ModulePath == "" {
		return nil
	}
	return ctx.warnOrFail("goimports", runGoimportsOnGenerated(ctx.ProjectDir, ctx.ModulePath))
}

// stepRehashTracked — was Step 8f.1.
// Re-stamps every file written this run after goimports. goimports
// rewrites imports in-place after the chokepoint stamped the
// pre-formatted content; without the re-stamp, the embedded hash would
// be stale and the next run's guard would flag every formatted file as
// "hand-edited". Also flushes the deferred heal notices: a "heal" whose
// post-format bytes converged back to the replaced content was
// formatting noise, not a content change, and stays silent.
func stepRehashTracked(ctx *pipelineContext) error {
	checksums.RestampWritten(ctx.AbsPath, ctx.Checksums)
	checksums.FlushHealNotices(ctx.AbsPath)
	return nil
}

// stepPostGenValidate — was Step 8g.
// Runs the heuristic post-gen warnings (orphan handlers, missing
// authorizers, etc.). Always non-fatal: warnings only.
func stepPostGenValidate(ctx *pipelineContext) error {
	if warnings := validateGeneratedProject(ctx.ProjectDir); len(warnings) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Post-generation warnings:\n")
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "  • %s\n", w)
		}
	}
	return nil
}

// stepGoBuildValidate — was Step 9.
// Runs `go build ./...` to catch any codegen that produced
// non-compiling Go. Includes the pre-refactor heuristic hints
// (pkg/config / authorizer_gen) on failure to nudge the user toward
// the most likely fix.
func stepGoBuildValidate(ctx *pipelineContext) error {
	return runGoBuildValidate(ctx.ProjectDir)
}
