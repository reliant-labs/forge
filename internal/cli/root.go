package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	version   string
	buildDate string // Set via ldflags at build time
	gitCommit string // Set via ldflags at build time
)

func Execute() error {
	return NewRootCmd().Execute()
}

// CLIName returns the command name users should type to invoke Forge.
// When the binary is "forge" (standalone install), it returns "forge".
// When embedded in another binary (e.g. "reliant"), it returns "reliant forge".
func CLIName() string {
	return strings.Join(forgeCommand(), " ")
}

// forgeCommand returns the command tokens needed to invoke Forge.
// Standalone: ["forge"]. Embedded: ["reliant", "forge"].
// The first element is always the resolved executable path when called
// via forgeExecCommand, or the base name for display purposes here.
func forgeCommand() []string {
	base := filepath.Base(os.Args[0])
	if base == "forge" {
		return []string{"forge"}
	}
	return []string{base, "forge"}
}

// forgeExecCommand returns exec-ready command tokens using the resolved
// executable path. Use this when spawning forge as a subprocess.
func forgeExecCommand() ([]string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	if filepath.Base(exePath) == "forge" {
		return []string{exePath}, nil
	}
	return []string{exePath, "forge"}, nil
}

func SetVersion(v, date, commit string) {
	version = v
	buildDate = date
	gitCommit = commit
}

// GetVersion returns the forge binary's version string. Callers can use this
// to stamp the current forge version into generated artifacts (e.g. the
// .forge/checksums.json header), which enables pinned installs in CI.
func GetVersion() string {
	return version
}

// GetGitCommit returns the git commit SHA the forge binary was built from.
// Returns "unknown" if the binary was not built with ldflags.
func GetGitCommit() string {
	return gitCommit
}

// NewRootCmd builds and returns the fully assembled root command.
func NewRootCmd() *cobra.Command {
	var verbose bool

	rootCmd := &cobra.Command{
		Use:   "forge",
		Short: "Connect RPC development framework for LLM-optimized applications",
		Long: `Forge is a development framework where everything communicates via
Connect RPC interfaces, purpose-built for LLM-driven development.

It enables easy mocking, middleware injection, spec-driven development,
and component swapping - all while maintaining a single, consistent
interface pattern throughout the entire stack.`,
		Version: fmt.Sprintf("%s (built %s, commit %s)", version, buildDate, gitCommit),
	}

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")

	// Add all commands
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newGenerateCmd())
	rootCmd.AddCommand(newDBCmd())
	rootCmd.AddCommand(newNewCmd())
	rootCmd.AddCommand(newAddCmd())
	rootCmd.AddCommand(newBuildCmd())
	rootCmd.AddCommand(newDeployCmd())
	rootCmd.AddCommand(newTestCmd())
	rootCmd.AddCommand(newLintCmd())
	rootCmd.AddCommand(newPackageCmd())
	rootCmd.AddCommand(newPackCmd())
	rootCmd.AddCommand(newStarterCmd())
	rootCmd.AddCommand(newDebugCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newDocsCmd())
	rootCmd.AddCommand(newUpgradeCmd())
	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newProtocGenForgeCmd())
	rootCmd.AddCommand(newComponentCmd())
	rootCmd.AddCommand(newSkillCmd())
	rootCmd.AddCommand(newCICmd())
	rootCmd.AddCommand(newToolsCmd())
	rootCmd.AddCommand(newBacklogCmd())
	rootCmd.AddCommand(newAuditCmd())
	rootCmd.AddCommand(newMapCmd())
	rootCmd.AddCommand(newConfigCmd())

	return rootCmd
}

// newVersionCmd creates the version subcommand so both `forge version` and
// `forge --version` work. Cobra's built-in --version flag handles the latter;
// this covers users who type the subcommand form.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the forge version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s version %s (built %s, commit %s)\n", CLIName(), version, buildDate, gitCommit)
		},
	}
}

// newRunCmd creates the run command.
func newRunCmd() *cobra.Command {
	var opts runOptions
	var serviceFlag string

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run a Forge project with hot reload",
		Long: `Run a Forge project with the specified environment configuration.

This starts all infrastructure (via docker compose), Go services (via Air),
and Next.js apps (via npm run dev), with color-coded log output.

Press Ctrl+C to stop all processes.

Examples:
  forge run                        # Run with default dev configuration
  forge run --env=staging          # Run with staging environment
  forge run --no-infra             # Skip docker-compose infrastructure
  forge run --service api-gateway  # Run only a specific service`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if serviceFlag != "" {
				opts.services = []string{serviceFlag}
			}
			return runProjectDev(opts)
		},
	}

	runCmd.Flags().StringVar(&opts.env, "env", "dev", "Environment to run (dev, staging, prod)")
	runCmd.Flags().BoolVar(&opts.noInfra, "no-infra", false, "Skip starting infrastructure via docker-compose")
	runCmd.Flags().StringVar(&serviceFlag, "service", "", "Run only a specific service or app by name")
	runCmd.Flags().BoolVar(&opts.debug, "debug", false, "Start with Delve debugger (hot-reload + debug on :2345)")

	return runCmd
}