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

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/dblint"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
)

// lintFlags holds the flag values for the lint command.
type lintFlags struct {
	contract        bool
	db              bool
	migrationSafety bool
	fix             bool
	exportedVars    bool
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

Examples:
  forge lint                    # Run all standard linters
  forge lint --contract         # Run contract interface enforcement linter
  forge lint --db               # Run DB entity lint rules
  forge lint --migration-safety  # Run SQL migration safety checks
  forge lint --exported-vars     # Run exported vars linter
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
	cmd.Flags().BoolVar(&flags.fix, "fix", false, "Automatically fix issues where possible")

	return cmd
}

func runLint(flags lintFlags, paths []string) error {
	// When a specific flag is set, run only that linter (preserving current behavior).
	if flags.contract {
		cfg, err := loadProjectConfig()
		if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
			return fmt.Errorf("failed to load project config: %w", err)
		}
		if cfg != nil && !cfg.Features.ContractsEnabled() {
			fmt.Println("contracts feature is disabled in forge.yaml")
			return nil
		}
		return runContractLinter(paths)
	}
	if flags.exportedVars {
		return runContractLinter(paths)
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

func runContractLinter(paths []string) error {
	fmt.Println("🔍 Running contract interface enforcement linter...")
	fmt.Println()

	binPath, err := resolveContractLintBinary()
	if err != nil {
		return err
	}

	var lintExec *exec.Cmd
	if strings.HasSuffix(binPath, "main.go") {
		args := []string{"run", binPath}
		args = append(args, paths...)
		lintExec = exec.Command("go", args...)
		fmt.Printf("Running: go %s\n", strings.Join(args, " "))
	} else {
		lintExec = exec.Command(binPath, paths...)
		fmt.Printf("Running: %s %s\n", binPath, strings.Join(paths, " "))
	}

	// Inherit environment and set flags needed for analysis to resolve modules.
	// GOWORK=off prevents workspace-mode confusion in the analyzer.
	// GOFLAGS=-mod=mod allows the analyzer to fetch missing modules.
	lintExec.Env = appendEnvIfUnset(os.Environ(), "GOWORK", "off")
	lintExec.Env = appendEnvIfUnset(lintExec.Env, "GOFLAGS", "-mod=mod")
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
				return fmt.Errorf("linting failed")
			}
		}
		return fmt.Errorf("failed to run contract linter: %w", err)
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
		return fmt.Errorf("migration safety violations found")
	}
	return nil
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
	} else if err := runContractLinter(paths); err != nil {
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

	if hasFailed {
		return fmt.Errorf("linting failed")
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
