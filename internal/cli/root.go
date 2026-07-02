package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
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

// projectRoot forwards to cmdutil.ProjectRoot — the shared project-root
// resolver. The `add` group moved to internal/cli/add and now calls cmdutil
// directly; this unexported forwarder keeps the remaining internal/cli call
// sites (ci.go, delete.go, disown.go, package.go) unchanged.
func projectRoot() (string, error) { return cmdutil.ProjectRoot() }

// validateProjectName / validateServiceName / validateFrontendName forward
// to the shared validators in cmdutil (now the canonical home, shared with
// the internal/cli/add group). new.go calls these.
func validateProjectName(name string) error  { return cmdutil.ValidateProjectName(name) }
func validateServiceName(name string) error  { return cmdutil.ValidateServiceName(name) }
func validateFrontendName(name string) error { return cmdutil.ValidateFrontendName(name) }

// Name returns the command name users should type to invoke Forge.
// When the binary is "forge" (standalone install), it returns "forge".
// When embedded in another binary (e.g. "reliant"), it returns "reliant forge".
// Forwards to cmdutil.Name so the dir-nested command groups share one
// implementation without importing internal/cli.
func Name() string {
	return cmdutil.Name()
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

			// Self-heal git-hook activation (idempotent, best-effort).
			// Placed before the experimental-warning early-returns so
			// --silence-experimental doesn't also silence it. No-op unless
			// we're in a forge project that ships .githooks/ (see
			// ensureGitHooksActivated).
			if root, err := cmdutil.FindProjectRoot(); err == nil && root != "" {
				ensureGitHooksActivated(root)
			}

			if silenceExperimental || os.Getenv("FORGE_SILENCE_EXPERIMENTAL") != "" {
				return
			}
			store, err := loadProjectStore()
			if err != nil || store == nil {
				return
			}
			emitExperimentalWarning(cmd.ErrOrStderr(), store.Features().EnabledExperimentalFeatures())
		},
	}

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().BoolVar(&silenceExperimental, "silence-experimental", false, "suppress the experimental-features warning (also: FORGE_SILENCE_EXPERIMENTAL=1)")

	// Add all commands
	rootCmd.AddCommand(newGenerateCmd())
	// `forge disown` is the one-way door from forge-owned (Tier-1) to
	// user-owned (Tier-2). Top-level because the drift-guard error
	// message prints it.
	rootCmd.AddCommand(newDisownCmd())
	// (`forge unfork`, the legacy-fork migration tool, was removed after
	// its one-release deprecation window — the legacy-manifest migration
	// in `forge generate` converts forked entries to disowned
	// automatically.)
	rootCmd.AddCommand(newDBCmd())
	rootCmd.AddCommand(newMigrateCmd())
	rootCmd.AddCommand(newNewCmd())
	// `new-env` scaffolds a new deploy/kcl/<env>/ by deriving it from an
	// existing env — converting the silent copy-paste-a-sibling footgun
	// (inherited cluster/namespace/registry/platform) into REPLACE_ME_*
	// author-time placeholders gated by `new-env --check`.
	rootCmd.AddCommand(newNewEnvCmd())
	// `add` migrated to the internal/cli/add group (factory registry).
	rootCmd.AddCommand(newDeleteCmd())
	rootCmd.AddCommand(newBuildCmd())
	rootCmd.AddCommand(newDeployCmd())
	rootCmd.AddCommand(newPromoteCmd())
	rootCmd.AddCommand(newSmokeCmd())
	rootCmd.AddCommand(newSecretsCmd())
	rootCmd.AddCommand(newTestCmd())
	// `lint` migrated to the internal/cli/lint group (factory registry).
	rootCmd.AddCommand(newPackageCmd())
	// `pack` migrated to the internal/cli/pack group (factory registry).
	// `debug` migrated to the internal/cli/debug group (factory registry).
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newDocsCmd())
	rootCmd.AddCommand(newUpgradeCmd())
	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newProtocGenForgeCmd())
	// `component` migrated to the internal/cli/component group (registered
	// via the factory registry below).
	rootCmd.AddCommand(newSkillCmd())
	rootCmd.AddCommand(newCICmd())
	rootCmd.AddCommand(newToolsCmd())
	// `backlog` migrated to the internal/cli/backlog group (factory registry).
	rootCmd.AddCommand(newFrictionCmd())
	// `audit` migrated to the internal/cli/audit group (factory registry).
	rootCmd.AddCommand(newGraphCmd())
	rootCmd.AddCommand(newMapCmd())
	rootCmd.AddCommand(newIntrospectCmd())
	rootCmd.AddCommand(newClusterCmd())
	rootCmd.AddCommand(newDevStackCmd())
	rootCmd.AddCommand(newAPICmd())
	rootCmd.AddCommand(newMCPCmd())
	rootCmd.AddCommand(newUpCmd())
	rootCmd.AddCommand(newFeaturesCmd())

	// Dir-nested command groups (internal/cli/<group>) self-register a
	// command factory via init() — they are blank-imported in groups.go so
	// the registration runs. Range the registry and attach each one. As
	// groups migrate out of the flat files above, their flat AddCommand line
	// moves here automatically (it disappears from above, appears via the
	// registry). The factory carries the shared I/O surface; group commands
	// still call package-level helpers in internal/cli directly.
	f := factory.New()
	for _, makeCmd := range factory.Registered() {
		rootCmd.AddCommand(makeCmd(f))
	}

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
