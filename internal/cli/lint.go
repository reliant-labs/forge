package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/dblint"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
	"github.com/reliant-labs/forge/internal/linter/frontendpacklint"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
	"github.com/reliant-labs/forge/internal/linter/scaffolds"
)

// lintFlags holds the flag values for the lint command.
type lintFlags struct {
	contract         bool
	db               bool
	migrationSafety  bool
	fix              bool
	exportedVars     bool
	conventions      bool
	frontendPacks    bool
	scaffolds        bool
	tests            bool
	banners          bool
	suggestExcludes  bool
	wireCoverage     bool
	checkWorkarounds bool
}

func newLintCmd() *cobra.Command {
	var flags lintFlags

	cmd := &cobra.Command{
		Use:   "lint [paths...]",
		Short: "Run linters on the project",
		Long: `Run various linters on the Forge project.

This command will:
- Run standard Go linters (golangci-lint)
- Run proto linters (buf lint)
- Run TypeScript linters for Next.js frontends (if frontends/ exists)
- Optionally run contract interface enforcement linter (--contract)
- Optionally run DB entity lint rules (--db)
- Optionally run SQL migration safety checks (--migration-safety)
- Optionally run forge convention rules (--conventions)
- Optionally run frontend pack convention rules (--frontend-packs)
- Optionally run scaffold ownership rules (--scaffolds)
- Optionally run handler-test convention rules (--tests)
- Optionally run lifecycle-banner rules on forge templates (--banners)

Examples:
  forge lint                    # Run all standard linters
  forge lint --contract         # Run contract interface enforcement linter
  forge lint --db               # Run DB entity lint rules
  forge lint --migration-safety  # Run SQL migration safety checks
  forge lint --exported-vars     # Run exported vars linter
  forge lint --conventions       # Run forge convention rules on proto files
  forge lint --frontend-packs   # Flag third-party UI imports in frontend pack templates
  forge lint --scaffolds        # Flag committed FORGE_SCAFFOLD markers and
                                # _gen files missing the canonical header
  forge lint --tests            # Nudge handler tests toward tdd.RunRPCCases
                                # (warnings only — see migration/v0.x-to-tdd-rpccases)
  forge lint --banners          # Verify every forge template carries the
                                # right Tier-1 / Tier-2 lifecycle banner
                                # (warnings only — runs only inside the forge repo)
  forge lint --suggest-excludes # Print a YAML snippet of internal packages that look
                                # like good candidates for contracts.exclude in forge.yaml
                                # (analyzer-style, embed-only, etc.)
  forge lint --wire-coverage    # Report unresolved Deps fields in pkg/app/wire_gen.go
                                # (warnings) AND unresolved forge:placeholder annotations
                                # in pkg/app/app_extras.go (errors — gate the build)
  forge lint --check-workarounds # Flag cross-lane workarounds (cast<X>Repo helpers in
                                # pkg/app/wire_gen.go, hand-rolled pkg/app/testing_extras.go,
                                # cmd/<name>.go files not declared in forge.yaml binaries:)
  forge lint --fix              # Auto-fix issues where possible`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var paths []string
			if len(args) > 0 {
				paths = args
			} else {
				paths = []string{"./..."}
			}

			return runLint(flags, paths)
		},
	}

	cmd.Flags().BoolVar(&flags.contract, "contract", false, "Run contract interface enforcement linter")
	cmd.Flags().BoolVar(&flags.exportedVars, "exported-vars", false, "Run exported vars linter")
	cmd.Flags().BoolVar(&flags.db, "db", false, "Run DB entity lint rules on proto/db/ files")
	cmd.Flags().BoolVar(&flags.migrationSafety, "migration-safety", false, "Run SQL migration safety checks")
	cmd.Flags().BoolVar(&flags.conventions, "conventions", false, "Run forge convention rules on proto files")
	cmd.Flags().BoolVar(&flags.frontendPacks, "frontend-packs", false, "Flag third-party UI imports in frontend pack templates (warnings only)")
	cmd.Flags().BoolVar(&flags.scaffolds, "scaffolds", false, "Enforce forge ownership boundary (FORGE_SCAFFOLD markers + _gen file headers)")
	cmd.Flags().BoolVar(&flags.tests, "tests", false, "Run handler-test convention rules (e.g. forgeconv-handler-tests-use-tdd; warnings only)")
	cmd.Flags().BoolVar(&flags.banners, "banners", false, "Verify forge templates carry the right Tier-1 / Tier-2 lifecycle banner (warnings only; no-op outside the forge repo)")
	cmd.Flags().BoolVar(&flags.suggestExcludes, "suggest-excludes", false, "Print a YAML snippet of contracts.exclude candidates (heuristic-based; nothing is mutated)")
	cmd.Flags().BoolVar(&flags.wireCoverage, "wire-coverage", false, "Report unresolved Deps fields in pkg/app/wire_gen.go (warnings) and unresolved forge:placeholder annotations in pkg/app/app_extras.go (errors)")
	cmd.Flags().BoolVar(&flags.checkWorkarounds, "check-workarounds", false, "Flag cross-lane workarounds (cast<X>Repo helpers, testing_extras.go, undeclared cmd/<name>.go) — warnings only")
	cmd.Flags().BoolVar(&flags.fix, "fix", false, "Automatically fix issues where possible")

	return cmd
}

func runLint(flags lintFlags, paths []string) error {
	// When a specific flag is set, run only that linter (preserving current behavior).
	if flags.suggestExcludes {
		cfg, err := loadProjectConfig()
		if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config: %w", err)
		}
		return runSuggestExcludes(cfg)
	}
	if flags.contract {
		cfg, err := loadProjectConfig()
		if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config: %w", err)
		}
		if cfg != nil && !cfg.Features.ContractsEnabled() {
			fmt.Println("contracts feature is disabled in forge.yaml")
			return nil
		}
		return runContractLinter(paths, contractExcludesFromConfig(cfg))
	}
	if flags.exportedVars {
		cfg, err := loadProjectConfig()
		if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config: %w", err)
		}
		return runContractLinter(paths, contractExcludesFromConfig(cfg))
	}
	if flags.db {
		cfg, err := loadProjectConfig()
		if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config: %w", err)
		}
		if cfg != nil && !cfg.Features.ORMEnabled() {
			fmt.Println("orm feature is disabled in forge.yaml")
			return nil
		}
		return runDBLint()
	}
	if flags.migrationSafety {
		cfg, err := loadProjectConfig()
		if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config: %w", err)
		}
		if cfg != nil && !cfg.Features.MigrationsEnabled() {
			fmt.Println("migrations feature is disabled in forge.yaml")
			return nil
		}
		return runMigrationSafetyLint(cfg)
	}
	if flags.conventions {
		return runConventionLint()
	}
	if flags.frontendPacks {
		return runFrontendPackLint()
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
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		return runWireCoverageLint(cwd)
	}
	if flags.checkWorkarounds {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		return runCheckWorkaroundsLint(cwd)
	}

	// Load project config for lint defaults. A missing config file is fine
	// (we fall back to defaults), but a parse/read error should fail hard so
	// users don't silently lint with the wrong configuration.
	cfg, err := loadProjectConfig()
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		return fmt.Errorf("failed to load project config: %w", err)
	}

	// No flags set — run ALL linters, each skipping gracefully if tool not available.
	return runAllLinters(flags.fix, paths, cfg)
}

// contractExcludesFromConfig returns the contracts.exclude list from the
// project config, or nil when no config is loaded.
func contractExcludesFromConfig(cfg *config.ProjectConfig) []string {
	if cfg == nil {
		return nil
	}
	return cfg.Contracts.Exclude
}

func runContractLinter(paths []string, excludes []string) error {
	fmt.Println("🔍 Running contract interface enforcement linter...")
	fmt.Println()

	binPath, err := resolveContractLintBinary()
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
		lintExec = exec.Command("go", goArgs...)
		fmt.Printf("Running: go %s\n", strings.Join(goArgs, " "))
	} else {
		lintExec = exec.Command(binPath, lintArgs...)
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

func resolveContractLintBinary() (string, error) {
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
	if err := os.MkdirAll("bin", 0755); err != nil {
		return srcPath, nil
	}

	buildCmd := exec.Command("go", "build", "-o", localBin, "./cmd/contractlint")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		fmt.Printf("Warning: failed to build binary, falling back to go run: %v\n", err)
		return srcPath, nil
	}

	fmt.Printf("Built %s\n", localBin)
	return localBin, nil
}

// runDBLint runs advisory lint rules on proto/db/ entity definitions.
// Findings are printed as warnings and never cause a non-zero exit.
func runDBLint() error {
	fmt.Println("🔍 Running DB entity lint rules...")
	fmt.Println()

	protoDBDir := filepath.Join("proto", "db")
	if _, err := os.Stat(protoDBDir); os.IsNotExist(err) {
		fmt.Println("⚠️  No proto/db/ directory found — skipping DB lint")
		return nil
	}

	result, err := dblint.LintProtoDir(protoDBDir)
	if err != nil {
		return fmt.Errorf("DB lint failed: %w", err)
	}

	fmt.Print(result.FormatText())

	if !result.HasWarnings() {
		fmt.Println("✅ No DB lint warnings!")
	}
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
func runConventionLint() error {
	fmt.Println("Running forge convention rules...")

	combined := forgeconv.Result{}
	hasProto := false

	if _, err := os.Stat("proto"); err == nil {
		hasProto = true
		res, err := forgeconv.LintProtoTree("proto")
		if err != nil {
			return fmt.Errorf("forge convention lint (proto) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)
	} else {
		fmt.Println("  ⚠️  No proto/ directory found — skipping proto convention lint")
	}

	// Internal-package contract shape — runs whether or not proto/ exists,
	// because CLI/library projects without proto can still ship internal/
	// packages whose bootstrap codegen would silently break on a wrong-named
	// contract.
	hasInternal := false
	if _, err := os.Stat("internal"); err == nil {
		hasInternal = true
		cfg, cfgErr := loadProjectConfig()
		if cfgErr != nil && !errors.Is(cfgErr, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config for contract-name lint: %w", cfgErr)
		}
		excludes := contractExcludesFromConfig(cfg)
		res, err := forgeconv.LintInternalContracts(".", excludes)
		if err != nil {
			return fmt.Errorf("forge convention lint (contracts) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)

		// Hexagonal-architecture conventions for `--type=adapter` and
		// `--type=interactor` packages. Both rules are warnings (the
		// false-positive risk is real for projects that opt out of the
		// marker convention), but they catch the canonical foot-guns
		// the patterns are designed to prevent. See `forge skill load
		// adapter` and `forge skill load interactor`.
		adapterRes, err := forgeconv.LintAdapterNoRPC(".")
		if err != nil {
			return fmt.Errorf("forge convention lint (adapter-no-rpc) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, adapterRes.Findings...)

		interactorRes, err := forgeconv.LintInteractorDepsAreInterfaces(".")
		if err != nil {
			return fmt.Errorf("forge convention lint (interactor-deps-are-interfaces) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, interactorRes.Findings...)
	}

	// Handler-tree analyzers — only run when handlers/ exists. The
	// no-handler-error-mapping rule catches per-service `toConnectError`
	// / `mapServiceError` re-rolls; canonical replacement is
	// `svcerr.Wrap(err)` from forge/pkg/svcerr. Warnings only — false-
	// positive risk is real (some projects have legitimate custom mapping
	// for project-specific sentinels), and the build should not gate on a
	// hand-rolled helper that hasn't been migrated yet.
	hasHandlers := false
	if _, err := os.Stat("handlers"); err == nil {
		hasHandlers = true
		res, err := forgeconv.LintHandlerErrorMapping(".")
		if err != nil {
			return fmt.Errorf("forge convention lint (handler error mapping) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)
	}

	// Component-tree analyzers — also run on workers/ and operators/
	// (which have the same Deps-struct shape as handlers/). The
	// optional-dep-marker-position rule catches misplaced
	// `// forge:optional-dep` markers (typo'd onto a struct rather
	// than a field, or onto a non-Deps type) — the marker has no
	// effect when it's not on a Deps field, and silent failure is
	// exactly the kind of bug a lint rule earns its keep on.
	hasComponentTree := hasHandlers
	for _, sub := range []string{"workers", "operators"} {
		if _, err := os.Stat(sub); err == nil {
			hasComponentTree = true
		}
	}
	if hasComponentTree {
		res, err := forgeconv.LintOptionalDepMarkerPosition(".")
		if err != nil {
			return fmt.Errorf("forge convention lint (optional-dep marker position) failed: %w", err)
		}
		combined.Findings = append(combined.Findings, res.Findings...)
	}

	// If none of proto/, internal/, handlers/, workers/, or operators/
	// exist, there's nothing to lint.
	if !hasProto && !hasInternal && !hasComponentTree {
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

// runTestsLint runs handler-test convention rules. Currently:
//
//   - forgeconv-handler-tests-use-tdd: warns when a handler test file
//     hand-rolls the `tests := []struct{name, call}` shape instead of
//     `tdd.RunRPCCases`. See `forge skill load
//     migration/v0.x-to-tdd-rpccases` for the conversion playbook, or
//     run `forge test migrate-tdd` to convert most files automatically.
//
// All findings are warnings — never gates the build. The lint exists to
// surface drift, not block legitimate pre-`tdd.RunRPCCases` projects.
func runTestsLint() error {
	fmt.Println("🔍 Running handler-test convention lint...")
	fmt.Println()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	res, err := forgeconv.LintHandlerTests(cwd)
	if err != nil {
		return fmt.Errorf("handler-test lint failed: %w", err)
	}
	if len(res.Findings) == 0 {
		fmt.Println("✓ handler-test conventions passed")
		return nil
	}
	fmt.Print(res.FormatText())
	// Warnings only — never gate.
	fmt.Println("(warnings only — not failing the build)")
	return nil
}

// runBannersLint verifies forge's own template files carry the
// lifecycle banner that matches their tier:
//
//   - Tier 1 (regenerated every run): "// Code generated by forge ... DO NOT EDIT."
//   - Tier 2 (one-shot scaffold): "// forge:scaffold one-shot"
//   - Tier 3 (user-owned skeleton): banner-less by design.
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

// runAllLinters runs every linter, each skipping gracefully if the required tool isn't installed.
func runAllLinters(fix bool, paths []string, cfg *config.ProjectConfig) error {
	fmt.Println("🔍 Running all linters...")
	fmt.Println()

	hasFailed := false

	// 1. Standard Go linters (golangci-lint)
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		fmt.Println("⚠️  golangci-lint not found on PATH — skipping")
	} else if err := runGolangciLint(fix, paths); err != nil {
		fmt.Fprintf(os.Stderr, "❌ golangci-lint failed: %v\n", err)
		hasFailed = true
	}

	// 2. Contract interface enforcement
	if cfg != nil && !cfg.Features.ContractsEnabled() {
		fmt.Println("⚠️  contracts feature disabled — skipping contract linter")
	} else if _, err := resolveContractLintBinary(); err != nil {
		fmt.Println("⚠️  contractlint not available — skipping")
	} else if err := runContractLinter(paths, contractExcludesFromConfig(cfg)); err != nil {
		fmt.Fprintf(os.Stderr, "❌ contract linter failed: %v\n", err)
		hasFailed = true
	}

	// 4. Buf lint
	if _, err := exec.LookPath("buf"); err != nil {
		fmt.Println("⚠️  buf not found on PATH — skipping buf lint")
	} else if err := runBufLint(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ buf lint failed: %v\n", err)
		hasFailed = true
	}

	// 5. Frontend linters (tsc + eslint)
	if err := runFrontendLinters(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Frontend lint failed: %v\n", err)
		hasFailed = true
	}

	// 6. DB entity lint (advisory — warnings only, does not fail the build)
	if cfg != nil && !cfg.Features.ORMEnabled() {
		fmt.Println("⚠️  orm feature disabled — skipping DB lint")
	} else if dirExists("proto/db") {
		if err := runDBLint(); err != nil {
			// DB lint errors are non-fatal; they print warnings but don't block.
			fmt.Fprintf(os.Stderr, "⚠️  DB lint: %v\n", err)
		}
	}

	// 7. SQL migration safety lint
	if cfg != nil && !cfg.Features.MigrationsEnabled() {
		fmt.Println("⚠️  migrations feature disabled — skipping migration safety lint")
	} else if err := runMigrationSafetyLint(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Migration safety lint failed: %v\n", err)
		hasFailed = true
	}

	// 8. Forge convention rules (proto + internal-package contracts).
	// Errors gate the build (a missing pk: true would silently produce
	// broken codegen; a non-canonical contract.go would produce a
	// bootstrap that references types that don't exist); warnings are
	// surfaced but tolerated.
	if dirExists("proto") || dirExists("internal") {
		if err := runConventionLint(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Forge convention lint failed: %v\n", err)
			hasFailed = true
		}
	}

	// 9. Frontend pack convention rules — only meaningful when running
	// inside the forge repo itself (where pack sources live). Warnings
	// only; never gates the build.
	if dirExists(filepath.Join("internal", "packs")) {
		if err := runFrontendPackLint(); err != nil {
			// Soft rule, but report unexpected failures (I/O errors, etc).
			fmt.Fprintf(os.Stderr, "⚠️  Frontend pack lint: %v\n", err)
		}
	}

	// 10. Scaffold ownership lint — flags committed FORGE_SCAFFOLD markers
	// and _gen files without the canonical forge header. Errors gate the
	// build; warnings (missing "Source:" line) print but tolerate.
	if err := runScaffoldsLint(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Scaffold ownership lint failed: %v\n", err)
		hasFailed = true
	}

	// 11. Handler-test convention lint — warns on hand-rolled
	// `tests := []struct{name, call}` table tests under handlers/*/.
	// Warnings only; never gates the build (legacy projects pre-date the
	// scaffolded `tdd.RunRPCCases` shape and may not have migrated yet).
	if dirExists("handlers") {
		if err := runTestsLint(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Handler-test lint: %v\n", err)
		}
	}

	// 11b. Lifecycle-banner lint — only meaningful inside the forge
	// repo itself (where template sources live). Warnings only; the
	// helper short-circuits to a no-op when no template tree is present.
	if dirExists(filepath.Join("internal", "templates")) ||
		dirExists(filepath.Join("internal", "packs")) {
		if err := runBannersLint(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Banner lint: %v\n", err)
		}
	}

	// 12. Proto-vs-ORM staleness — warns when gen/db/v1/*.pb.go has no
	// matching .pb.orm.go sibling or when the .pb.go is newer than every
	// sibling. This catches the "ran buf generate alone" pitfall (see
	// the proto skill for the full rationale). Warnings only; never
	// gates the build.
	if dirExists(filepath.Join("gen", "db", "v1")) {
		if err := runORMSyncLint("."); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  proto-orm sync lint: %v\n", err)
		}
	}

	// 13. Wire-coverage — surfaces unresolved Deps fields in
	// pkg/app/wire_gen.go and unresolved `forge:placeholder` markers
	// in pkg/app/app_extras.go. TODO findings are warnings (active
	// development), placeholder findings are errors (the user
	// explicitly promised tightening). The lint runs even when
	// wire_gen.go is missing — placeholder errors fire purely off
	// app_extras.go so a forge generate refused-to-write state still
	// gets diagnosed.
	if fileExists(filepath.Join("pkg", "app", "wire_gen.go")) ||
		fileExists(filepath.Join("pkg", "app", "app_extras.go")) {
		cwd, err := os.Getwd()
		if err == nil {
			if err := runWireCoverageLint(cwd); err != nil {
				fmt.Fprintf(os.Stderr, "❌ wire-coverage lint: %v\n", err)
				hasFailed = true
			}
		}
	}

	// 14. Check-workarounds — flags the canonical cross-lane workarounds
	// (FORGE_REVIEW_PROCESS.md §2): cast<X>Repo helpers in wire_gen.go,
	// pkg/app/testing_extras.go hand-rolled stubs, cmd/<name>.go files
	// not declared in forge.yaml's binaries: block. Warnings only —
	// these can be legitimate in some projects, but each has a canonical
	// forge-primitive replacement either already-landed or in flight.
	{
		cwd, err := os.Getwd()
		if err == nil {
			if err := runCheckWorkaroundsLint(cwd); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  check-workarounds lint: %v\n", err)
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

func runGolangciLint(fix bool, paths []string) error {
	fmt.Println("Running golangci-lint...")

	args := []string{"run"}
	if fix {
		args = append(args, "--fix")
	}
	args = append(args, paths...)

	cmd := exec.Command("golangci-lint", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	fmt.Println("✓ golangci-lint passed")
	return nil
}

func runBufLint() error {
	if _, err := os.Stat("buf.yaml"); os.IsNotExist(err) {
		return nil
	}

	fmt.Println("Running buf lint...")

	cmd := exec.Command("buf", "lint")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	fmt.Println("✓ buf lint passed")
	return nil
}

// runFrontendLinters runs TypeScript type-checking and framework-specific linters
// for each frontend defined in the project config. Falls back to directory scanning
// if no config is available.
func runFrontendLinters(cfg *config.ProjectConfig) error {
	if cfg != nil && len(cfg.Frontends) > 0 {
		return runFrontendLintersFromConfig(cfg)
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
		if err := lintFrontendDir(e.Name(), feDir, "", false); err != nil {
			hasFailed = true
		}
	}

	if hasFailed {
		return fmt.Errorf("frontend linting failed")
	}
	return nil
}

// runFrontendLintersFromConfig lints frontends using project config entries.
func runFrontendLintersFromConfig(cfg *config.ProjectConfig) error {
	hasFailed := false
	for _, fe := range cfg.Frontends {
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		if err := lintFrontendDir(fe.Name, feDir, fe.Type, cfg.Lint.Frontend.CSSHealth); err != nil {
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
func lintFrontendDir(name, feDir, feType string, cssHealth bool) error {
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
		if err := runNPMCommand(feDir, "run", "lint"); err != nil {
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
		if err := runNPMCommand(feDir, "run", "typecheck"); err != nil {
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
			if err := runNPMCommand(feDir, "run", "lint:styles"); err != nil {
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
func runNPMCommand(dir string, args ...string) error {
	cmd := exec.Command("npm", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
