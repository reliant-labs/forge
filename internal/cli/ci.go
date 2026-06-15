package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclrender"
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
		Short: "Verify generated code is pristine and up to date",
		Long: "Two checks, both local to the checkout:\n" +
			"  1. Self-certification: every generated file's embedded forge:hash marker\n" +
			"     must verify (recompute vs embedded) — catches hand-edits that were\n" +
			"     committed without --force / forge disown.\n" +
			"  2. Freshness: runs forge generate and verifies no files changed —\n" +
			"     catches stale generated code after an input (proto/forge.yaml) change.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := requireFeature(config.FeatureCI); err != nil {
				return err
			}

			// Pass 1: recompute embedded hashes. A hand-edited generated
			// file fails HERE, by name, before the regenerate pass would
			// abort on the same files with a less CI-shaped message.
			root, err := projectRoot()
			if err != nil {
				return err
			}
			cs, err := generator.LoadChecksums(root)
			if err != nil {
				return fmt.Errorf("load .forge ownership state: %w", err)
			}
			if drift := scanProjectDrift(root, cs); len(drift) > 0 {
				fmt.Fprintf(os.Stderr, "Error: %d generated file(s) were hand-edited after forge wrote them:\n", len(drift))
				for _, d := range drift {
					fmt.Fprintf(os.Stderr, "  - %s\n", d.Path)
				}
				fmt.Fprintln(os.Stderr, "Move the edits to a user-owned extension point (then regenerate), or `forge disown <path> --reason \"<why>\"` to take ownership.")
				return fmt.Errorf("generated files failed self-certification")
			}

			// Pass 2: regenerate and diff.
			parts, err := forgeExecCommand()
			if err != nil {
				return fmt.Errorf("resolve forge binary: %w", err)
			}
			genCmd := exec.CommandContext(cmd.Context(), parts[0], append(parts[1:], "generate")...)
			genCmd.Stdout = os.Stdout
			genCmd.Stderr = os.Stderr
			if err := genCmd.Run(); err != nil {
				return fmt.Errorf("forge generate failed: %w", err)
			}

			// Check for uncommitted changes.
			diffCmd := exec.CommandContext(cmd.Context(), "git", "diff", "--exit-code")
			diffCmd.Stdout = os.Stdout
			diffCmd.Stderr = os.Stderr
			if err := diffCmd.Run(); err != nil {
				_, _ = fmt.Fprintln(os.Stderr, "Error: generated code is out of date. Run 'forge generate' and commit the changes.")
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
			if _, err := requireFeature(config.FeatureCI); err != nil {
				return err
			}

			// Source of truth for the env list is the filesystem
			// (deploy/kcl/<env>/main.k presence).
			projectDir := projectDirForKCL()
			envs, lerr := ListEnvs(projectDir)
			if lerr != nil {
				return fmt.Errorf("list envs: %w", lerr)
			}
			if len(envs) == 0 {
				fmt.Println("No environments declared (no deploy/kcl/<env>/main.k) — nothing to validate.")
				return nil
			}

			hasFailed := false
			for _, env := range envs {
				mainK := filepath.Join("deploy", "kcl", env, "main.k")
				fmt.Printf("Validating %s ... ", mainK)

				// Validate by rendering through the embedded runtime (no
				// external `kcl` binary); we only care about success/failure.
				wd, _ := os.Getwd()
				if _, err := kclrender.Run(wd, mainK, nil); err != nil {
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
			store, err := loadProjectStore()
			if err != nil {
				return fmt.Errorf("load project config: %w", err)
			}
			if !store.Features().CIEnabled() {
				return config.DisabledFeatureError(config.FeatureCI)
			}
			if !store.Features().MigrationsEnabled() {
				return config.DisabledFeatureError(config.FeatureMigrations)
			}

			migrationsDir := store.Database().MigrationsDir
			if migrationsDir == "" {
				migrationsDir = filepath.Join("db", "migrations")
			}
			result, err := migrationlint.LintMigrationsDir(migrationsDir, migrationlint.ConfigFromProject(store.Database().MigrationSafety))
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
			store, err := requireFeature(config.FeatureCI)
			if err != nil {
				return err
			}
			ci := store.CI()

			// If no specific flag set, default to --all behavior.
			if !flagGo && !flagNPM {
				flagAll = true
			}

			// Zero-value VulnScan config means "all enabled" (project convention).
			allEnabled := ci.VulnScan == (config.CIVulnConfig{})
			runGo := flagGo || (flagAll && (allEnabled || ci.VulnScan.Go))
			runNPM := flagNPM || (flagAll && (allEnabled || ci.VulnScan.NPM))

			hasFailed := false

			if runGo {
				if err := ciRunGovulncheck(cmd.Context()); err != nil {
					fmt.Fprintf(os.Stderr, "❌ govulncheck failed: %v\n", err)
					hasFailed = true
				}
			}

			if runNPM {
				if err := ciRunNPMAudit(cmd.Context(), store.Config()); err != nil {
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

func ciRunGovulncheck(ctx context.Context) error {
	if _, err := exec.LookPath("govulncheck"); err != nil {
		fmt.Println("⚠️  govulncheck not found on PATH — skipping")
		return nil
	}

	fmt.Println("Running govulncheck ./...")
	cmd := exec.CommandContext(ctx, "govulncheck", "./...")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ciRunNPMAudit(ctx context.Context, cfg *config.ProjectConfig) error {
	if len(cfg.Frontends) == 0 {
		fmt.Println("No frontends defined — skipping npm audit.")
		return nil
	}

	hasFailed := false
	for _, fe := range cfg.Frontends {
		dir := fe.Path
		fmt.Printf("Running npm audit in %s ...\n", dir)
		cmd := exec.CommandContext(ctx, "npm", "audit", "--audit-level=high")
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
