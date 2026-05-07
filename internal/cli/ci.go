package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
)

func newCICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "CI helper commands — verify, scan, and validate in CI pipelines",
	}
	cmd.AddCommand(newCIVerifyGeneratedCmd())
	cmd.AddCommand(newCIValidateKCLCmd())
	cmd.AddCommand(newCIVulnScanCmd())
	cmd.AddCommand(newCIMigrationSafetyCmd())
	return cmd
}

func newCIVerifyGeneratedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify-generated",
		Short: "Verify generated code is up to date",
		Long:  "Runs forge generate and verifies no files changed. Used in CI to catch stale generated code.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Run forge generate.
			parts, err := forgeExecCommand()
			if err != nil {
				return fmt.Errorf("resolve forge binary: %w", err)
			}
			genCmd := exec.Command(parts[0], append(parts[1:], "generate")...)
			genCmd.Stdout = os.Stdout
			genCmd.Stderr = os.Stderr
			if err := genCmd.Run(); err != nil {
				return fmt.Errorf("forge generate failed: %w", err)
			}

			// Check for uncommitted changes.
			diffCmd := exec.Command("git", "diff", "--exit-code")
			diffCmd.Stdout = os.Stdout
			diffCmd.Stderr = os.Stderr
			if err := diffCmd.Run(); err != nil {
				fmt.Fprintln(os.Stderr, "Error: generated code is out of date. Run 'forge generate' and commit the changes.")
				return fmt.Errorf("generated code is out of date")
			}

			fmt.Println("✅ Generated code is up to date.")
			return nil
		},
	}
}

func newCIValidateKCLCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate-kcl",
		Short: "Validate KCL deploy manifests for all environments",
		Long:  "Validates KCL deploy manifests by running kcl on each environment's main.k file defined in forge.yaml.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProjectConfig()
			if err != nil {
				return fmt.Errorf("load project config: %w", err)
			}

			if len(cfg.Envs) == 0 {
				fmt.Println("No environments defined in forge.yaml — nothing to validate.")
				return nil
			}

			hasFailed := false
			for _, env := range cfg.Envs {
				mainK := filepath.Join("deploy", "kcl", env.Name, "main.k")
				fmt.Printf("Validating %s ... ", mainK)

				kclCmd := exec.Command("kcl", "run", mainK)
				kclCmd.Stdout = nil // discard output; we only care about exit code
				kclCmd.Stderr = os.Stderr
				if err := kclCmd.Run(); err != nil {
					fmt.Println("FAIL")
					hasFailed = true
				} else {
					fmt.Println("OK")
				}
			}

			if hasFailed {
				return fmt.Errorf("one or more KCL validations failed")
			}

			fmt.Println("✅ All KCL manifests valid.")
			return nil
		},
	}
}

func newCIMigrationSafetyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migration-safety",
		Short: "Run SQL migration safety checks based on forge.yaml config",
		Long:  "Checks SQL migrations for patterns that pass on empty databases but fail or lock populated databases.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProjectConfig()
			if err != nil {
				return fmt.Errorf("load project config: %w", err)
			}
			if !cfg.Features.MigrationsEnabled() {
				fmt.Println("migrations feature is disabled in forge.yaml")
				return nil
			}

			migrationsDir := cfg.Database.MigrationsDir
			if migrationsDir == "" {
				migrationsDir = filepath.Join("db", "migrations")
			}
			result, err := migrationlint.LintMigrationsDir(migrationsDir, migrationlint.ConfigFromProject(cfg.Database.MigrationSafety))
			if err != nil {
				return fmt.Errorf("migration safety lint failed: %w", err)
			}
			fmt.Print(result.FormatText())
			if result.HasErrors() {
				return fmt.Errorf("migration safety violations found")
			}
			return nil
		},
	}
}

func newCIVulnScanCmd() *cobra.Command {
	var (
		flagGo  bool
		flagNPM bool
		flagAll bool
	)

	cmd := &cobra.Command{
		Use:   "vuln-scan",
		Short: "Run vulnerability scanners based on forge.yaml config",
		Long:  "Runs govulncheck for Go and npm audit for frontends. Defaults to scanning everything enabled in forge.yaml.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProjectConfig()
			if err != nil {
				return fmt.Errorf("load project config: %w", err)
			}

			// If no specific flag set, default to --all behavior.
			if !flagGo && !flagNPM {
				flagAll = true
			}

			// Zero-value VulnScan config means "all enabled" (project convention).
			allEnabled := cfg.CI.VulnScan == (config.CIVulnConfig{})
			runGo := flagGo || (flagAll && (allEnabled || cfg.CI.VulnScan.Go))
			runNPM := flagNPM || (flagAll && (allEnabled || cfg.CI.VulnScan.NPM))

			hasFailed := false

			if runGo {
				if err := ciRunGovulncheck(); err != nil {
					fmt.Fprintf(os.Stderr, "❌ govulncheck failed: %v\n", err)
					hasFailed = true
				}
			}

			if runNPM {
				if err := ciRunNPMAudit(cfg); err != nil {
					fmt.Fprintf(os.Stderr, "❌ npm audit failed: %v\n", err)
					hasFailed = true
				}
			}

			if hasFailed {
				return fmt.Errorf("vulnerability scan found issues")
			}

			fmt.Println("✅ Vulnerability scan passed.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&flagGo, "go", false, "Run govulncheck only")
	cmd.Flags().BoolVar(&flagNPM, "npm", false, "Run npm audit only")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Run all scanners enabled in forge.yaml (default)")

	return cmd
}

func ciRunGovulncheck() error {
	if _, err := exec.LookPath("govulncheck"); err != nil {
		fmt.Println("⚠️  govulncheck not found on PATH — skipping")
		return nil
	}

	fmt.Println("Running govulncheck ./...")
	cmd := exec.Command("govulncheck", "./...")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ciRunNPMAudit(cfg *config.ProjectConfig) error {
	if len(cfg.Frontends) == 0 {
		fmt.Println("No frontends defined — skipping npm audit.")
		return nil
	}

	hasFailed := false
	for _, fe := range cfg.Frontends {
		dir := fe.Path
		fmt.Printf("Running npm audit in %s ...\n", dir)
		cmd := exec.Command("npm", "audit", "--audit-level=high")
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  ❌ npm audit failed for %s: %v\n", fe.Name, err)
			hasFailed = true
		}
	}

	if hasFailed {
		return fmt.Errorf("npm audit found vulnerabilities")
	}
	return nil
}
