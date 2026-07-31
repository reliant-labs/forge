package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/pkg/pgtest"
)

var (
	version   string
	buildDate string // Set via ldflags at build time
	gitCommit string // Set via ldflags at build time
)

// Execute is the entrypoint used by main() to dispatch the assembled
// root cobra command. Wraps NewRootCmd().Execute().
//
// pgtest.Shutdown releases this process's share of the cross-process embedded
// postgres pool (a no-op unless a command — `forge generate` and the scaffold
// verbs are the ones that reach it — actually booted or reused one). Deferred
// HERE, before main()'s os.Exit, so a failed
// command still releases its share and the last process out tears the shared
// server down cleanly instead of leaking its data dir and SysV IPC.
func Execute() error {
	defer pgtest.Shutdown()
	return NewRootCmd().Execute()
}

// projectRoot forwards to cmdutil.ProjectRoot — the shared project-root
// resolver. The `scaffold` group lives in internal/cli/scaffold and calls cmdutil
// directly; this unexported forwarder keeps the remaining internal/cli call
// sites (ci.go, delete.go, disown.go, package.go) unchanged.
func projectRoot() (string, error) { return cmdutil.ProjectRoot() }

// validateProjectName / validateServiceName / validateFrontendName forward
// to the shared validators in cmdutil (now the canonical home, shared with
// the internal/cli/scaffold group). new.go calls these.
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
// executable path. Use this when spawning forge as a subprocess (the
// protoc-gen-forge buf plugin, `forge ci` re-entry).
//
// The subcommand route comes from cmdutil.CmdRouteTokens when the CLI
// recorded it (the normal case — the root PersistentPreRun records how
// forge is mounted before any RunE fires): a standalone forge binary
// self-invokes as [exe] no matter what the file is named, an embedded
// `reliant forge` mount as [exe, "forge"]. The basename heuristic
// remains only as a fallback for callers outside cobra execution —
// guessing from the basename was the trap that made a temp-built
// standalone binary self-invoke `<exe> forge …`, hit cobra's "unknown
// command", and break descriptor generation.
func forgeExecCommand() ([]string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	if tokens, ok := cmdutil.CmdRouteTokens(); ok {
		return append([]string{exePath}, tokens...), nil
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
	var silenceExperimental bool

	var rootCmd *cobra.Command
	rootCmd = &cobra.Command{
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

			// Record how forge is mounted in the executing command
			// tree (standalone root vs a `reliant forge`-style
			// subcommand) so self-invocation (forgeExecCommand) and
			// next-step hints (cmdutil.Name) route correctly no matter
			// what the binary file is named.
			cmdutil.RecordCmdRoute(rootCmd)

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

	// There is no global --verbose. Verbosity is per-command because there
	// is nothing global to turn up: `forge generate --verbose` lists skipped
	// steps, `forge doctor -v` shows evidence for passing checks, and those
	// are different outputs, not two dials on one substrate. A root
	// PersistentFlag bound to a local nobody read used to accept `-v` on
	// EVERY command and do nothing with it — and, worse, shadowed
	// `forge generate -v`, so the one place a user would most expect it
	// silently produced ordinary output. Commands that want it register it.
	rootCmd.PersistentFlags().BoolVar(&silenceExperimental, "silence-experimental", false, "suppress the experimental-features warning (also: FORGE_SILENCE_EXPERIMENTAL=1)")

	// Add all commands
	// `forge run` is the single-command dev runner (alias for
	// `forge env up --host-only` + dev-server passthrough) — restored for the
	// reliant one-shot's `reliant forge run -- --host 0.0.0.0` preview flow.
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newGenerateCmd())
	// (`forge unfork`, the legacy-fork migration tool, was removed after
	// its one-release deprecation window — the legacy-manifest migration
	// in `forge generate` converts forked entries to disowned
	// automatically.)
	rootCmd.AddCommand(newDBCmd())
	rootCmd.AddCommand(newBuildCmd())
	// `forge test` was REMOVED. The suite a project runs is defined in its own
	// Taskfile.yml (`task test`, `task test:integration`, `task test:e2e`), and
	// that is what the generated CI workflow and the reliant one-shot gate now
	// invoke. Forge owning a second spelling of `go test` + `npm test` bought
	// nothing and cost correctness: the two definitions drifted, so `forge test`
	// could report green over a suite that was not the project's suite (and it
	// ran the whole Go suite twice — untagged, then under -tags=integration,
	// which ADDS files rather than filtering them). Contrast `forge lint`, which
	// stays because it orchestrates analyzers that exist only inside forge.
	//
	// newTestRemovedCmd below is a signpost, not a command: it only errors.
	rootCmd.AddCommand(newTestRemovedCmd())
	// `lint` migrated to the internal/cli/lint group (factory registry).
	rootCmd.AddCommand(newPackageCmd())
	// `debug` migrated to the internal/cli/debug group (factory registry).
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newDocsCmd())
	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newProtocGenForgeCmd())
	// `component` migrated to the internal/cli/component group (registered
	// via the factory registry below).
	rootCmd.AddCommand(newSkillCmd())
	rootCmd.AddCommand(newCICmd())
	rootCmd.AddCommand(newToolsCmd())
	rootCmd.AddCommand(newClusterCmd())
	rootCmd.AddCommand(newAPICmd())
	// `env` is the environment noun: every env-REQUIRED lifecycle verb
	// (up/down/status/list/new/deploy/promote/smoke/secrets/devstack)
	// lives under it with the env as a positional argument. Commands where
	// env is an optional modifier (e.g. `forge build [env]`) stay at root.
	rootCmd.AddCommand(newEnvCmd())
	// `project` is the project-structure noun: the commands that create,
	// retire, or INSPECT a project as a whole (new/delete/disown/migrate/
	// upgrade/map/graph/introspect/features/annotations). The flat members
	// attach in newProjectCmd; `audit` attaches below via the factory
	// registry. Scaffolding is NOT here — it is the top-level `forge
	// scaffold` verb.
	projectCmd := newProjectCmd()
	rootCmd.AddCommand(projectCmd)

	// Dir-nested command groups (internal/cli/<group>) self-register a
	// command factory via init() — they are blank-imported in groups.go so
	// the registration runs. Range the registry and attach each one under
	// its home: `audit` is a project-inspection view, so it nests under
	// `forge project`; everything else (including `scaffold`) is a
	// top-level verb. The factory carries the shared I/O surface; group
	// commands still call package-level helpers in internal/cli directly.
	f := factory.New()
	for _, makeCmd := range factory.Registered() {
		c := makeCmd(f)
		switch c.Name() {
		case "audit":
			projectCmd.AddCommand(c)
		default:
			rootCmd.AddCommand(c)
		}
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
