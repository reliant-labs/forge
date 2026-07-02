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
	"github.com/reliant-labs/forge/internal/linter/frontendpacklint"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
	"github.com/reliant-labs/forge/internal/linter/scaffolds"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// lintFlags holds the flag values for the lint command.
type lintFlags struct {
	contract          bool
	migrationSafety   bool
	fix               bool
	exportedVars      bool
	conventions       bool
	frontendPacks     bool
	frontendStores    bool
	scaffolds         bool
	tests             bool
	banners           bool
	suggestExcludes   bool
	suggestBufExcepts bool
	wireCoverage      bool
	bootstrapCoverage bool
	checkWorkarounds  bool
	optionalDepsGuard bool
	configDeps        bool
	strict            bool
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
- Run TypeScript linters for Next.js frontends (if frontends/ exists)
- Optionally run targeted rule sets (--contract, --db, --migration-safety,
  --conventions, --tests)

Examples:
  forge lint                     # Run all standard linters
  forge lint --fix               # Auto-fix issues where possible
  forge lint --contract          # Run contract interface enforcement linter
  forge lint --db                # Run DB entity lint rules
  forge lint --migration-safety  # Run SQL migration safety checks
  forge lint --conventions       # Run forge convention rules on proto files
  forge lint --tests             # Run test-convention rules across backend
                                 # handlers and frontend hooks (warnings only)
  forge lint --json              # Machine-readable findings for sub-agents / CI
                                 # (schema in lint_json.go; exit code matches
                                 # text mode; combines with the targeted flags
                                 # above, but not with --fix / --suggest-*)

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
	cmd.Flags().BoolVar(&flags.frontendPacks, "frontend-packs", false, "Flag third-party UI imports in frontend pack templates (warnings only)")
	cmd.Flags().BoolVar(&flags.frontendStores, "frontend-stores", false, "Flag Zustand stores that import generated Connect clients (warnings only)")
	cmd.Flags().BoolVar(&flags.scaffolds, "scaffolds", false, "Enforce forge ownership boundary (FORGE_SCAFFOLD markers + _gen file headers)")
	cmd.Flags().BoolVar(&flags.tests, "tests", false, "Run test-convention rules (handler-tests-use-tdd + frontend-hook-tests; warnings only)")
	cmd.Flags().BoolVar(&flags.banners, "banners", false, "Verify forge templates carry the right Tier-1 / Tier-2 lifecycle banner (warnings only; no-op outside the forge repo)")
	cmd.Flags().BoolVar(&flags.suggestExcludes, "suggest-excludes", false, "Print a YAML snippet of contracts.exclude candidates (heuristic-based; nothing is mutated)")
	cmd.Flags().BoolVar(&flags.suggestBufExcepts, "suggest-buf-excepts", false, "Run buf lint and print a buf.yaml lint.except snippet for STANDARD rules that fired on more than 3 .proto files (nothing is mutated)")
	cmd.Flags().BoolVar(&flags.wireCoverage, "wire-coverage", false, "Report unresolved Deps fields in pkg/app/wire_gen.go (warnings) and unresolved forge:placeholder annotations in pkg/app/app_extras.go (errors)")
	cmd.Flags().BoolVar(&flags.bootstrapCoverage, "bootstrap-deps-coverage", false, "Verify pkg/app/bootstrap.go wires every package Deps field that name-matches an AppExtras field (catches the audit-no-op silent-drop bug class)")
	cmd.Flags().BoolVar(&flags.checkWorkarounds, "check-workarounds", false, "Flag cross-lane workarounds (cast<X>Repo helpers, testing_extras.go, undeclared cmd/<name>.go) — warnings only")
	cmd.Flags().BoolVar(&flags.optionalDepsGuard, "optional-deps-guard", false, "Flag unguarded derefs of `// forge:optional-dep` Deps fields (warnings only; suppress with `// forge:optional-checked` on the deref line)")
	cmd.Flags().BoolVar(&flags.configDeps, "config-deps", false, "Flag scalar Deps fields — scalars are configuration; declare a <Component>Config block in proto/config and take it as a typed field (warnings only)")
	cmd.Flags().BoolVar(&flags.strict, "strict", false, "Escalate advisory security findings to errors (e.g. RPCs missing a (forge.v1.method) auth-posture annotation) so they fail the build / CI")
	cmd.Flags().BoolVar(&flags.fix, "fix", false, "Automatically fix issues where possible")
	cmd.Flags().BoolVar(&flags.jsonOut, "json", false, "Output findings as JSON (see lint_json.go header for the schema; exit code matches text mode)")

	// User-vs-maintainer surface split: the flags below are fully
	// functional but hidden from --help (visible via --help-dev). The
	// visible set is pinned by TestLintHelpSurface — a new flag must
	// consciously pick a side. See help_dev.go for the rule of thumb.
	hideDevFlags(cmd,
		"exported-vars",           // forge-internal style rule
		"frontend-packs",          // lints forge's own pack templates
		"frontend-stores",         // convention audit, agent-workflow nudge
		"scaffolds",               // forge ownership-boundary enforcement
		"banners",                 // no-op outside the forge repo
		"suggest-excludes",        // one-shot migration/setup helper
		"suggest-buf-excepts",     // one-shot migration/setup helper
		"wire-coverage",           // DI wiring audit (forge codegen internals)
		"bootstrap-deps-coverage", // DI wiring audit (forge codegen internals)
		"check-workarounds",       // parallel-lane agent-workflow audit
		"optional-deps-guard",     // forge:optional-dep annotation audit
		"config-deps",             // Deps-shape convention audit
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
	if flags.frontendPacks {
		return runFrontendPackLint()
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
	if flags.wireCoverage {
		return runWithCwd(runWireCoverageLint)
	}
	if flags.bootstrapCoverage {
		return runWithCwd(runBootstrapDepsCoverageLint)
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

	// Load project config for lint defaults. A missing config file is fine
	// (we fall back to defaults), but a parse/read error should fail hard so
	// users don't silently lint with the wrong configuration.
	_, cfg, err := loadLintConfig()
	if err != nil {
		return err
	}

	// No flags set — run ALL linters, each skipping gracefully if tool not available.
	return runAllLinters(ctx, flags.fix, flags.strict, paths, cfg)
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

func runContractLinter(ctx context.Context, paths []string, excludes []string) error {
	fmt.Println("🔍 Running contract interface enforcement linter...")
	fmt.Println()

	binPath, err := resolveContractLintBinary(ctx)
	if err != nil {
		return err
	}

	// Build the argument list. -exclude is a top-level flag handled by
	// cmd/contractlint/main.go before multichecker.Main; it must precede the
	// positional package paths.
	var lintArgs []string
	if len(excludes) > 0 {
		lintArgs = append(lintArgs, "-exclude="+strings.Join(excludes, ","))
	}
	lintArgs = append(lintArgs, paths...)

	var lintExec *exec.Cmd
	if strings.HasSuffix(binPath, "main.go") {
		goArgs := append([]string{"run", binPath}, lintArgs...)
		lintExec = exec.CommandContext(ctx, "go", goArgs...)
		fmt.Printf("Running: go %s\n", strings.Join(goArgs, " "))
	} else {
		lintExec = exec.CommandContext(ctx, binPath, lintArgs...)
		fmt.Printf("Running: %s %s\n", binPath, strings.Join(lintArgs, " "))
	}

	// Inherit environment and set flags needed for analysis to resolve modules.
	// GOWORK=off prevents workspace-mode confusion in the analyzer.
	// GOFLAGS=-mod=mod allows the analyzer to fetch missing modules.
	//
	// Exception: if the project ships a go.work file (the case for forge
	// itself, which depends on its own pkg/ submodule via the workspace), we
	// must NOT override GOWORK — the workspace is what wires the local
	// pkg/ checkout in. -mod=mod is incompatible with workspace mode, so we
	// also skip the GOFLAGS override in that case.
	lintExec.Env = os.Environ()
	if hasWorkspaceGoMod() {
		// Honour the existing workspace; do not force -mod=mod (incompatible
		// with workspace mode). GOPROXY default is still useful for fetching
		// transitive deps that aren't in the workspace.
	} else {
		lintExec.Env = appendEnvIfUnset(lintExec.Env, "GOWORK", "off")
		lintExec.Env = appendEnvIfUnset(lintExec.Env, "GOFLAGS", "-mod=mod")
	}
	lintExec.Env = ensureEnvDefault(lintExec.Env, "GOPROXY", "https://proxy.golang.org,direct")
	lintExec.Stdout = os.Stdout
	lintExec.Stderr = os.Stderr
	fmt.Println()

	if err := lintExec.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 3 {
				fmt.Println()
				fmt.Println("❌ Contract interface violations found!")
				fmt.Println()
				fmt.Println("Exported methods on types implementing contract interfaces must be declared in the interface.")
				return cliutil.UserErr("forge lint --contract",
					"contract interface violations found",
					"",
					"either declare the exported method in the contract interface, or unexport it (lowercase) if it's helper-only")
			}
		}
		return cliutil.WrapUserErr("forge lint --contract",
			"failed to run contract linter", "",
			"ensure 'contractlint' builds (cmd/contractlint/main.go) and your project go.mod is tidy", err)
	}

	fmt.Println()
	fmt.Println("✅ No contract interface violations found!")
	return nil
}

func resolveContractLintBinary(ctx context.Context) (string, error) {
	if path, err := exec.LookPath("contractlint"); err == nil {
		return path, nil
	}

	localBin := filepath.Join("bin", "contractlint")
	if _, err := os.Stat(localBin); err == nil {
		return localBin, nil
	}

	srcPath := filepath.Join("cmd", "contractlint", "main.go")
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return "", fmt.Errorf("contract linter not found: not on PATH, not at bin/contractlint, and no source at %s", srcPath)
	}

	fmt.Println("Building contractlint from source...")
	if err := os.MkdirAll("bin", 0o755); err != nil {
		return srcPath, nil
	}

	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", localBin, "./cmd/contractlint")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		fmt.Printf("Warning: failed to build binary, falling back to go run: %v\n", err)
		return srcPath, nil
	}

	fmt.Printf("Built %s\n", localBin)
	return localBin, nil
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

	// Internal-package contract shape, plus the hexagonal-architecture
	// conventions for `--type=adapter` and `--type=interactor`. All
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
		// nudge points at the future `forge add handler-file` split
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

	// If none of proto/, internal/, handlers/, workers/, or operators/
	// exist, there's nothing to lint.
	hasAny := hasProto || hasInternal || hasComponentTree
	return combined, notes, hasAny, nil
}

// runFrontendPackLint scans frontend pack templates (under
// internal/packs/<name>/templates/*.tsx.tmpl) for direct third-party UI
// imports — the convention is that frontend packs wrap base library
// primitives. Findings are advisory warnings; the build is never gated.
//
// Soft-rule rationale: some packs legitimately need third-party deps
// (charts, maps, headless engines). Each pack opts those out via
// `allowed_third_party:` in its pack.yaml.
func runFrontendPackLint() error {
	packsRoot := filepath.Join("internal", "packs")
	if _, err := os.Stat(packsRoot); os.IsNotExist(err) {
		// Not a forge-repo workspace — silently skip. End-user projects
		// don't ship pack source.
		fmt.Println("⚠️  No internal/packs/ directory found — skipping frontend pack lint")
		return nil
	}

	fmt.Println("🔍 Running frontend pack convention lint...")
	fmt.Println()

	res, err := frontendpacklint.LintPacksRoot(packsRoot)
	if err != nil {
		return fmt.Errorf("frontend pack lint failed: %w", err)
	}

	fmt.Print(res.FormatText())
	// Soft rule — never gate the build, even on warnings.
	return nil
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
//     run `forge test migrate-tdd` to convert most files automatically.
//
//   - forgeconv-frontend-hook-tests: warns when a generated
//     frontends/<name>/src/hooks/<svc>-hooks.ts has no sibling test or
//     `.tsx.starter` waiting to be activated. See the `frontend-testing`
//     skill.
//
// All findings are warnings — never gates the build. The lint exists to
// surface drift, not block legitimate pre-`tdd.RunRPCCases` projects or
// frontends that genuinely don't want hook tests.
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

	combined := forgeconv.Result{
		Findings: append(append([]forgeconv.Finding{}, handlerRes.Findings...), frontendRes.Findings...),
	}
	if len(combined.Findings) == 0 {
		fmt.Println("✓ test conventions passed")
		return nil
	}
	fmt.Print(combined.FormatText())
	// Warnings only — never gate.
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// runBannersLint verifies forge's own template files carry the
// lifecycle banner that matches their tier:
//
//   - Tier 1 (forge-owned, regenerated every run): "// Code generated
//     by forge. DO NOT EDIT." + "// forge-owned: regenerated every run
//     — do not edit (forge disown to take ownership)"
//   - Tier 2 (yours): "// yours: scaffolded once, never touched again
//     — forge will not overwrite this file"
//   - Fragments / skip-listed files: banner-less by design.
//
// Warnings only — the rule is a hint to template authors that LLMs and
// humans alike rely on the banner to know whether they may edit the
// emitted file. Runs only when invoked inside the forge repo (i.e.
// when `internal/templates/` and/or `internal/packs/` exist); no-op
// elsewhere.
func runBannersLint() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	hasTemplates := dirExists(filepath.Join("internal", "templates")) ||
		dirExists(filepath.Join("internal", "packs"))
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
//   - committed FORGE_SCAFFOLD markers are an error
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
			"resolve any committed FORGE_SCAFFOLD: markers and ensure _gen files carry the canonical 'Code generated by forge' header (re-run 'forge generate')")
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
			"either rewrite the destructive migration as a non-destructive sequence, or allowlist the file under migration_safety.allowed_destructive in forge.yaml")
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
func runAllLinters(ctx context.Context, fix, strict bool, paths []string, cfg *config.ProjectConfig) error {
	fmt.Println("🔍 Running all linters...")
	fmt.Println()

	rc := &lintRunCtx{ctx: ctx, fix: fix, strict: strict, paths: paths, cfg: cfg, cwd: lintCwd()}
	hasFailed := false

	for _, step := range lintPipeline() {
		run, skipMsg := step.shouldRun(rc)
		if !run {
			// A skip message prints a ⚠️ line; a silent skip (directory
			// absent / cwd unavailable) prints nothing — both preserve the
			// pre-refactor text output exactly.
			if skipMsg != "" {
				fmt.Println("⚠️  " + skipMsg)
			}
			continue
		}
		if err := step.runText(rc); err != nil {
			fmt.Fprintf(os.Stderr, step.errFormat, err)
			if step.gates {
				hasFailed = true
			}
		}
	}

	if hasFailed {
		return cliutil.UserErr("forge lint",
			"one or more linters reported errors",
			"",
			"address the per-linter errors above (each preceded by ❌); re-run 'forge lint' to confirm")
	}

	fmt.Println()
	fmt.Println("✅ All linters passed!")
	return nil
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
// Findings print verbatim; a non-zero exit (only on a golangci-lint
// invocation error, never on findings) returns nil so the advisory step can
// never break the build.
func runTypedAccessGuardAdvisory(ctx context.Context, paths []string) error {
	fmt.Println("Checking typed-config guardrail (advisory)...")

	args := []string{"run", "--enable-only=forbidigo", "--issues-exit-code=0"}
	args = append(args, paths...)

	cmd := exec.CommandContext(ctx, "golangci-lint", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Advisory: a golangci-lint launch/config error is reported but
		// never gates. (--issues-exit-code=0 already neutralizes the
		// findings exit code, so this only fires on a genuine tool error.)
		fmt.Printf("⚠️  typed-config guardrail check could not run: %v\n", err)
		return nil
	}
	fmt.Println("✓ typed-config guardrail check complete (advisory). To gate the build on these findings, set " +
		"config.enforce_typed_access: error in forge.yaml and re-render .golangci.yml (forge upgrade).")
	return nil
}

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

// runFrontendLinters runs TypeScript type-checking and framework-specific linters
// for each frontend defined in the project config. Falls back to directory scanning
// if no config is available.
func runFrontendLinters(ctx context.Context, cfg *config.ProjectConfig) error {
	if cfg != nil && len(cfg.Frontends) > 0 {
		return runFrontendLintersFromConfig(ctx, cfg)
	}

	// Fallback: scan frontends/ directory when no config is available
	if !dirExists("frontends") {
		return nil
	}

	entries, err := os.ReadDir("frontends")
	if err != nil {
		return nil
	}

	hasFailed := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		feDir := filepath.Join("frontends", e.Name())
		if err := lintFrontendDir(ctx, e.Name(), feDir, "", false); err != nil {
			hasFailed = true
		}
	}

	if hasFailed {
		return fmt.Errorf("frontend linting failed")
	}
	return nil
}

// runFrontendLintersFromConfig lints frontends using project config entries.
func runFrontendLintersFromConfig(ctx context.Context, cfg *config.ProjectConfig) error {
	hasFailed := false
	for _, fe := range cfg.Frontends {
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		if err := lintFrontendDir(ctx, fe.Name, feDir, fe.Type, cfg.Lint.Frontend.CSSHealth); err != nil {
			hasFailed = true
		}
	}
	if hasFailed {
		return fmt.Errorf("frontend linting failed")
	}
	return nil
}

// lintFrontendDir runs linters for a single frontend directory.
// feType can be "nextjs" or empty (unknown).
func lintFrontendDir(ctx context.Context, name, feDir, feType string, cssHealth bool) error {
	if !dirExists(feDir) {
		fmt.Printf("  ⚠️  %s: directory %s not found, skipping\n", name, feDir)
		return nil
	}

	pkgJSON := filepath.Join(feDir, "package.json")
	if _, err := os.Stat(pkgJSON); err != nil {
		return nil
	}

	if _, err := os.Stat(filepath.Join(feDir, "node_modules")); os.IsNotExist(err) {
		fmt.Printf("  ⚠️  %s: node_modules not found — run 'npm install' in %s\n", name, feDir)
		return nil
	}

	scripts, err := readPackageScripts(pkgJSON)
	if err != nil {
		return fmt.Errorf("%s: read package.json scripts: %w", name, err)
	}

	fmt.Printf("Running frontend linters for %s...\n", name)
	hasFailed := false

	if hasPackageScript(scripts, "lint") {
		if err := runNPMCommand(ctx, feDir, "run", "lint"); err != nil {
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

	if hasPackageScript(scripts, "typecheck") {
		if err := runNPMCommand(ctx, feDir, "run", "typecheck"); err != nil {
			fmt.Fprintf(os.Stderr, "  ❌ %s: npm run typecheck failed: %v\n", name, err)
			hasFailed = true
		} else {
			fmt.Printf("  ✓ %s: typecheck passed\n", name)
		}
	} else if _, err := os.Stat(filepath.Join(feDir, "tsconfig.json")); err == nil {
		fmt.Printf("  ⚠️  %s: no npm typecheck script found; add `typecheck`: `tsc --noEmit`\n", name)
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
