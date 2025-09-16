package cli

import (
	"fmt"

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

func SetVersion(v, date, commit string) {
	version = v
	buildDate = date
	gitCommit = commit
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
	rootCmd.AddCommand(newDebugCmd())
	rootCmd.AddCommand(newDocsCmd())

	return rootCmd
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