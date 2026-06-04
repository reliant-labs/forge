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

// Execute is the entrypoint used by main() to dispatch the assembled
// root cobra command. Wraps NewRootCmd().Execute().
func Execute() error {
	return NewRootCmd().Execute()
}

// Name returns the command name users should type to invoke Forge.
// When the binary is "forge" (standalone install), it returns "forge".
// When embedded in another binary (e.g. "reliant"), it returns "reliant forge".
func Name() string {
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

// SetVersion stamps the version/date/commit metadata used by the
// `version` subcommand and the rendered Cobra Version string. Called
// once from main() with ldflags-injected values.
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
	rootCmd.AddCommand(newMigrateCmd())
	rootCmd.AddCommand(newNewCmd())
	rootCmd.AddCommand(newAddCmd())
	rootCmd.AddCommand(newBuildCmd())
	rootCmd.AddCommand(newDeployCmd())
	rootCmd.AddCommand(newTestCmd())
	rootCmd.AddCommand(newLintCmd())
	rootCmd.AddCommand(newFmtCmd())
	rootCmd.AddCommand(newPackageCmd())
	rootCmd.AddCommand(newPackCmd())
	rootCmd.AddCommand(newStarterCmd())
	rootCmd.AddCommand(newDebugCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newDocsCmd())
	rootCmd.AddCommand(newUpgradeCmd())
	rootCmd.AddCommand(newUnforkCmd())
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
	rootCmd.AddCommand(newDevCmd())
	rootCmd.AddCommand(newAPICmd())

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
			fmt.Printf("%s version %s (built %s, commit %s)\n", Name(), version, buildDate, gitCommit)
		},
	}
}

// newRunCmd creates the run command.
//
// Three usage shapes share one cobra command:
//
//  1. `forge run` — orchestrator: starts every service + frontend +
//     docker compose infra. The pre-host/cluster-split default.
//  2. `forge run <service>` — host-mode single-service runner: execs
//     `go run ./cmd server <service>` with `.env.<env>` loaded onto
//     the child environment. PID is tracked under
//     `~/.cache/forge/run/<service>.pid` for `--background`.
//  3. `forge run <service> stop` — kills the background process for
//     <service>. No-op when nothing is tracked.
//
// The host-mode runner is the inner loop for services declared
// `deploy = "host"` in `deploy/kcl/<env>/`: faster iteration than
// building+pushing a docker image and waiting on a cluster rollout.
// Mirrors the `forge dev port-forward --background` PID-tracking pattern.
func newRunCmd() *cobra.Command {
	var opts runOptions
	var serviceFlag string
	var background bool
	var envFile string

	runCmd := &cobra.Command{
		Use:   "run [service]",
		Short: "Run a Forge project with hot reload, or a single host-mode service",
		Long: `Run a Forge project with the specified environment configuration.

With no positional arg, runs the orchestrator: docker compose infra, every
Go service (via Air), and every Next.js app, with color-coded log output.

With a positional service name, runs that single service as a host process —
the inner loop for services declared deploy = "host" in deploy/kcl/<env>/.
Loads .env.<env> (default .env.dev) onto the child env so DATABASE_URL and
friends come from the local file rather than the cluster's Secret.

Press Ctrl+C to stop. With --background, the process detaches and PID is
tracked under ~/.cache/forge/run/<service>.pid for ` + "`forge run <service> stop`" + `.

Examples:
  forge run                              # Orchestrator: every service + infra
  forge run --env=staging                # Orchestrator with staging env vars
  forge run --no-infra                   # Orchestrator, skip docker-compose
  forge run --service api-gateway        # Orchestrator, filter to one service
  forge run admin-server                 # Host-mode single-service runner
  forge run admin-server --background    # Detach, track PID for later stop
  forge run admin-server stop            # Kill the tracked background PID
  forge run admin-server --env-file .env.local  # Custom env file`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// `forge run <service> stop` short-circuits to the PID kill.
			if len(args) == 2 && args[1] == "stop" {
				return runHostServiceStop(args[0])
			}
			// `forge run <service>` is the host-mode single-service runner.
			if len(args) == 1 && args[0] != "stop" {
				return runHostService(cmd.Context(), args[0], opts.env, envFile, background)
			}
			// Otherwise: the orchestrator (existing behaviour).
			if serviceFlag != "" {
				opts.services = []string{serviceFlag}
			}
			return runProjectDev(opts)
		},
	}

	runCmd.Flags().StringVar(&opts.env, "env", "dev", "Environment to run (dev, staging, prod) — selects which .env.<env> file the host-mode runner loads")
	runCmd.Flags().BoolVar(&opts.noInfra, "no-infra", false, "Skip starting infrastructure via docker-compose (orchestrator only)")
	runCmd.Flags().StringVar(&serviceFlag, "service", "", "Run only a specific service or app by name (orchestrator only)")
	runCmd.Flags().BoolVar(&opts.debug, "debug", false, "Start with Delve debugger (hot-reload + debug on :2345) — orchestrator only")
	runCmd.Flags().BoolVar(&background, "background", false, "Detach the host-mode runner and return immediately (stop with `forge run <service> stop`)")
	runCmd.Flags().StringVar(&envFile, "env-file", "", "Override the env file loaded by the host-mode runner (default .env.<env>)")

	return runCmd
}
