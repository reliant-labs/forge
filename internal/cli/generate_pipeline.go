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
	// Accept means: "honor my hand-edits to Tier-1 files; refresh the
	// recorded checksum to match the on-disk content, then proceed with
	// the rest of the pipeline." Used at most once when forking a
	// generated file. See stepCheckTier1Drift for the full guard logic.
	Accept bool

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
}

// newPipelineContext builds the initial context. The caller (the cobra
// RunE) wires projectDir + force + accept; everything else is filled by
// the early steps so a unit test can construct a synthetic context
// without touching disk.
func newPipelineContext(projectDir string, force, accept bool) (*pipelineContext, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project dir: %w", err)
	}
	return &pipelineContext{
		ProjectDir: projectDir,
		AbsPath:    abs,
		Force:      force,
		Accept:     accept,
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
		{Name: "check Tier-1 file-stomp guard", Gate: always, Run: stepCheckTier1Drift, Tag: "validate"},
		{Name: "sync forge/pkg dev replace", Gate: always, Run: stepSyncDevForgePkg, Tag: "config"},
		{Name: "announce project", Gate: always, Run: stepAnnounceProject, Tag: "config"},
		{Name: "pre-codegen contract check", Gate: always, Run: stepPreCodegenContractCheck, Tag: "validate"},
		{Name: "detect proto directories", Gate: always, Run: stepDetectProtoDirs, Tag: "proto"},
		{Name: "buf generate (Go stubs)", Gate: gateCodegenEnabled, Run: stepBufGenerateGo, Tag: "proto"},
		{Name: "descriptor extraction", Gate: gateCodegenEnabled, Run: stepDescriptorGenerate, Tag: "proto"},
		{Name: "ORM generate (proto/db)", Gate: gateORMHasDB, Run: stepOrmGenerate, Tag: "codegen"},
		{Name: "initial migration scaffold", Gate: gateMigrationsHasDB, Run: stepInitialMigration, Tag: "migrations"},
		{Name: "entity-aware migration", Gate: gateMigrationsHasServices, Run: stepEntityMigration, Tag: "migrations"},
		{Name: "TypeScript stubs (frontends)", Gate: gateFrontendEnabled, Run: stepFrontendBufTS, Tag: "frontend"},
		{Name: "config loader (proto/config)", Gate: gateCodegenHasConfig, Run: stepConfigLoader, Tag: "codegen"},
		{Name: "parse services + module path", Gate: gateNeedsServices, Run: stepParseServicesAndModule, Tag: "codegen"},
		{Name: "frontend hooks", Gate: gateFrontendHasServices, Run: stepFrontendHooks, Tag: "frontend"},
		{Name: "ensure frontend components", Gate: gateFrontendHasFrontends, Run: stepFrontendComponents, Tag: "frontend"},
		{Name: "frontend CRUD pages", Gate: gateFrontendHasServices, Run: stepFrontendPages, Tag: "frontend"},
		{Name: "frontend nav + dashboard", Gate: gateFrontendHasFrontends, Run: stepFrontendNav, Tag: "frontend"},
		{Name: "cleanup stale codegen", Gate: gateCodegenHasServices, Run: stepCleanupStale, Tag: "codegen"},
		{Name: "service stubs", Gate: gateCodegenHasServices, Run: stepServiceStubs, Tag: "codegen"},
		{Name: "internal/db/ ORM (entity-driven)", Gate: gateORMHasServices, Run: stepInternalDBORM, Tag: "codegen"},
		{Name: "CRUD handlers", Gate: gateCodegenHasServices, Run: stepCRUDHandlers, Tag: "codegen"},
		{Name: "authorizer", Gate: gateCodegenHasServices, Run: stepAuthorizer, Tag: "codegen"},
		{Name: "service mocks", Gate: gateCodegenHasServices, Run: stepServiceMocks, Tag: "codegen"},
		{Name: "internal package contracts", Gate: gateContractsEnabled, Run: stepInternalContracts, Tag: "codegen"},
		{Name: "auth middleware", Gate: gateAuthProviderConfigured, Run: stepAuthMiddleware, Tag: "codegen"},
		{Name: "tenant middleware (auto-enable + emit)", Gate: gateCodegenHasServicesCfg, Run: stepTenantMiddleware, Tag: "codegen"},
		{Name: "webhook routes", Gate: gateCodegenHasCfg, Run: stepWebhookRoutes, Tag: "codegen"},
		{Name: "pkg/app/bootstrap.go", Gate: gateCodegenHasAnyEntrypoint, Run: stepBootstrap, Tag: "codegen"},
		{Name: "pkg/app/testing.go", Gate: gateCodegenHasAnyEntrypoint, Run: stepBootstrapTesting, Tag: "codegen"},
		{Name: "pkg/app/migrate.go", Gate: gateMigrateHasDriver, Run: stepBootstrapMigrate, Tag: "codegen"},
		{Name: "sqlc generate", Gate: always, Run: stepSqlcGenerate, Tag: "tools"},
		{Name: "go mod tidy (gen/)", Gate: always, Run: stepGoModTidyGen, Tag: "tools"},
		{Name: "CI workflows", Gate: gateCIWorkflows, Run: stepCIWorkflows, Tag: "deploy"},
		{Name: "pack generate hooks", Gate: gateHasPacks, Run: stepPackGenerateHooks, Tag: "codegen"},
		{Name: "regenerate infra files", Gate: gateDeployEnabled, Run: stepRegenerateInfra, Tag: "deploy"},
		{Name: "per-env deploy config", Gate: gateDeployHasConfig, Run: stepPerEnvDeployConfig, Tag: "deploy"},
		{Name: "Grafana dashboards", Gate: gateObservabilityHasCfg, Run: stepGrafanaDashboards, Tag: "deploy"},
		{Name: "entity-aware seed data", Gate: gateMigrationsHasDBOrServices, Run: stepEntitySeeds, Tag: "migrations"},
		{Name: "frontend mocks + transport", Gate: gateFrontendHasFrontends, Run: stepFrontendMocks, Tag: "frontend"},
		{Name: "go mod tidy (root)", Gate: always, Run: stepGoModTidyRoot, Tag: "tools"},
		{Name: "goimports on generated Go", Gate: always, Run: stepGoimports, Tag: "tools"},
		{Name: "rehash tracked files", Gate: always, Run: stepRehashTracked, Tag: "tools"},
		{Name: "refresh ORM output mtimes", Gate: gateORMHasDB, Run: stepTouchORMOutputs, Tag: "tools"},
		{Name: "post-gen validation", Gate: always, Run: stepPostGenValidate, Tag: "validate"},
		{Name: "go build (validate generated code)", Gate: always, Run: stepGoBuildValidate, Tag: "validate"},
	}
}

// always is the trivial gate. Used for steps that are unconditional
// (config loading, checksums, build validation) or whose internal
// no-op-when-not-applicable behavior matches the pre-refactor shape.
func always(_ *pipelineContext) bool { return true }

// Gate helpers. Pure predicates over ctx — no I/O. Tests assert these
// don't mutate ctx by calling them twice and comparing field-wise.

func gateCodegenEnabled(ctx *pipelineContext) bool {
	return ctx.Cfg == nil || ctx.Cfg.Features.CodegenEnabled()
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
	return ctx.Cfg != nil && len(ctx.Cfg.Packs) > 0
}

func gateDeployEnabled(ctx *pipelineContext) bool {
	return ctx.Cfg == nil || ctx.Cfg.Features.DeployEnabled()
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

func gateMigrationsHasDB(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.MigrationsEnabled()) && ctx.HasDB
}

func gateMigrationsHasServices(ctx *pipelineContext) bool {
	return (ctx.Cfg == nil || ctx.Cfg.Features.MigrationsEnabled()) && ctx.HasServices
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
func stepLoadConfig(ctx *pipelineContext) error {
	cfg, err := loadProjectConfigFrom(filepath.Join(ctx.ProjectDir, defaultProjectConfigFile))
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		return fmt.Errorf("failed to load project config: %w", err)
	}
	if errors.Is(err, ErrProjectConfigNotFound) {
		ctx.Cfg = nil
		return nil
	}
	ctx.Cfg = cfg
	return nil
}

// stepLoadChecksums — was Step 0b.
// Loads .forge/checksums.json and stamps the active forge version. The
// caller (runGeneratePipeline) is responsible for calling SaveChecksums
// on exit so partial pipeline runs still persist what they wrote.
//
// Also resets the per-run SkipWrite set in the checksums package — that
// set is populated by `forge generate --accept` so subsequent codegen
// emitters know to leave the user's hand-edited Tier-1 files alone for
// this run. It's pipeline-run state, not persistent state, so it must
// start empty on every invocation.
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
// codegen step runs, walk every Tier-1 entry in `.forge/checksums.json`
// and compare the on-disk content's hash against the recorded hash (and
// every prior render in History). If any Tier-1 file has been hand-
// edited (LLM or human), abort the pipeline with a single batched
// report listing every conflict — the user gets one error to react to,
// not N runs to discover N conflicts.
//
// Three escape hatches:
//
//   - --force: skip the check and let codegen overwrite. Documents the
//     "throw my changes away and regenerate from the templates" intent.
//   - --accept: keep the user's edits, refresh the recorded checksum to
//     the on-disk content, then proceed. Documents the rare "I really
//     did want to fork this Tier-1 file" intent.
//   - move the customization into a Tier-2 (`forge:scaffold one-shot`)
//     file instead.
//
// Tier-2 files are intentionally not checked: Tier-2 means "scaffold
// once, never overwrite", so a user edit IS the steady state.
func stepCheckTier1Drift(ctx *pipelineContext) error {
	if ctx.Checksums == nil {
		return nil
	}
	drift := ctx.Checksums.CheckTier1Drift(ctx.AbsPath)
	if len(drift) == 0 {
		return nil
	}

	if ctx.Force {
		fmt.Fprintf(os.Stderr, "⚠️  --force: overwriting %d hand-edited Tier-1 file(s):\n", len(drift))
		for _, d := range drift {
			fmt.Fprintf(os.Stderr, "   - %s\n", d.Path)
		}
		return nil
	}

	if ctx.Accept {
		if err := ctx.Checksums.AcceptTier1Drift(ctx.AbsPath, drift); err != nil {
			return fmt.Errorf("accept Tier-1 drift: %w", err)
		}
		// Mark each accepted path as opt-out for the rest of this run so
		// the codegen emitters skip the write — otherwise the just-
		// accepted user content would get clobbered by the immediately-
		// following Tier-1 emit pass.
		for _, d := range drift {
			checksums.AddSkipWrite(d.Path)
		}
		fmt.Fprintf(os.Stderr, "📝 --accept: refreshed %d Tier-1 checksum(s) to match on-disk content:\n", len(drift))
		for _, d := range drift {
			fmt.Fprintf(os.Stderr, "   - %s\n", d.Path)
		}
		fmt.Fprintf(os.Stderr, "   These files are now forks — forge will treat them as Tier-1 going forward but won't notice future template changes.\n")
		return nil
	}

	// Default: error with a batched report.
	var b strings.Builder
	fmt.Fprintf(&b, "%d Tier-1 file(s) modified after last `forge generate`:\n\n", len(drift))
	for _, d := range drift {
		fmt.Fprintf(&b, "  • %s\n", d.Path)
		fmt.Fprintf(&b, "      recorded: %s\n", short(d.RecordedHash))
		fmt.Fprintf(&b, "      current:  %s (no match in %d prior render(s))\n", short(d.OnDiskHash), d.HistoryDepth)
	}
	fmt.Fprintf(&b, "\nTier-1 files carry the `// Code generated by forge ... DO NOT EDIT.` banner — forge owns them and regenerates them every run. Three options:\n")
	fmt.Fprintf(&b, "  1. Revert your edits (e.g. `git checkout -- <path>`) so forge can regenerate cleanly.\n")
	fmt.Fprintf(&b, "  2. Re-run with `--force` to discard your edits and overwrite from the current templates.\n")
	fmt.Fprintf(&b, "  3. Re-run with `--accept` to keep your edits and refresh the recorded checksum (rare; documents an intentional fork).\n")
	fmt.Fprintf(&b, "  4. Move your customization into a Tier-2 file (`// forge:scaffold one-shot`) — see the `frontend` skill for the nav.tsx / nav_gen.tsx pattern.\n")
	return fmt.Errorf("Tier-1 file-stomp guard:\n%s", b.String())
}

// short truncates a hex digest for human-readable error messages. Full
// digests live in `.forge/checksums.json` for the user to grep.
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
	if vendored, vendorErr := syncDevForgePkgReplace(ctx.ProjectDir); vendorErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: forge/pkg dev-mode vendor sync failed: %v\n", vendorErr)
	} else {
		_ = vendored
	}
	return nil
}

// stepAnnounceProject prints the "📦 Generating code for project: …"
// banner. Was inline at lines 145-161 of the pre-refactor pipeline.
// Also emits the forge_version mismatch warning when applicable.
func stepAnnounceProject(ctx *pipelineContext) error {
	if ctx.Cfg != nil {
		if warning := forgeVersionMismatchWarning(ctx.Cfg.ForgeVersion, buildinfo.Version()); warning != "" {
			fmt.Fprintln(os.Stderr, warning)
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
	ctx.HasServices = dirExists(filepath.Join(ctx.ProjectDir, "proto/services"))
	ctx.HasAPI = dirExists(filepath.Join(ctx.ProjectDir, "proto/api"))
	ctx.HasDB = dirExists(filepath.Join(ctx.ProjectDir, "proto/db"))
	ctx.HasConfig = dirExists(filepath.Join(ctx.ProjectDir, "proto/config"))
	ctx.HasWorkers = len(discoverWorkers(ctx.ProjectDir)) > 0
	ctx.HasOperators = len(discoverOperators(ctx.ProjectDir)) > 0

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
	if err := runDescriptorGenerate(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: descriptor generation failed: %v\n", err)
	}
	return nil
}

// stepOrmGenerate — was Step 2.
// Runs `protoc-gen-forge` over proto/db/ to emit ORM type aliases. The
// gate already verified ctx.HasDB, so the inner runner only needs to
// pre-flight that proto/db has actual .proto files (some scaffolds
// create the directory with no contents yet).
func stepOrmGenerate(ctx *pipelineContext) error {
	if err := runOrmGenerate(ctx.ProjectDir); err != nil {
		return fmt.Errorf("ORM generation failed: %w", err)
	}
	return nil
}

// stepInitialMigration — was Step 2b.
// Auto-generate the very first migration when proto/db entities exist
// but db/migrations is empty. No-op once any migration file exists.
// Best-effort: failure logs a warning and the pipeline keeps going.
func stepInitialMigration(ctx *pipelineContext) error {
	if hasSQLMigrations(ctx.ProjectDir) {
		return nil
	}
	if err := maybeGenerateInitialMigration(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: initial migration generation failed: %v\n", err)
	}
	return nil
}

// stepEntityMigration — was Step 2c.
// Replace the boilerplate "users + posts" placeholder migration with
// an entity-aware migration generated from proto/services/ entities.
// The replacement only happens when the on-disk migration is the
// untouched scaffold; once a user has hand-edited it, we leave it
// alone.
func stepEntityMigration(ctx *pipelineContext) error {
	entityDefs, parseErr := codegen.ParseEntityProtos(ctx.ProjectDir)
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: entity proto parsing for migrations failed: %v\n", parseErr)
		return nil
	}
	if len(entityDefs) == 0 || !isBoilerplateMigration(ctx.ProjectDir) {
		return nil
	}
	migDir := filepath.Join(ctx.ProjectDir, "db", "migrations")
	removeBoilerplateMigrations(migDir)
	planEntities := codegen.EntityDefsToPlanEntities(entityDefs)
	if err := generator.GeneratePlanMigrations(ctx.ProjectDir, planEntities); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: entity migration generation failed: %v\n", err)
		return nil
	}
	fmt.Printf("  ✅ Generated entity-aware migration (%d tables)\n", len(entityDefs))
	return nil
}

// stepFrontendBufTS — was Step 3.
// Per-frontend `buf generate` for TypeScript stubs. Only runs for
// nextjs / react-native frontends. Best-effort per-frontend.
func stepFrontendBufTS(ctx *pipelineContext) error {
	for _, fe := range ctx.Cfg.Frontends {
		if strings.EqualFold(fe.Type, "nextjs") || strings.EqualFold(fe.Type, "react-native") {
			if err := runBufGenerateTypeScript(fe, ctx.Cfg, ctx.ProjectDir); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: TypeScript generation for %s failed: %v\n", fe.Name, err)
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
	if ctx.Cfg != nil {
		features = ctx.Cfg.Features
	}
	configFields, cfgErr := generateConfigLoader(ctx.ProjectDir, features, ctx.Checksums)
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
	if err := generateFrontendHooks(ctx.Cfg, ctx.Services, ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: frontend hooks generation failed: %v\n", err)
	}
	return nil
}

// stepFrontendComponents — was Step 3d.
// Idempotent: copies the shared shadcn/ui component set into each
// frontend if missing. No-op when components are already present.
func stepFrontendComponents(ctx *pipelineContext) error {
	ensureFrontendComponents(ctx.Cfg, ctx.ProjectDir)
	return nil
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
	if err := generateFrontendPages(ctx.Cfg, ctx.Services, ctx.ProjectDir, pageEntities); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: frontend page generation failed: %v\n", err)
	}
	return nil
}

// stepFrontendNav — was Step 3f.
// Re-render nav and dashboard with current entity data. Tier-1 files
// (nav_gen.tsx / dashboard_gen.tsx) are owned by forge; sibling Tier-2
// nav.tsx / dashboard.tsx files are user-owned scaffolds.
func stepFrontendNav(ctx *pipelineContext) error {
	if err := generateFrontendNav(ctx.Cfg, ctx.Services, ctx.ProjectDir, ctx.Checksums, ctx.Force); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: frontend nav generation failed: %v\n", err)
	}
	return nil
}

// stepCleanupStale — was Step 3z.
// Conservative sweep of orphaned codegen artifacts after a service is
// removed or renamed. Only runs when len(services) > 0 — on a freshly-
// scaffolded project the descriptor may be empty (or buf hasn't run
// successfully yet); an empty services slice would cause the sweep to
// delete the just-scaffolded gen/handlers/mocks directories.
func stepCleanupStale(ctx *pipelineContext) error {
	if len(ctx.Services) == 0 {
		return nil
	}
	removed, cerr := cleanupStaleArtifacts(ctx.Cfg, ctx.Services, ctx.ProjectDir)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "Warning: stale-artifact cleanup failed: %v\n", cerr)
		return nil
	}
	if len(removed) > 0 {
		fmt.Println("\n🧹 Removed stale codegen artifacts:")
		for _, p := range removed {
			rel, _ := filepath.Rel(ctx.ProjectDir, p)
			if rel == "" {
				rel = p
			}
			fmt.Printf("  - %s\n", rel)
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
	crudMethodNames := collectCRUDMethodNames(ctx.Services, ctx.ProjectDir)
	if err := generateServiceStubs(ctx.Cfg, ctx.Services, ctx.ProjectDir, crudMethodNames, ctx.Checksums); err != nil {
		return fmt.Errorf("service stub generation failed: %w", err)
	}
	return nil
}

// stepInternalDBORM — was Step 4a.
// Emit internal/db/types.go + per-entity ORM helpers from entity
// descriptors. The "service-name fallback" picks any one service to
// derive the import-path hint; pack-only entities (e.g. stripe in
// proto/db/v1) resolve via gen/db/v1 and don't need a service hint.
func stepInternalDBORM(ctx *pipelineContext) error {
	entities, entErr := codegen.ParseEntityProtos(ctx.ProjectDir)
	if entErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: entity parsing for ORM generation failed: %v\n", entErr)
		return nil
	}
	if len(entities) == 0 {
		return nil
	}
	// Pick a service-name fallback for entities whose ProtoFile path
	// doesn't carry one (e.g. legacy callers). Modern descriptors
	// always populate ProtoFile, so this is a hint-only value.
	svcName := ""
	for _, e := range entities {
		if n := codegen.ServiceNameFromProtoFile(e.ProtoFile); n != "" {
			svcName = n
			break
		}
	}
	planEntities := codegen.EntityDefsToPlanEntities(entities)
	entries := make([]generator.EntityImport, len(entities))
	for i, e := range entities {
		entries[i] = generator.EntityImport{
			Name:      e.Name,
			ProtoFile: e.ProtoFile,
		}
	}
	if err := generator.GeneratePlanDBTypesFromEntities(ctx.ProjectDir, ctx.ModulePath, svcName, entries); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: db types generation failed: %v\n", err)
	}
	if err := generator.GeneratePlanORM(ctx.ProjectDir, ctx.ModulePath, svcName, planEntities); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: ORM generation failed: %v\n", err)
		return nil
	}
	fmt.Printf("  ✅ Generated internal/db/ (%d entity ORM files)\n", len(entities))
	return nil
}

// stepCRUDHandlers — was Step 4b.
// Emits handler implementations for the CRUD method shapes detected
// against entity descriptors. Best-effort warning so a single
// unsupported entity doesn't brick the rest of generate.
func stepCRUDHandlers(ctx *pipelineContext) error {
	if err := generateCRUDHandlers(ctx.Services, ctx.ModulePath, ctx.ProjectDir, ctx.Checksums); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: CRUD handler generation failed: %v\n", err)
	}
	return nil
}

// stepAuthorizer — was Step 4c.
// Emits the role-mapping authorizer_gen.go from forge proto annotations.
// Best-effort warning.
func stepAuthorizer(ctx *pipelineContext) error {
	if err := codegen.GenerateAuthorizer(ctx.Services, ctx.ModulePath, ctx.ProjectDir, ctx.Checksums); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: authorizer generation failed: %v\n", err)
	}
	return nil
}

// stepServiceMocks — was Step 5.
// Always regenerate. Mocks are Tier-1 files; their content is owned by
// forge and is the test seam frontends import statically.
func stepServiceMocks(ctx *pipelineContext) error {
	if err := generateServiceMocks(ctx.Services, ctx.ProjectDir); err != nil {
		return fmt.Errorf("mock generation failed: %w", err)
	}
	return nil
}

// stepInternalContracts — was Step 5b.
// Emit `<pkg>/contracts_gen.go` for every internal package. Reads the
// canonical Service / Deps / New(Deps) shape from contract.go and
// surfaces it as the bootstrap step's source of truth.
func stepInternalContracts(ctx *pipelineContext) error {
	if err := generateInternalPackageContracts(ctx.ProjectDir, ctx.Cfg); err != nil {
		return fmt.Errorf("internal package contract generation failed: %w", err)
	}
	return nil
}

// stepAuthMiddleware — was Step 5c.
// Emit `auth_gen.go` shim wiring forge/pkg/auth to the configured
// provider (clerk, jwt, etc.). Gated on cfg.Auth.Provider being set
// to a real provider.
func stepAuthMiddleware(ctx *pipelineContext) error {
	if err := generateAuthMiddleware(ctx.Cfg, ctx.Services, ctx.ModulePath, ctx.ProjectDir, ctx.Checksums); err != nil {
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
			if writeErr := generator.WriteProjectConfigFile(ctx.Cfg, configPath); writeErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to persist multi-tenant config: %v\n", writeErr)
			} else {
				fmt.Println("  ✅ Auto-enabled multi-tenant config (entities use tenant_key)")
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
	if err := generateWebhookRoutes(ctx.Cfg, ctx.ProjectDir, ctx.Checksums); err != nil {
		return fmt.Errorf("webhook route generation failed: %w", err)
	}
	return nil
}

// deriveOrmEnabled is the probe order for "should bootstrap.go include
// the ORM wire-up?". Extracted so the order can be golden-tested
// independently of the pipeline.
//
// Rules (preserved verbatim from the pre-refactor inline block):
//
//  1. If proto/db/ exists AND contains at least one .proto file with
//     entity messages, ORM is on.
//  2. Otherwise, if internal/db/types.go exists on disk, ORM is on
//     (new architecture: entities live inline in service protos and
//     the types file is generated by stepInternalDBORM upstream).
//  3. Otherwise, ORM is off.
//
// The order matters: rule 1 is authoritative and short-circuits; rule
// 2 is the fallback for projects without proto/db. Reversing them
// would let a stale internal/db/types.go from a prior run mask a
// proto/db that's currently empty (regression surface).
//
// Returns an error only when the proto/db scan itself fails (e.g. I/O
// permission). Callers should treat that as a hard fault.
func deriveOrmEnabled(projectDir string, hasDB bool) (bool, error) {
	if hasDB {
		ok, perr := hasProtoFilesInDir(filepath.Join(projectDir, "proto", "db"))
		if perr != nil {
			return false, fmt.Errorf("scan proto/db for ORM protos: %w", perr)
		}
		if ok {
			return true, nil
		}
	}
	if _, err := os.Stat(filepath.Join(projectDir, "internal", "db", "types.go")); err == nil {
		return true, nil
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
	ormEnabled, err := deriveOrmEnabled(ctx.ProjectDir, ctx.HasDB)
	if err != nil {
		return err
	}
	if err := generateBootstrap(ctx.Services, ctx.ModulePath, dbDriver, ormEnabled, ctx.ProjectDir, ctx.ConfigFields, ctx.Checksums); err != nil {
		return fmt.Errorf("bootstrap generation failed: %w", err)
	}
	return nil
}

// stepBootstrapTesting — was Step 6b.
// Emits pkg/app/testing.go. Reads cfg.Auth.MultiTenant.Enabled which
// stepTenantMiddleware may have just flipped on (via in-place mutation
// of ctx.Cfg). The order is load-bearing: if 6b ran before 5d, the
// generated testing.go could disagree with the on-disk forge.yaml.
func stepBootstrapTesting(ctx *pipelineContext) error {
	mtEnabled := ctx.Cfg != nil && ctx.Cfg.Auth.MultiTenant != nil && ctx.Cfg.Auth.MultiTenant.Enabled
	if err := generateBootstrapTesting(ctx.Services, ctx.ModulePath, mtEnabled, ctx.ProjectDir, ctx.Checksums); err != nil {
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
	if err := runSqlcGenerate(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: sqlc generate failed: %v\n", err)
	}
	return nil
}

// stepGoModTidyGen — was Step 8.
// `go mod tidy` inside gen/ (the standalone module that holds proto
// stubs). Best-effort: failure here usually means a generated import
// references a fork we haven't sync'd yet — surface as warning so the
// rest of the pipeline can finish, then user fixes go.sum and re-runs.
func stepGoModTidyGen(ctx *pipelineContext) error {
	if err := runGoModTidyGen(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: go mod tidy in gen/ failed: %v\n", err)
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
	if err := generator.RegenerateInfraFiles(ctx.AbsPath, ctx.Cfg); err != nil {
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
	if err := generatePerEnvDeployConfig(ctx.ProjectDir, ctx.Cfg, ctx.Checksums); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: per-env deploy config generation failed: %v\n", err)
	}
	return nil
}

// stepGrafanaDashboards — was Step 8d-i.
// Emits Grafana dashboard JSON. Non-fatal: dashboards are operator
// candy, not a build-blocker.
func stepGrafanaDashboards(ctx *pipelineContext) error {
	if err := generator.GenerateGrafanaDashboards(ctx.Cfg.Name, ctx.AbsPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Grafana dashboard generation failed: %v\n", err)
	}
	return nil
}

// stepEntitySeeds — was Step 8d-ii.
// Generates entity-aware seed data. Parses entity protos and writes
// SQL/Go fixtures keyed off entity field metadata. The pre-refactor
// version stashed the parsed entityDefs into a local that step 8d-iii
// then reused; we now stash on ctx so the order is explicit.
func stepEntitySeeds(ctx *pipelineContext) error {
	entityDefs, parseErr := codegen.ParseEntityProtos(ctx.ProjectDir)
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: entity proto parsing for seeds failed: %v\n", parseErr)
		return nil
	}
	ctx.EntityDefs = entityDefs
	if len(entityDefs) == 0 {
		return nil
	}
	seedEntities := generator.EntityDefsToSeedEntities(entityDefs)
	if err := generator.GenerateEntitySeeds(seedEntities, ctx.AbsPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: entity seed generation failed: %v\n", err)
	}
	return nil
}

// stepFrontendMocks — was Step 8d-iii.
// Always runs when frontends are configured even with no entities or
// services: the inner emitter falls back to a no-op `mock-transport.ts`
// stub so connect.ts's static `require('@/lib/mock-transport')` resolves
// at build time. Without that stub, `npm run build` fails during the
// `/_not-found` prerender with a webpack module-resolution error that
// doesn't finger-point at the missing file.
func stepFrontendMocks(ctx *pipelineContext) error {
	if err := generateFrontendMocks(ctx.Cfg, ctx.Services, ctx.EntityDefs, ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: frontend mock generation failed: %v\n", err)
	}
	return nil
}

// stepGoModTidyRoot — was Step 8e.
// `go mod tidy` in the project root. Best-effort.
func stepGoModTidyRoot(ctx *pipelineContext) error {
	if err := runGoModTidyRoot(ctx.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: go mod tidy in project root failed: %v\n", err)
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
	if err := runGoimportsOnGenerated(ctx.ProjectDir, ctx.ModulePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: goimports failed: %v\n", err)
	}
	return nil
}

// stepTouchORMOutputs runs after goimports/rehash to bump the *.pb.orm.go
// mtimes so they sit at-or-after their *.pb.go siblings. See touchORMOutputs
// for the rationale (protogen skips no-op writes; protoc-gen-go does not, so
// stale mtimes spuriously trigger proto-orm-out-of-sync lint).
func stepTouchORMOutputs(ctx *pipelineContext) error {
	if err := touchORMOutputs(filepath.Join(ctx.ProjectDir, "gen", "db")); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: refresh ORM mtimes: %v\n", err)
	}
	return nil
}

// stepRehashTracked — was Step 8f.1.
// Re-hashes every tracked file after goimports. goimports rewrites
// imports in-place after WriteGeneratedFile recorded the
// pre-formatted content; without this re-hash, audit would flag every
// .go file we just emitted as "user-edited (drift detected)". We only
// refresh the current Hash; History already contains the pre-goimports
// rendering so a future template update can still distinguish stale
// codegen from a real user edit.
func stepRehashTracked(ctx *pipelineContext) error {
	rehashTrackedFiles(ctx.AbsPath, ctx.Checksums)
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
