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
	var silenceExperimental bool

	rootCmd := &cobra.Command{
		Use:   "forge",
		Short: "Connect RPC development framework for LLM-optimized applications",
		Long: `Forge is a development framework where everything communicates via
Connect RPC interfaces, purpose-built for LLM-driven development.

It enables easy mocking, middleware injection, spec-driven development,
and component swapping - all while maintaining a single, consistent
interface pattern throughout the entire stack.`,
		Version: fmt.Sprintf("%s (built %s, commit %s)", version, buildDate, gitCommit),
		// SilenceErrors: cobra never prints the error itself — main()
		// owns the single, final "Error: ..." line. Without this every
		// failure printed twice (cobra's copy first, buried under the
		// usage block, then main's copy) — multi-line failure reports
		// (e.g. the Tier-1 stomp-guard report, journey fr-a04f8c0609)
		// appeared twice with usage spam sandwiched between the copies.
		// SilenceUsage is NOT set here: it is set in PersistentPreRun
		// (after flag/arg parsing succeeds) so runtime errors skip the
		// usage dump while genuine usage mistakes keep the help block.
		SilenceErrors: true,
		// PersistentPreRun fires once per invocation regardless of
		// which subcommand the user typed. We use it to emit a single
		// "experimental features on" warning so users running with
		// `features.experimental.<x>: true` are reminded the schema
		// may break between versions. Suppress with
		// --silence-experimental (or FORGE_SILENCE_EXPERIMENTAL=1 in
		// CI). Errors loading config are swallowed — a missing
		// forge.yaml is the normal "outside-a-project" path.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Usage-dump suppression for RUNTIME errors only. This
			// hook runs after flag parsing and arg validation succeed,
			// so genuine usage mistakes (unknown flag, wrong arg
			// count) still print the usage block — but a pipeline-step
			// failure inside RunE (generate, add, build, …) no longer
			// buries the real error under 40 lines of flag help.
			cmd.SilenceUsage = true

			if silenceExperimental || os.Getenv("FORGE_SILENCE_EXPERIMENTAL") != "" {
				return
			}
			cfg, err := loadProjectConfig()
			if err != nil || cfg == nil {
				return
			}
			emitExperimentalWarning(cmd.ErrOrStderr(), cfg.Features.EnabledExperimentalFeatures())
		},
	}

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().BoolVar(&silenceExperimental, "silence-experimental", false, "suppress the experimental-features warning (also: FORGE_SILENCE_EXPERIMENTAL=1)")

	// Add all commands
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newGenerateCmd())
	// `forge disown` is the one-way door from forge-owned (Tier-1) to
	// user-owned (Tier-2). Top-level because the drift-guard error
	// message prints it.
	rootCmd.AddCommand(newDisownCmd())
	// `forge unfork` survives ONE release as legacy-fork migration
	// tooling (also registered under `forge generate unfork` for muscle
	// memory; see generate.go). Two cobra instances, same implementation.
	rootCmd.AddCommand(newUnforkCmd())
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
	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newProtocGenForgeCmd())
	rootCmd.AddCommand(newComponentCmd())
	rootCmd.AddCommand(newSkillCmd())
	rootCmd.AddCommand(newCICmd())
	rootCmd.AddCommand(newToolsCmd())
	rootCmd.AddCommand(newBacklogCmd())
	rootCmd.AddCommand(newFrictionCmd())
	rootCmd.AddCommand(newAuditCmd())
	rootCmd.AddCommand(newGraphCmd())
	rootCmd.AddCommand(newMapCmd())
	rootCmd.AddCommand(newIntrospectCmd())
	rootCmd.AddCommand(newConfigCmd())
	rootCmd.AddCommand(newDevCmd())
	rootCmd.AddCommand(newAPICmd())
	rootCmd.AddCommand(newUpCmd())
	rootCmd.AddCommand(newExperimentalCmd())

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
	// secretsFile overrides the KCL HostDeploy.secrets_file path. The
	// flag name stays `--env-file` for muscle-memory continuity even
	// though the contract is now "gitignored secrets dotenv" rather
	// than "every env var" — KCL env_vars carry the rest.
	var secretsFile string

	runCmd := &cobra.Command{
		Use:   "run [service]",
		Short: "Run a Forge project with hot reload, or a single host-mode service",
		Long: `Run a Forge project with the specified environment configuration.

With no positional arg, runs the orchestrator: docker compose infra, every
Go service (via Air), and every Next.js app, with color-coded log output.

With a positional service name, runs that single service as a host process —
the inner loop for services declared deploy = "host" in deploy/kcl/<env>/.
Env composition: the KCL HostDeploy block's secrets_file (gitignored
dotenv with API keys) is loaded first, then env_vars (KCL-declared
per-env config: DATABASE_URL, NATS_URL, …) is layered on top so KCL
wins on conflict — config stays reproducible across machines.

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
  forge run admin-server --env-file .env.local  # Override KCL secrets_file path`,
		// Runtime failures (a host postgres squatting on 5432, a child
		// dying) are not usage errors — dumping the flag table after
		// them buries the actionable message (journey fr-8236556f2e).
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// `forge run <service> stop` short-circuits to the PID kill.
			if len(args) == 2 && args[1] == "stop" {
				return runHostServiceStop(args[0])
			}
			// `forge run <service>` is the host-mode single-service runner.
			if len(args) == 1 && args[0] != "stop" {
				return runHostService(cmd.Context(), args[0], opts.env, secretsFile, background)
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
	runCmd.Flags().StringVar(&secretsFile, "env-file", "", "Override the KCL HostDeploy.secrets_file path (gitignored dotenv with secrets only — config lives in KCL env_vars)")
	runCmd.Flags().IntVar(&opts.proxyPort, "proxy-port", 0, "Cross-frontend dev proxy port (default 8080, auto-shifted past any declared service/frontend port; env var FORGE_RUN_PROXY_PORT also honoured). Maps <name>.localhost:<port> → each frontend / HTTP-routed service.")
	runCmd.Flags().BoolVar(&opts.noProxy, "no-proxy", false, "Disable the cross-frontend dev proxy (orchestrator only) — use the raw per-frontend ports instead of the unified <name>.localhost URL.")

	return runCmd
}
