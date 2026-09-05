package lint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/contractcheck"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
	"github.com/reliant-labs/forge/internal/linter/scaffolds"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// lintFlags holds the flag values for the lint command.
type lintFlags struct {
	contract          bool
	migrationSafety   bool
	fix               bool
	noFix             bool
	exportedVars      bool
	conventions       bool
	frontendStores    bool
	scaffolds         bool
	tests             bool
	banners           bool
	suggestExcludes   bool
	suggestBufExcepts bool
	checkWorkarounds  bool
	optionalDepsGuard bool
	configDeps        bool
	columnMarkers     bool
	crudFixtures      bool
	protoMarkers      bool
	protoOptions      bool
	createNullability bool
	computedFields    bool
	vendoredProtos    bool
	configReach       bool
	generatedDrift    bool
	strict            bool
	skipFrontends     bool
	jsonOut           bool
}

func newCmd(_ *factory.Factory) *cobra.Command {
	var flags lintFlags

	cmd := &cobra.Command{
		Use:   "lint [paths...]",
		Short: "Run linters on the project",
		Long: `Run various linters on the Forge project.

This command will:
- Run standard Go linters (golangci-lint)
- Run proto linters (buf lint)
- Lint and TYPECHECK every frontend the project declares (forge resolves the
  set from forge.yaml — no shell loop over frontends/*/ required). The
  typecheck runs the project's own "typecheck" npm script when it has one,
  and otherwise runs the frontend's TypeScript compiler with --noEmit.
  Severity is lint.frontend.typecheck (error | warn | off) in forge.yaml.
- Optionally run targeted rule sets (--contract, --db, --migration-safety,
  --conventions, --tests)

By default 'forge lint' AUTO-FIXES deterministic-safe issues first, then
gates on the residue: it canonicalizes Go formatting (goimports/gofmt import
grouping + whitespace) across generated AND owned/hand-edited files, applies
golangci-lint's safe autofixes, and runs eslint --fix on frontends — so
mechanical formatting never surfaces as a gating error. Pass --no-fix to gate
only and mutate nothing (CI / read-only checks); --json is always detect-only.

Examples:
  forge lint                     # Auto-fix deterministic-safe issues, then gate
  forge lint --no-fix            # Gate only; mutate nothing (CI / read-only)
  forge lint --skip-frontends    # Backend-only gate; no Node toolchain needed
  forge lint --contract          # Run contract interface enforcement linter
  forge lint --db                # Run DB entity lint rules
  forge lint --migration-safety  # Run SQL migration safety checks
  forge lint --conventions       # Run forge convention rules on proto files
  forge lint --generated-drift   # Fail if a forge-generated ("DO NOT EDIT")
                                 # file was hand-edited
  forge lint --tests             # Run test-convention rules across backend
                                 # handlers and frontend hooks (warnings only)
  forge lint --config-deps       # Flag scalar Deps fields — a scalar is
                                 # configuration, not a collaborator; prints the
                                 # exact proto block + AppConfig line to write
  forge lint --column-markers    # Flag COMMENT ON COLUMN/CONSTRAINT text
                                 # carrying an unrecognized forge:* marker
  forge lint --crud-fixtures     # Flag a seeded foreign-key value in a
                                 # scaffolded lifecycle test that names no
                                 # seeded parent row (a FK added after the
                                 # test was scaffolded)
  forge lint --proto-markers     # Flag a .proto comment carrying an
                                 # unrecognized forge:* marker (a misspelled
                                 # one does nothing and warns nowhere)
  forge lint --create-nullability # Fail when a field's optional label
                                 # disagrees between an entity message and
                                 # its Create<Entity>Request (the flattened
                                 # request silently drops write presence)
  forge lint --computed-fields   # Flag a forge:computed field that no
                                 # non-generated Go file assigns — nothing
                                 # populates it, so the column default ships
  forge lint --proto-options     # Flag a (forge.v1.*) annotation naming an
                                 # option field forge's descriptors do not
                                 # define — it compiles, and forge reads it
                                 # nowhere
  forge lint --vendored-protos   # Fail when proto/forge/v1/forge.proto has
                                 # drifted from this forge binary's copy
                                 # (forge's upgrade path does not track it)
  forge lint --config-reach      # Flag config fields no binary or frontend
                                 # loads (per-binary configs can strand a
                                 # whole message)
  forge lint --optional-deps-guard  # Flag unguarded derefs of a
                                 # // forge:optional-dep field (nil at runtime)
  forge lint --frontend-stores   # Flag Zustand stores holding server data that
                                 # belongs in a generated React Query hook
  forge lint --json              # Machine-readable findings for sub-agents / CI
                                 # (schema in lint_json.go; exit code matches
                                 # text mode; combines with the targeted flags
                                 # above, but not with --fix / --suggest-*)

The three advisory rules above are warnings only — they never fail the build.

Additional maintainer/debug flags exist (forge-repo internals, wiring
audits, suggest-* helpers); run 'forge lint --help-dev' to list them.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var paths []string
			if len(args) > 0 {
				paths = args
			} else {
				paths = []string{"./..."}
			}

			if flags.jsonOut {
				return runLintJSON(cmd.Context(), flags, paths)
			}
			return runLint(cmd.Context(), flags, paths)
		},
	}

	cmd.Flags().BoolVar(&flags.contract, "contract", false, "Run contract interface enforcement linter")
	cmd.Flags().BoolVar(&flags.exportedVars, "exported-vars", false, "Run exported vars linter")
	cmd.Flags().BoolVar(&flags.migrationSafety, "migration-safety", false, "Run SQL migration safety checks")
	cmd.Flags().BoolVar(&flags.conventions, "conventions", false, "Run forge convention rules on proto files")
	// See the note on --optional-deps-guard: no backticks in a usage string,
	// or cobra renders the quoted span as the flag's argument placeholder.
	cmd.Flags().BoolVar(&flags.generatedDrift, "generated-drift", false, "Fail when a forge-generated (\"Code generated by forge. DO NOT EDIT.\") file was edited after forge wrote it")
	cmd.Flags().BoolVar(&flags.frontendStores, "frontend-stores", false, "Flag Zustand stores that import generated Connect clients (warnings only)")
	cmd.Flags().BoolVar(&flags.scaffolds, "scaffolds", false, "Enforce forge ownership boundary (FORGE_SCAFFOLD markers + _gen file headers)")
	cmd.Flags().BoolVar(&flags.tests, "tests", false, "Run test-convention rules (handler-tests-use-tdd + frontend-hook-tests; warnings only)")
	cmd.Flags().BoolVar(&flags.banners, "banners", false, "Verify forge templates carry the right Tier-1 / Tier-2 lifecycle banner (warnings only; no-op outside the forge repo)")
	cmd.Flags().BoolVar(&flags.suggestExcludes, "suggest-excludes", false, "Print a YAML snippet of contracts.exclude candidates (heuristic-based; nothing is mutated)")
	cmd.Flags().BoolVar(&flags.suggestBufExcepts, "suggest-buf-excepts", false, "Run buf lint and print a buf.yaml lint.except snippet for STANDARD rules that fired on more than 3 .proto files (nothing is mutated)")
	cmd.Flags().BoolVar(&flags.checkWorkarounds, "check-workarounds", false, "Flag cross-lane workarounds (cast<X>Repo helpers, testing_extras.go, undeclared cmd/<name>.go) — warnings only")
	// No backticks in this usage string: cobra reads the first backticked
	// span as the flag's argument-name placeholder, so `// forge:optional-dep`
	// rendered as `--optional-deps-guard // forge:optional-dep` in --help.
	// Harmless while the flag was hidden; visible now that it is not.
	cmd.Flags().BoolVar(&flags.optionalDepsGuard, "optional-deps-guard", false, "Flag unguarded derefs of // forge:optional-dep Deps fields (warnings only; suppress with // forge:optional-checked on the deref line)")
	cmd.Flags().BoolVar(&flags.configDeps, "config-deps", false, "Flag scalar Deps fields — scalars are configuration; declare a <Component>Config block in proto/config and take it as a typed field (warnings only)")
	cmd.Flags().BoolVar(&flags.columnMarkers, "column-markers", false, "Flag COMMENT ON COLUMN/CONSTRAINT text containing forge: that matches no known column marker (warnings only)")
	cmd.Flags().BoolVar(&flags.crudFixtures, "crud-fixtures", false, "Flag seeded foreign-key values in handlers_crud_test.go that name no seeded parent row — a foreign key added after the test was scaffolded (warnings only)")
	cmd.Flags().BoolVar(&flags.protoMarkers, "proto-markers", false, "Flag .proto comments containing forge: that match no known proto marker — a misspelled marker is inert and warns nowhere (warnings only)")
	cmd.Flags().BoolVar(&flags.createNullability, "create-nullability", false, "Fail when a field's optional label disagrees between an entity message and its Create<Entity>Request — the flattened request drops write presence silently")
	cmd.Flags().BoolVar(&flags.computedFields, "computed-fields", false, "Flag a forge:computed field that no non-generated Go file assigns — nothing populates it, so the insert takes the column default (warnings only)")
	cmd.Flags().BoolVar(&flags.protoOptions, "proto-options", false, "Flag (forge.v1.*) annotation fields this forge binary's descriptors do not define — a retired or misspelled option field compiles under buf and is read by nothing (warnings only)")
	cmd.Flags().BoolVar(&flags.vendoredProtos, "vendored-protos", false, "Fail when a vendored proto (proto/forge/v1/forge.proto) differs from the copy embedded in this forge binary — forge's upgrade path does not track these copies, so drift is otherwise invisible")
	cmd.Flags().BoolVar(&flags.configReach, "config-reach", false, "Flag config fields that no binary and no frontend loads — with per-binary configs, an unbound config message generates but is never loaded (warnings only)")
	cmd.Flags().BoolVar(&flags.strict, "strict", false, "Escalate advisory findings to errors so they fail the build / CI: RPCs missing a (forge.v1.method) auth-posture annotation, and any lane that could NOT run (frontend typecheck or eslint with deps not installed; typed-config guardrail when golangci-lint never reported)")
	cmd.Flags().BoolVar(&flags.skipFrontends, "skip-frontends", false, "Skip the whole frontend lane (eslint/stylelint + TypeScript typecheck) for a backend-only gate that needs no Node toolchain")
	cmd.Flags().BoolVar(&flags.fix, "fix", false, "Deprecated: auto-fix of deterministic-safe issues is now the default; this flag is a no-op kept for back-compat (use --no-fix to opt out)")
	cmd.Flags().BoolVar(&flags.noFix, "no-fix", false, "Skip the deterministic-safe auto-fix pre-pass (Go formatting, golangci autofixes, eslint --fix); gate only and mutate nothing (CI / read-only)")
	cmd.Flags().BoolVar(&flags.jsonOut, "json", false, "Output findings as JSON (see lint_json.go header for the schema; exit code matches text mode)")

	// User-vs-maintainer surface split: the flags below are fully
	// functional but hidden from --help (visible via --help-dev). The
	// visible set is pinned by TestLintHelpSurface — a new flag must
	// consciously pick a side. See cmdutil.HideDevFlags for the mechanism.
	//
	// The rule of thumb: hide a flag when it is meaningless outside the
	// forge repo, or when it is a one-shot you run during setup/migration
	// and never again. A continuous rule about the USER's own code stays
	// visible however advisory it is — config-deps and optional-deps-guard
	// were both hidden as "audits" and were consequently undiscoverable,
	// despite config-deps finding 15 real issues on the flagship app with
	// paste-ready remediation and no false positives on a clean scaffold.
	hideDevFlags(cmd,
		"exported-vars",       // bundled into --contract; separate flag is for --json explicitness
		"scaffolds",           // forge ownership-boundary enforcement
		"banners",             // no-op outside the forge repo
		"suggest-excludes",    // one-shot migration/setup helper
		"suggest-buf-excepts", // one-shot migration/setup helper
		"check-workarounds",   // parallel-lane agent-workflow audit
	)

	return cmd
}

func runLint(ctx context.Context, flags lintFlags, paths []string) error {
	// When a specific flag is set, run only that linter (preserving current behavior).
	if flags.suggestExcludes {
		_, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		return runSuggestExcludes(cfg)
	}
	if flags.suggestBufExcepts {
		return runSuggestBufExcepts(ctx)
	}
	if flags.contract {
		store, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		if store != nil && !store.Features().ContractsEnabled() {
			fmt.Println("contracts feature is disabled in forge.yaml")
			return nil
		}
		return runContractLinter(ctx, paths, contractExcludesFromConfig(cfg))
	}
	if flags.exportedVars {
		_, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		return runContractLinter(ctx, paths, contractExcludesFromConfig(cfg))
	}
	if flags.migrationSafety {
		store, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		if store != nil && !store.Features().MigrationsEnabled() {
			fmt.Println("migrations feature is disabled in forge.yaml")
			return nil
		}
		return runMigrationSafetyLint(cfg)
	}
	if flags.conventions {
		return runConventionLint(forgeconv.LintOptions{Strict: flags.strict})
	}
	if flags.generatedDrift {
		return runWithCwd(runGeneratedDriftLint)
	}
	if flags.frontendStores {
		return runFrontendStoresLint()
	}
	if flags.scaffolds {
		return runScaffoldsLint()
	}
	if flags.tests {
		return runTestsLint()
	}
	if flags.banners {
		return runBannersLint()
	}
	if flags.checkWorkarounds {
		return runWithCwd(runCheckWorkaroundsLint)
	}
	if flags.optionalDepsGuard {
		return runWithCwd(runOptionalDepsGuardLint)
	}
	if flags.configDeps {
		return runWithCwd(runConfigDepsLint)
	}
	if flags.columnMarkers {
		_, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		return runColumnMarkersLint(cfg)
	}
	if flags.crudFixtures {
		_, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		return runWithCwd(func(cwd string) error { return runCrudFixturesLint(cwd, cfg) })
	}
	if flags.protoMarkers {
		return runProtoMarkersLint(protoDirDefault)
	}
	if flags.createNullability {
		return runCreateNullabilityLint(protoDirDefault)
	}
	if flags.computedFields {
		return runWithCwd(runComputedFieldsLint)
	}
	if flags.protoOptions {
		return runProtoOptionsLint(protoDirDefault)
	}
	if flags.vendoredProtos {
		return runWithCwd(runVendoredProtosLint)
	}
	if flags.configReach {
		_, cfg, err := loadLintConfig()
		if err != nil {
			return err
		}
		return runWithCwd(func(cwd string) error { return runConfigReachLint(cwd, cfg) })
	}

	// Load project config for lint defaults. A missing config file is fine
	// (we fall back to defaults), but a parse/read error should fail hard so
	// users don't silently lint with the wrong configuration.
	_, cfg, err := loadLintConfig()
	if err != nil {
		return err
	}

	// No flags set — run ALL linters, each skipping gracefully if tool not
	// available. Auto-fix-then-gate is the DEFAULT: the deterministic-safe
	// fixers (Go formatting, golangci safe autofixes, eslint --fix) are applied
	// first so mechanical issues never gate. --no-fix opts out (CI / read-only);
	// the legacy --fix flag is now redundant (auto-fix is the default) but still
	// accepted so existing invocations keep working.
	return runAllLinters(ctx, lintRunOptions{
		fix:           !flags.noFix,
		strict:        flags.strict,
		skipFrontends: flags.skipFrontends,
		paths:         paths,
		cfg:           cfg,
	})
}

// lintRunOptions carries the whole-pipeline inputs from the flag layer to
// the two drivers (text and JSON). It exists so a new pipeline-wide
// switch is one field rather than another positional bool on two
// signatures — the shape the pre-existing (fix, strict, paths, cfg) list
// was already straining against.
type lintRunOptions struct {
	fix           bool
	strict        bool
	skipFrontends bool
	paths         []string
	cfg           *config.ProjectConfig
}

// loadLintConfig loads the project store and its config for the lint
// command. A missing forge.yaml is fine (returns a nil store and nil cfg);
// a parse/read error fails hard with the shared "failed to load project
// config" message so users don't silently lint with the wrong
// configuration. Callers that only need the config discard the store.
func loadLintConfig() (*projectstore.Store, *config.ProjectConfig, error) {
	store, err := loadProjectStore()
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		return nil, nil, fmt.Errorf("failed to load project config: %w", err)
	}
	var cfg *config.ProjectConfig
	if store != nil {
		cfg = store.Config()
	}
	return store, cfg, nil
}

// runWithCwd resolves the current working directory (failing with the shared
// "getwd" error) and invokes run with it. Factors out the getwd boilerplate
// the cwd-rooted targeted linters all share.
func runWithCwd(run func(cwd string) error) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	return run(cwd)
}

// contractExcludesFromConfig returns the contracts.exclude list from the
// project config, or nil when no config is loaded.
func contractExcludesFromConfig(cfg *config.ProjectConfig) []string {
	if cfg == nil {
		return nil
	}
	return cfg.Contracts.Exclude
}

// runContractLinter runs the contract-interface analysis IN-PROCESS
// (see contract_inprocess.go): the analyzers are imported from
// internal/linter/contract and driven with go/packages +
// go/analysis/checker inside the forge binary itself, so the analysis
// is exactly as new as forge — no PATH lookup, no separately-installed
// binary, no version skew. The historical subprocess driver
// (resolveContractLintBinary → contractlint on PATH / bin/ / `go run`)
// let a stale ~/go/bin/contractlint produce phantom violations against
// a fresh forge with nothing to catch the mismatch.
func runContractLinter(ctx context.Context, paths []string, excludes []string) error {
	fmt.Println("🔍 Running contract interface enforcement linter (in-process)...")
	fmt.Println()

	diags, err := runContractAnalysisInProcess(ctx, paths, excludes)
	if err != nil {
		return cliutil.WrapUserErr("forge lint --contract",
			"failed to run contract linter", "",
			"ensure the project builds (`go build ./...`) and go.mod is tidy", err)
	}

	if len(diags) > 0 {
		for _, d := range diags {
			fmt.Println(d)
		}
		fmt.Println()
		fmt.Println("❌ Contract interface violations found!")
		fmt.Println()
		fmt.Println("Exported methods on types implementing contract interfaces must be declared in the interface.")
		return cliutil.UserErr("forge lint --contract",
			"contract interface violations found",
			"",
			"either declare the exported method in the contract interface, or unexport it (lowercase) if it's helper-only")
	}

	fmt.Println("✅ No contract interface violations found!")
	return nil
}

// runConventionLint runs the forge-convention analyzers (forgeconv) over
// every .proto file in the project, plus the internal-package contract
// shape analyzer over every internal/<pkg>/contract.go. These analyzers
// catch the failure modes the overnight migration kept hitting: messages
// that look like entities by field-name but lack the explicit annotations
// forge codegen requires; contract.go files using non-canonical names
// (Sender/Config/NewSender) that produce broken bootstrap codegen.
// Findings with severity=error fail `forge lint`; warnings are printed
// but do not gate the build.
func runConventionLint(opts forgeconv.LintOptions) error {
	fmt.Println("Running forge convention rules...")

	combined, notes, hasAny, err := collectConventionFindings(opts)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  ⚠️  " + n)
	}
	if !hasAny {
		return nil
	}

	if len(combined.Findings) == 0 {
		fmt.Println("✓ forge conventions passed")
		return nil
	}

	fmt.Print(combined.FormatText())
	if combined.HasErrors() {
		return cliutil.UserErr("forge lint --conventions",
			"forge convention violations found",
			"",
			"fix the findings above (proto annotations, contract.go names) — see 'forge skill load contracts' and 'forge skill load proto' for the rules")
	}
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// collectConventionFindings gathers every forge-convention finding
// without printing — the shared engine behind runConventionLint (text)
// and `forge lint --json`. Returns the combined findings, any
// informational skip notes ("No proto/ directory found — …"), and
// hasAny=false when none of the lintable trees (proto/, internal/,
// handlers/, workers/, operators/) exist at all.
func collectConventionFindings(opts forgeconv.LintOptions) (forgeconv.Result, []string, bool, error) {
	combined := forgeconv.Result{}
	var notes []string
	hasProto := false

	if _, err := os.Stat("proto"); err == nil {
		hasProto = true
		res, err := forgeconv.LintProtoTreeOpts("proto", opts)
		if err != nil {
			return combined, notes, false, fmt.Errorf("forge convention lint (proto) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)
	} else {
		notes = append(notes, "No proto/ directory found — skipping proto convention lint")
	}

	// Internal-package contract shape, plus the dep-interface rule and
	// the `// forge:outbound-io` convention `--type=adapter` stamps. All
	// three rules live in internal/contractcheck and ship through one
	// Inspect call so the engine controls ordering and de-dup; the
	// per-rule severity / gating discipline is preserved (contract-names
	// is an error; the other two are warnings).
	//
	// Runs whether or not proto/ exists — CLI/library projects without
	// proto can still ship internal/ packages whose bootstrap codegen
	// would silently break on a wrong-named contract.
	hasInternal := false
	if _, err := os.Stat("internal"); err == nil {
		hasInternal = true
		store, cfgErr := loadProjectStore()
		if cfgErr != nil && !errors.Is(cfgErr, ErrProjectConfigNotFound) {
			return combined, notes, false, fmt.Errorf("failed to load project config for contract-shape lint: %w", cfgErr)
		}
		var cfg *config.ProjectConfig
		if store != nil {
			cfg = store.Config()
		}
		// The convention lint is not (yet) ctx-aware; the engine's
		// inter-rule cancellation hook is a forward-looking concern.
		// Using context.Background() preserves today's behavior; threading
		// the cobra cmd.Context() through is a separate cleanup.
		fs, err := contractcheck.Inspect(context.Background(), ".", contractcheck.Options{
			Excludes: contractExcludesFromConfig(cfg),
		})
		if err != nil {
			return combined, notes, false, fmt.Errorf("forge convention lint (contract-shape) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, fs...)
	}

	// Handler-tree analyzers — only run when handlers/ exists. The
	// no-handler-error-mapping rule catches per-service `toConnectError`
	// / `mapServiceError` re-rolls; canonical replacement is
	// `svcerr.Wrap(err)` from forge/pkg/svcerr. Warnings only — false-
	// positive risk is real (some projects have legitimate custom mapping
	// for project-specific sentinels), and the build should not gate on a
	// hand-rolled helper that hasn't been migrated yet.
	hasHandlers := false
	if _, err := os.Stat("internal/handlers"); err == nil {
		hasHandlers = true
		res, err := forgeconv.LintHandlerErrorMapping(".")
		if err != nil {
			return combined, notes, false, fmt.Errorf("forge convention lint (handler error mapping) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)

		// Handler-file size — warns when any handlers/<svc>/*.go grows
		// past lint.handler_file_max_loc (default 1000). The threshold
		// is project-configurable via forge.yaml. Warnings only — the
		// nudge points at the future `forge scaffold handler-file` split
		// subcommand rather than blocking on file size.
		store, cfgErr := loadProjectStore()
		if cfgErr != nil && !errors.Is(cfgErr, ErrProjectConfigNotFound) {
			return combined, notes, false, fmt.Errorf("failed to load project config for handler-file-size lint: %w", cfgErr)
		}
		threshold := config.DefaultHandlerFileMaxLOC
		if store != nil {
			threshold = store.Lint().EffectiveHandlerFileMaxLOC()
		}
		sizeRes, err := forgeconv.LintHandlerFileSize(".", threshold)
		if err != nil {
			return combined, notes, false, fmt.Errorf("forge convention lint (handler file size) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, sizeRes.Findings...)
	}

	// Component-tree analyzers — also run on workers/ and operators/
	// (which have the same Deps-struct shape as handlers/). The
	// optional-dep-marker-position rule catches misplaced
	// `// forge:optional-dep` markers (typo'd onto a struct rather
	// than a field, or onto a non-Deps type) — the marker has no
	// effect when it's not on a Deps field, and silent failure is
	// exactly the kind of bug a lint rule earns its keep on.
	hasComponentTree := hasHandlers
	for _, sub := range []string{"internal/workers", "internal/operators"} {
		if _, err := os.Stat(sub); err == nil {
			hasComponentTree = true
		}
	}
	if hasComponentTree {
		res, err := forgeconv.LintOptionalDepMarkerPosition(".")
		if err != nil {
			return combined, notes, false, fmt.Errorf("forge convention lint (optional-dep marker position) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)
	}

	// Frontend forge-owned-dotenv hygiene — a committed .env.local / .env*
	// under a frontend that hard-codes a forge-owned variable
	// (*_MOCK_API / *_API_URL / *_OTEL_ENDPOINT / *_ENVIRONMENT) fights the
	// KCL-injected value. WARN here in `forge lint`; the SAME analyzer runs
	// at ERROR on the build/deploy path (deploy.go's gateFrontendEnvFiles),
	// so a forge-owned dotenv can never reach a shipped build.
	hasFrontends := false
	if feDirs := frontendDirsForLint(); len(feDirs) > 0 {
		hasFrontends = true
		res := forgeconv.LintFrontendEnvFiles(".", feDirs, forgeconv.SeverityWarning)
		combined.Findings = append(combined.Findings, res.Findings...)

		// Raw process.env / import.meta.env reads in frontend source —
		// the frontend mirror of the backend's os.Getenv guardrail. The
		// analyzer is silent on any frontend that has not adopted
		// proto-declared config (no src/lib/config_gen.ts), so this
		// cannot light up a project that has nowhere else to read from;
		// see internal/linter/forgeconv/frontend_process_env.go.
		//
		// WARN by default, for the same reason the backend's
		// enforce_typed_access defaults to warn: adoption is
		// incremental, and a frontend mid-migration must not go red the
		// day it declares its first config field. A project that has
		// finished migrating gates it with
		// `lint.rules: {forgeconv-frontend-process-env: error}` — the
		// existing repo-wide dial, not a second knob.
		//
		// The analyzer emits at that default and `lint.rules` re-grades
		// through the shared engine, rather than the severity being
		// resolved here, so `off` (drop the finding), `warn`, `error`
		// and the `*` wildcard all behave exactly as they do for every
		// other forge rule — no second implementation of the same
		// three-state dial to drift out of step.
		peRes := forgeconv.LintFrontendProcessEnv(".", feDirs, forgeconv.SeverityWarning)
		peFindings := peRes.Findings
		if store, err := loadProjectStore(); err == nil && store != nil {
			peFindings = store.Lint().EffectiveRules().ApplyAll(peFindings)
		}
		combined.Findings = append(combined.Findings, peFindings...)
	}

	// If none of proto/, internal/, handlers/, workers/, operators/, or a
	// frontend dir exist, there's nothing to lint.
	hasAny := hasProto || hasInternal || hasComponentTree || hasFrontends
	return combined, notes, hasAny, nil
}

// frontendDirsForLint resolves the frontend source dirs the forge-owned
// dotenv rule scans, relative to the project root (cwd). It prefers the
// declared frontends (forge.yaml frontends[].path — honors custom /
// sibling paths), and falls back to a shallow scan of the conventional
// `frontends/` directory when no config is loadable. Returns nil when the
// project declares no frontend and has no frontends/ dir.
func frontendDirsForLint() []string {
	if store, err := loadProjectStore(); err == nil && store != nil {
		if cfg := store.Config(); cfg != nil && len(cfg.Frontends) > 0 {
			var dirs []string
			for _, fe := range cfg.Frontends {
				// A frontend with no directory in this repository (a
				// cross-repo source pin, a sibling-checkout path) has no
				// dotenv file here to lint.
				dir, ok := fe.Dir(".")
				if !ok {
					continue
				}
				dirs = append(dirs, dir)
			}
			return dirs
		}
	}
	entries, err := os.ReadDir("frontends")
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("frontends", e.Name()))
		}
	}
	return dirs
}

// runFrontendStoresLint scans frontends/<name>/src/stores/*.ts and the
// historic web/src/store/*.ts for files that both spin up a Zustand
// store AND import a generated Connect client — the canonical
// "server data in client-only state" foot-gun. Warnings only; the
// build is never gated. See the frontend/state skill for the
// canonical placement (generated React Query hooks).
func runFrontendStoresLint() error {
	fmt.Println("🔍 Running frontend-stores convention lint...")
	fmt.Println()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	res, err := forgeconv.LintFrontendStores(cwd)
	if err != nil {
		return fmt.Errorf("frontend-stores lint failed: %w", err)
	}
	if len(res.Findings) == 0 {
		fmt.Println("✓ no frontend store / generated-client mixing detected.")
		return nil
	}
	fmt.Print(res.FormatText())
	// Warnings only — never gate.
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// runTestsLint runs test-convention rules. Currently:
//
//   - forgeconv-handler-tests-use-tdd: warns when a handler test file
//     hand-rolls the `tests := []struct{name, call}` shape instead of
//     `tdd.RunRPCCases`. See `forge skill load
//     migrations/v0.x-to-tdd-rpccases` for the conversion playbook, or
//     run `forge project migrate tdd` to convert most files automatically.
//
//   - forgeconv-frontend-hook-tests: warns when a generated
//     frontends/<name>/src/hooks/<svc>-hooks.ts has no sibling test or
//     `.tsx.starter` waiting to be activated. See the `frontend-testing`
//     skill.
//
//   - forgeconv-frontend-hook-test-shape: ERRORS when a scaffolded hook
//     test asserts the opposite shape from the hook it exercises — a
//     query assertion against a useMutation hook, or vice versa. That
//     pair can never pass, so it is a red suite rather than a coverage
//     gap, and it gates.
//
// The first two are warnings and never gate: they surface drift, not
// block legitimate pre-`tdd.RunRPCCases` projects or frontends that
// genuinely don't want hook tests. The shape rule is the exception,
// because a test that cannot pass is not a matter of taste.
func runTestsLint() error {
	fmt.Println("🔍 Running test convention lint...")
	fmt.Println()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	handlerRes, err := forgeconv.LintHandlerTests(cwd)
	if err != nil {
		return fmt.Errorf("handler-test lint failed: %w", err)
	}
	frontendRes, err := forgeconv.LintFrontendHookTests(cwd)
	if err != nil {
		return fmt.Errorf("frontend-hook-test lint failed: %w", err)
	}
	shapeRes, err := forgeconv.LintFrontendHookTestShape(cwd)
	if err != nil {
		return fmt.Errorf("frontend-hook-test-shape lint failed: %w", err)
	}

	combined := forgeconv.Result{
		Findings: append(append(append([]forgeconv.Finding{},
			handlerRes.Findings...), frontendRes.Findings...), shapeRes.Findings...),
	}
	if len(combined.Findings) == 0 {
		fmt.Println("✓ test conventions passed")
		return nil
	}
	fmt.Print(combined.FormatText())
	if combined.HasErrors() {
		return fmt.Errorf("a scaffolded hook test contradicts the hook it exercises and cannot pass; " +
			"fix the block named above, or delete the test file and re-run `forge generate` for a fresh one")
	}
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// runBannersLint verifies forge's own template files carry the
// lifecycle banner that matches their tier:
//
//   - Tier 1 (forge-owned, regenerated every run): "// Code generated
//     by forge. DO NOT EDIT." + "// forge-owned: regenerated every run
//     — do not edit (forge project disown to take ownership)"
//   - Tier 2 (yours): "// yours: scaffolded once, never touched again
//     — forge will not overwrite this file"
//   - Fragments / skip-listed files: banner-less by design.
//
// Warnings only — the rule is a hint to template authors that LLMs and
// humans alike rely on the banner to know whether they may edit the
// emitted file. Runs only when invoked inside the forge repo (i.e.
// when `internal/templates/` exists); no-op elsewhere.
func runBannersLint() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	hasTemplates := dirExists(filepath.Join("internal", "templates"))
	if !hasTemplates {
		// No templates to lint — silently skip so the lint stays a no-op
		// outside the forge repo.
		return nil
	}

	fmt.Println("Running lifecycle-banner lint on forge templates...")
	res, err := scaffolds.BannerLintRoot(cwd)
	if err != nil {
		return fmt.Errorf("banner lint failed: %w", err)
	}
	if len(res.Findings) == 0 {
		fmt.Println("  banner lint passed")
		return nil
	}
	fmt.Print(res.FormatText())
	// Warnings only — never gate.
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// runCheckWorkaroundsLint flags the canonical cross-lane workarounds that
// shipped to cpnext during the v0.2 rebuild (cast<X>Repo helpers in
// pkg/app/wire_gen.go, hand-rolled pkg/app/testing_extras.go, cmd/<name>.go
// files not declared in forge.yaml's binaries: block). All findings are
// warnings — never gates the build, but surfaces drift before merge so
// the corresponding forge primitive can replace the workaround.
//
// Wired into runAllLinters so plain `forge lint` runs it; `--check-workarounds`
// runs ONLY this rule for targeted CI gates.
func runCheckWorkaroundsLint(root string) error {
	fmt.Println("🔍 Running cross-lane workaround lint...")
	fmt.Println()

	res, err := scaffolds.LintWorkaroundsRoot(root)
	if err != nil {
		return fmt.Errorf("check-workarounds lint failed: %w", err)
	}
	if len(res.Findings) == 0 {
		fmt.Println("✓ no cross-lane workarounds detected.")
		return nil
	}
	fmt.Print(res.FormatText())
	// Warnings only — never gate.
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// runScaffoldsLint enforces the forge ownership boundary:
//   - surviving FORGE_SCAFFOLD markers are a warning (pending scaffold
//     work — a fresh scaffold always carries them, so they must never
//     gate the build)
//   - _gen files missing the canonical header are an error
//   - _gen files missing a "Source:" line are a warning
//
// The walk skips heavyweight directories (gen/, node_modules/, .git/, …)
// so it stays cheap on real projects.
func runScaffoldsLint() error {
	fmt.Println("🔍 Running scaffold ownership lint...")
	fmt.Println()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	res, err := scaffolds.LintRoot(cwd)
	if err != nil {
		return fmt.Errorf("scaffold lint failed: %w", err)
	}

	fmt.Print(res.FormatText())
	if res.HasErrors() {
		return cliutil.UserErr("forge lint --scaffolds",
			"scaffold ownership violations found",
			"",
			"ensure _gen files carry the canonical 'Code generated by forge' header (re-run 'forge generate')")
	}
	if len(res.Findings) > 0 {
		fmt.Println("(warnings only — not failing the build)")
	}
	return nil
}

func runMigrationSafetyLint(cfg *config.ProjectConfig) error {
	fmt.Println("🔍 Running SQL migration safety lint...")
	fmt.Println()

	migrationsDir := filepath.Join("db", "migrations")
	ruleConfig := migrationlint.DefaultConfig()
	if cfg != nil {
		if cfg.Database.MigrationsDir != "" {
			migrationsDir = cfg.Database.MigrationsDir
		}
		ruleConfig = migrationlint.ConfigFromProject(cfg.Database.MigrationSafety)
	}

	result, err := migrationlint.LintMigrationsDir(migrationsDir, ruleConfig)
	if err != nil {
		return fmt.Errorf("migration safety lint failed: %w", err)
	}
	fmt.Print(result.FormatText())
	if result.HasErrors() {
		return cliutil.UserErr("forge lint --migration-safety",
			"migration safety violations found",
			"",
			migrationlint.DestructiveChangeRemediation)
	}
	return nil
}

// hasWorkspaceGoMod reports whether the current working directory (or any
// parent up to the filesystem root) contains a go.work file. This signals
// that the project relies on Go workspace mode to wire local module
// checkouts (e.g. forge's own forge/ + forge/pkg/ pair), and we must not
// override GOWORK when running analyzer subprocesses.
func hasWorkspaceGoMod() bool {
	dir, err := os.Getwd()
	if err != nil {
		return false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// appendEnvIfUnset appends key=value to env only if key is not already set.
func appendEnvIfUnset(env []string, key, value string) []string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

// ensureEnvDefault sets key=defaultValue if the key is missing or set to an
// empty string. If the key already has a non-empty value it is left unchanged.
func ensureEnvDefault(env []string, key, defaultValue string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			// Key exists — only override when the value is empty.
			if e == prefix {
				env[i] = prefix + defaultValue
			}
			return env
		}
	}
	// Key not present at all — add it.
	return append(env, prefix+defaultValue)
}

// runAllLinters runs every linter, each skipping gracefully if the
// required tool isn't installed. It is a thin TEXT renderer over the
// shared linter table (lintPipeline, in lint_steps.go) — the same table
// the JSON aggregator (collectAllLintersJSON) renders. The ordering,
// feature gates, dir checks, and gating verdict are declared ONCE in the
// table; this driver only translates each step into human output.
func runAllLinters(ctx context.Context, opts lintRunOptions) error {
	fmt.Println("🔍 Running all linters...")
	fmt.Println()

	fix := opts.fix
	cwd := lintCwd()

	// Auto-fix-then-gate: apply the deterministic-safe Go formatter across the
	// whole source tree (owned + generated) BEFORE the gating pipeline, so
	// import grouping / gofmt whitespace is fixed in place and never surfaces
	// as a golangci-lint gate. golangci-lint --fix (fix flag, below) and eslint
	// --fix (frontend step) cover the rest. Skipped under --no-fix (fix=false).
	if fix && cwd != "" {
		reportFormatPrePass(cwd)
	}

	rc := &lintRunCtx{
		ctx:           ctx,
		fix:           fix,
		strict:        opts.strict,
		skipFrontends: opts.skipFrontends,
		paths:         opts.paths,
		cfg:           opts.cfg,
		cwd:           cwd,
	}
	hasFailed := false

	// Which GATING linters actually executed, and which were skipped. The
	// verdict has to name them: "✅ All linters passed!" was printed
	// unconditionally, so a machine without golangci-lint and without buf —
	// a fresh CI runner whose tool install silently failed — got a green
	// `forge lint` having gated on nothing. The ⚠️ skip lines scrolled past
	// above; the last line on screen, the one a human and an agent both
	// read as the answer, claimed every linter passed.
	// unavailable is the third bucket: lanes that were supposed to execute
	// and could not. See laneUnavailableError for why they are neither
	// "ran" nor "skipped".
	var ranGating, skippedGating, unavailable []string

	for _, step := range lintPipeline() {
		run, skipMsg := step.shouldRun(rc)
		if !run {
			// A skip message prints a ⚠️ line; a silent skip (directory
			// absent / cwd unavailable) prints nothing — a lane that does
			// not apply to this project shape is not a gap.
			if skipMsg != "" {
				fmt.Println("⚠️  " + skipMsg)
				if step.gates {
					skippedGating = append(skippedGating, step.name)
				}
			}
			continue
		}

		err := step.runText(rc)
		var unavail *laneUnavailableError
		switch {
		case err == nil:
			if step.gates {
				ranGating = append(ranGating, step.name)
			}
		case errors.As(err, &unavail):
			// Deliberately NOT appended to ranGating: counting a lane that
			// produced no verdict would be the final line claiming coverage
			// this run does not have.
			unavailable = append(unavailable, step.name)
			marker := "⚠️ "
			if rc.strict {
				marker = "❌"
				hasFailed = true
			}
			fmt.Fprintf(os.Stderr, "  %s %s did NOT run: %s\n     ↳ %s\n",
				marker, step.name, unavail.reason, unavail.fixHint)
		default:
			// It ran and reported problems — that IS coverage, so the lane
			// counts as having run even though it failed.
			if step.gates {
				ranGating = append(ranGating, step.name)
				hasFailed = true
			}
			fmt.Fprintf(os.Stderr, step.errFormat, err)
		}
	}

	if hasFailed {
		return cliutil.UserErr("forge lint",
			"one or more linters reported errors, or could not run under --strict",
			"",
			"address the per-linter errors above (each preceded by ❌); re-run 'forge lint' to confirm")
	}

	fmt.Println()
	return reportLintVerdict(os.Stdout, ranGating, skippedGating, unavailable)
}

// reportLintVerdict renders the final line of `forge lint` — the one a human
// scrolls to and an agent greps for — from what actually ran.
//
// The rule: a verdict may only claim what the run proved. Zero gating linters
// executed means the run proved nothing and must fail; a partial run must name
// the lanes it skipped rather than round up to "All linters passed!"; and a
// lane that was supposed to execute and could NOT must be named too, because
// its absence is the one thing a green last line would hide.
func reportLintVerdict(w io.Writer, ranGating, skippedGating, unavailable []string) error {
	if len(ranGating) == 0 {
		detail := "no gating linter was applicable here"
		switch {
		case len(unavailable) > 0 && len(skippedGating) > 0:
			detail = "could not run: " + strings.Join(unavailable, ", ") +
				"; skipped: " + strings.Join(skippedGating, ", ")
		case len(unavailable) > 0:
			detail = "could not run: " + strings.Join(unavailable, ", ")
		case len(skippedGating) > 0:
			detail = "skipped: " + strings.Join(skippedGating, ", ")
		}
		return cliutil.UserErr("forge lint",
			"no gating linter ran — this run proved nothing",
			detail,
			"install the missing tools ('forge tools install', or golangci-lint + buf) and re-run from the project root")
	}

	// A lane that could not run demotes the headline. A SKIP is the project
	// saying "this does not apply here" and is compatible with a ✅; a lane
	// that was supposed to execute and did not is a hole in the coverage the
	// verdict would otherwise be claiming.
	if len(unavailable) > 0 {
		line := fmt.Sprintf("⚠️  %d gating linter(s) passed; %d could NOT run (%s)",
			len(ranGating), len(unavailable), strings.Join(unavailable, ", "))
		if len(skippedGating) > 0 {
			line += fmt.Sprintf("; %d skipped (%s)", len(skippedGating), strings.Join(skippedGating, ", "))
		}
		fmt.Fprintln(w, line)
		fmt.Fprintln(w, "   ↳ a lane that could not run is not a lane that passed — re-run, or pass --strict to fail on it")
		return nil
	}

	if len(skippedGating) > 0 {
		fmt.Fprintf(w, "✅ %d gating linter(s) passed; %d skipped (%s)\n",
			len(ranGating), len(skippedGating), strings.Join(skippedGating, ", "))
		return nil
	}
	fmt.Fprintf(w, "✅ All %d gating linters passed.\n", len(ranGating))
	return nil
}

// ── "could not run" is not "passed" ─────────────────────────────────────────
//
// A lint lane has three outcomes that are not interchangeable, and collapsing
// any two of them is how a check goes silently absent:
//
//   - it did not APPLY — no proto/ tree, feature disabled, tool deliberately
//     not installed. The shouldRun path; reported as a named skip.
//   - it RAN — and found problems, or did not. The ordinary error/nil path.
//   - it could NOT RUN — it was supposed to execute and the attempt failed,
//     so the lane produced no verdict at all.
//
// The third had no vocabulary. runTypedAccessGuardAdvisory caught its
// golangci-lint failure, printed "⚠️  typed-config guardrail check could not
// run", and returned nil — so the pipeline never learned the lane was absent.
// The run exited 0 and the final verdict counted only the lanes that DID run
// and pronounced the project clean. `forge lint --json` was worse: it emitted
// "ok": true over a check that never executed, which is precisely the
// assertion a machine consumer acts on.
//
// The trigger is ordinary, not hypothetical. golangci-lint takes an exclusive
// OS file lock on $TMPDIR/golangci-lint.lock — ONE path shared by every
// golangci-lint on the machine, not per-project and not in the cache dir —
// and waits five seconds for it before exiting 3 with "parallel golangci-lint
// is running". Five seconds is no patience at all against a lint that takes
// tens of seconds, so an editor's golangci-lint LSP, a sibling CI matrix job,
// or a monorepo linting two services at once takes that lock between `forge
// lint`'s two invocations and the guardrail lane evaporates in silence.
//
// laneUnavailableError gives the condition a name any lane can return. The
// driver routes it to its own bucket: never counted as a gating linter that
// passed, always named in the final verdict, and escalated to a build failure
// under --strict — the same escalation the frontend typecheck lane already
// documents for its own "could not run" (see lint_frontend_typecheck.go).
//
// Deliberately NOT a hard failure by default. These lanes are advisory BY
// DESIGN: in `warn` mode the generated .golangci.yml leaves forbidigo out of
// linters.enable, so nothing gates on the guardrail's FINDINGS. Gating on its
// ABSENCE would be strictly stronger than gating on its presence, and it
// would fail `forge lint` over something outside the project entirely —
// another process holding a machine-global lock. Honest and visible is the
// fix; --strict is the gate for callers who want one.
type laneUnavailableError struct {
	// reason completes "<lane> did NOT run: <reason>".
	reason string
	// fixHint says what to DO about it.
	fixHint string
}

func (e *laneUnavailableError) Error() string { return e.reason }

// laneUnavailable builds the sentinel a lane returns when it could not run.
func laneUnavailable(fixHint, format string, a ...any) error {
	return &laneUnavailableError{reason: fmt.Sprintf(format, a...), fixHint: fixHint}
}

func runGolangciLint(ctx context.Context, fix bool, paths []string) error {
	fmt.Println("Running golangci-lint...")

	args := []string{"run"}
	if fix {
		args = append(args, "--fix")
	}
	args = append(args, paths...)

	cmd := exec.CommandContext(ctx, "golangci-lint", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	fmt.Println("✓ golangci-lint passed")
	return nil
}

// runTypedAccessGuardAdvisory runs the env-access guardrail (forbidigo) as a
// non-failing, advisory pass. It is the `warn` arm of
// config.enforce_typed_access: the gating `.golangci.yml` run deliberately
// leaves forbidigo OUT of its `linters.enable` list (so it never fails the
// build), and this step surfaces the same findings with --issues-exit-code=0
// so violations are visible but never gate. `error` mode skips this step —
// there forbidigo is enabled in the main gating run instead.
//
// Findings print verbatim. A non-zero exit can only mean the INVOCATION
// failed — --issues-exit-code=0 already neutralizes the findings exit code —
// so it is reported as a lane that could not run (laneUnavailableError), not
// swallowed into a ⚠️ and a nil return.
func runTypedAccessGuardAdvisory(ctx context.Context, paths []string) error {
	fmt.Println("Checking typed-config guardrail (advisory)...")

	args := []string{"run", "--enable-only=forbidigo", "--issues-exit-code=0"}
	args = append(args, paths...)

	cmd := exec.CommandContext(ctx, "golangci-lint", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// --issues-exit-code=0 means findings CANNOT produce a non-zero
		// exit, so getting one here says golangci-lint never got far enough
		// to report: this lane has no verdict to give. Returning nil (what
		// this used to do) told the driver the guardrail had run and found
		// nothing, and `forge lint` exited 0 over a check that never
		// executed.
		return laneUnavailable(typedAccessGuardUnavailableHint,
			"golangci-lint exited %v before the forbidigo pass could report", err)
	}
	fmt.Println("✓ typed-config guardrail check complete (advisory). To gate the build on these findings, set " +
		"config.enforce_typed_access: error in forge.yaml and re-render .golangci.yml (forge project upgrade).")
	return nil
}

// typedAccessGuardUnavailableHint is the remediation for a guardrail
// invocation that never reported. It leads with golangci-lint's
// machine-global lock because that is overwhelmingly the cause: the lock
// lives at $TMPDIR/golangci-lint.lock, is shared by every golangci-lint on
// the machine regardless of project or cache, and the default patience for
// it is five seconds.
const typedAccessGuardUnavailableHint = "re-run `forge lint`. If another golangci-lint was running on this machine " +
	"(an editor's golangci-lint LSP, a sibling CI job, another service in the same monorepo) it held the " +
	"machine-global lock at $TMPDIR/golangci-lint.lock and this invocation got five seconds: set " +
	"`allow-serial-runners: true` under `run:` in .golangci.yml so invocations queue instead of failing " +
	"(forge scaffolds that line — `forge project upgrade` re-renders it into an existing project)"

func runBufLint(ctx context.Context) error {
	if _, err := os.Stat("buf.yaml"); os.IsNotExist(err) {
		return nil
	}

	fmt.Println("Running buf lint...")

	// Capture stdout so we can scan for known migration-pain rules and
	// print the buf.yaml `except` snippet that resolves them. We still
	// stream the output to the user's terminal verbatim so nothing is
	// hidden behind the suggestion.
	cmd := exec.CommandContext(ctx, "buf", "lint")
	var bufOut strings.Builder
	cmd.Stdout = io.MultiWriter(os.Stdout, &bufOut)
	cmd.Stderr = io.MultiWriter(os.Stderr, &bufOut)

	if err := cmd.Run(); err != nil {
		printBufLintExceptHint(bufOut.String())
		return err
	}

	fmt.Println("✓ buf lint passed")
	return nil
}

// printBufLintExceptHint scans buf lint's output for STANDARD rules
// that legacy / ported .proto files commonly trip and prints the
// exact buf.yaml `lint.except` snippet that silences them. Migration
// projects (where the source repo predates the forge convention) tend
// to hit ALL four of these on the first `forge generate`; surfacing
// the resolved YAML in-line saves the "look up buf docs → write
// except → re-run" loop. FRICTION 2026-06-02: cp-forge proto port.
func printBufLintExceptHint(output string) {
	candidates := []string{
		"PACKAGE_VERSION_SUFFIX",
		"RPC_REQUEST_STANDARD_NAME",
		"RPC_RESPONSE_STANDARD_NAME",
		"RPC_REQUEST_RESPONSE_UNIQUE",
	}
	var hit []string
	for _, rule := range candidates {
		if strings.Contains(output, rule) {
			hit = append(hit, rule)
		}
	}
	if len(hit) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "💡 Migration hint: the rule(s) above are common when porting")
	fmt.Fprintln(os.Stderr, "   pre-forge .proto files. To silence them, add this to buf.yaml:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "     lint:")
	fmt.Fprintln(os.Stderr, "       use:")
	fmt.Fprintln(os.Stderr, "         - STANDARD")
	fmt.Fprintln(os.Stderr, "       except:")
	for _, rule := range hit {
		fmt.Fprintf(os.Stderr, "         - %s\n", rule)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "   See the proto / proto-breaking skills for context on each rule.")
}

// runFrontendLinters runs the rule-based frontend linters (eslint, and
// stylelint when css_health is on) for each frontend defined in the project
// config. Falls back to directory scanning if no config is available. When fix
// is set the eslint pass runs with --fix, the frontend half of the
// auto-fix-then-gate default.
//
// TypeScript typechecking is NOT here: it is its own pipeline step
// (lint_frontend_typecheck.go), so it gets its own severity dial and can
// still run when a frontend has no `typecheck` npm script. Running it in
// both lanes would typecheck every frontend twice.
func runFrontendLinters(ctx context.Context, cfg *config.ProjectConfig, fix bool) error {
	if cfg != nil && len(cfg.Frontends) > 0 {
		return runFrontendLintersFromConfig(ctx, cfg, fix)
	}

	// Fallback: scan frontends/ directory when no config is available
	if !dirExists("frontends") {
		return nil
	}

	entries, err := os.ReadDir("frontends")
	if err != nil {
		// The directory is there but unreadable — the lane cannot enumerate
		// what it was meant to lint. That is "could not run", not "clean".
		return laneUnavailable("fix the permissions on frontends/ (or declare the frontends in forge.yaml) and re-run",
			"frontends/ could not be read: %v", err)
	}

	var outcomes frontendLintOutcomes
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		feDir := filepath.Join("frontends", e.Name())
		outcomes.record(lintFrontendDir(ctx, e.Name(), feDir, "", false, fix))
	}
	return outcomes.verdict()
}

// runFrontendLintersFromConfig lints frontends using project config entries.
func runFrontendLintersFromConfig(ctx context.Context, cfg *config.ProjectConfig, fix bool) error {
	var outcomes frontendLintOutcomes
	for _, fe := range cfg.Frontends {
		// Skips a frontend whose code is in another repository: forge
		// lints what this project owns, and that tree's own forge lints
		// the rest.
		feDir, ok := fe.Dir(".")
		if !ok {
			continue
		}
		outcomes.record(lintFrontendDir(ctx, fe.Name, feDir, fe.Type, cfg.Lint.Frontend.CSSHealth, fix))
	}
	return outcomes.verdict()
}

// frontendLintOutcomes aggregates one lintFrontendDir result per frontend into
// the lane's single verdict, keeping "a linter failed" and "a linter could not
// run" apart across a multi-frontend project. A real failure outranks an
// unavailable one: if eslint actually reported errors somewhere, that is the
// answer, and the unavailable frontends are reported in the same run's output.
type frontendLintOutcomes struct {
	failed      bool
	unavailable []string
}

func (o *frontendLintOutcomes) record(err error) {
	if err == nil {
		return
	}
	var unavail *laneUnavailableError
	if errors.As(err, &unavail) {
		o.unavailable = append(o.unavailable, unavail.reason)
		return
	}
	o.failed = true
}

func (o *frontendLintOutcomes) verdict() error {
	if o.failed {
		return fmt.Errorf("frontend linting failed")
	}
	if len(o.unavailable) > 0 {
		return laneUnavailable(
			"run `npm install` in the frontend(s) named above and re-run `forge lint`, or pass --skip-frontends for a backend-only gate",
			"%s", strings.Join(o.unavailable, "; "))
	}
	return nil
}

// lintFrontendDir runs linters for a single frontend directory.
// feType can be "nextjs" or empty (unknown). When fix is set the npm lint
// script runs with `-- --fix` so eslint autofixes in place.
// The two "could not run" exits below (missing directory, missing
// node_modules) used to print a ⚠️ and return nil. This lane GATES, so a nil
// return made it count as a gating linter that PASSED — the verdict line
// claimed coverage over a frontend whose eslint was never invoked. They are
// classified exactly as the sibling typecheck lane classifies the same two
// conditions (lint_frontend_typecheck.go: typecheckUnavailable).
func lintFrontendDir(ctx context.Context, name, feDir, feType string, cssHealth, fix bool) error {
	if !dirExists(feDir) {
		return laneUnavailable(fmt.Sprintf("fix the `path:` for frontend %q in forge.yaml, or remove the entry", name),
			"%s: directory %s not found — eslint did NOT run", name, feDir)
	}

	pkgJSON := filepath.Join(feDir, "package.json")
	if _, err := os.Stat(pkgJSON); err != nil {
		// No package.json at all: not a Node project. A `frontends/`
		// subdirectory can legitimately be something else (a static export,
		// shared assets), so this is "does not apply", not "could not run".
		return nil
	}

	if _, err := os.Stat(filepath.Join(feDir, "node_modules")); os.IsNotExist(err) {
		return laneUnavailable(fmt.Sprintf("run `npm install` in %s, then re-run `forge lint`", feDir),
			"%s: node_modules not found in %s — eslint did NOT run", name, feDir)
	}

	scripts, err := readPackageScripts(pkgJSON)
	if err != nil {
		return fmt.Errorf("%s: read package.json scripts: %w", name, err)
	}

	fmt.Printf("Running frontend linters for %s...\n", name)
	hasFailed := false

	if hasPackageScript(scripts, "lint") {
		// Auto-fix-then-gate: with fix set, run the lint script with `-- --fix`
		// so eslint applies its deterministic-safe fixes in place before the
		// verdict, mirroring the Go formatter pre-pass. The re-run below the
		// script (eslint's own second pass) still gates on anything unfixable.
		lintArgs := []string{"run", "lint"}
		if fix {
			lintArgs = append(lintArgs, "--", "--fix")
		}
		if err := runNPMCommand(ctx, feDir, lintArgs...); err != nil {
			fmt.Fprintf(os.Stderr, "  ❌ %s: npm run lint failed: %v\n", name, err)
			hasFailed = true
		} else {
			fmt.Printf("  ✓ %s: lint passed\n", name)
		}
	} else if feType == "nextjs" {
		fmt.Printf("  ⚠️  %s: no npm lint script found; add one instead of relying on deprecated next lint\n", name)
	} else {
		fmt.Printf("  ⚠️  %s: no npm lint script found, skipping lint\n", name)
	}

	if cssHealth {
		if hasPackageScript(scripts, "lint:styles") {
			if err := runNPMCommand(ctx, feDir, "run", "lint:styles"); err != nil {
				fmt.Fprintf(os.Stderr, "  ❌ %s: npm run lint:styles failed: %v\n", name, err)
				hasFailed = true
			} else {
				fmt.Printf("  ✓ %s: style lint passed\n", name)
			}
		} else {
			fmt.Printf("  ⚠️  %s: css_health enabled but no npm lint:styles script found\n", name)
		}
	}

	if hasFailed {
		return fmt.Errorf("%s: frontend linting failed", name)
	}
	return nil
}

type packageJSONScripts struct {
	Scripts map[string]string `json:"scripts"`
}

func readPackageScripts(pkgJSON string) (map[string]string, error) {
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return nil, err
	}

	var pkg packageJSONScripts
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}
	return pkg.Scripts, nil
}

func hasPackageScript(scripts map[string]string, name string) bool {
	_, ok := scripts[name]
	return ok
}

// runNPMCommand runs an npm command in the given directory.
func runNPMCommand(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "npm", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
