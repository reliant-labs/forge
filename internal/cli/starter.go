package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/starters"
)

// newStarterCmd builds the `forge starter` command tree. Starters are
// the lighter-weight cousin of packs: a one-time copy of opinionated
// code into the project that the user owns thereafter. There is no
// install/upgrade lifecycle, no `pack.yaml` registration, and no
// `forge.yaml` tracking — `forge starter add` writes files and
// exits.
//
// Use starters for **business integrations** (Stripe billing, Twilio
// SMS, Clerk webhook user-sync) where every project diverges and
// central maintenance creates more bugs than it prevents. Pure
// infrastructure (auth middleware, JWKS rotation, audit interceptor)
// stays as packs.
func newStarterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "starter",
		Short: "Scaffold one-time business-integration starters (you own the code)",
		Long: `Manage starter scaffolds — one-time copies of opinionated code into
the project that the user owns thereafter.

Unlike packs, starters have no install/upgrade lifecycle. ` + "`forge starter add`" + ` writes
files and exits; forge does not track them in forge.yaml, run go mod tidy, or
re-render them on subsequent generates. After the scaffold lands, you own the
code and edit it however you need.

Subcommands:
  forge starter list              List available starters
  forge starter add <name>        Copy starter files into the project
  forge starter add <name> --service <svc>   Route into a specific service`,
	}
	cmd.AddCommand(newStarterListCmd())
	cmd.AddCommand(newStarterAddCmd())
	return cmd
}

func newStarterListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available starters",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStarterList(cmd.OutOrStdout())
		},
	}
}

func newStarterAddCmd() *cobra.Command {
	var (
		serviceFlag string
		forceFlag   bool
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Copy starter files into the project (one-time scaffold)",
		Long: `Copy a starter scaffold into the current Forge project. After this
command lands, the user owns every file and is responsible for keeping the
external SDK / API surface up to date. Forge will NOT regenerate, lint, or
upgrade the resulting code.

Examples:
  forge starter add stripe --service billing
  forge starter add twilio --service notifications
  forge starter add clerk-webhook --service api`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStarterAdd(args[0], serviceFlag, forceFlag, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&serviceFlag, "service", "",
		"Target service slug used to route destination paths (e.g. handlers/<service>/...)")
	cmd.Flags().BoolVar(&forceFlag, "force", false,
		"Overwrite existing files instead of skipping them (default: skip)")
	return cmd
}

func runStarterList(out interface {
	Write(p []byte) (int, error)
}) error {
	available, err := starters.ListStarters()
	if err != nil {
		return fmt.Errorf("list starters: %w", err)
	}
	if len(available) == 0 {
		fmt.Fprintln(out, "No starters available.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESCRIPTION")
	for _, s := range available {
		desc := s.Description
		if i := indexByte(desc, '\n'); i >= 0 {
			desc = desc[:i]
		}
		fmt.Fprintf(w, "%s\t%s\n", s.Name, desc)
	}
	return w.Flush()
}

func runStarterAdd(name, service string, force bool, out interface {
	Write(p []byte) (int, error)
}) error {
	if !starters.ValidStarterName(name) {
		return fmt.Errorf("invalid starter name %q", name)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	starter, err := starters.LoadStarter(name)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Adding starter '%s'...\n", starter.Name)
	if service != "" {
		fmt.Fprintf(out, "  Routing into service: %s\n", service)
	}
	if force {
		fmt.Fprintln(out, "  --force enabled: existing files will be overwritten")
	}

	if err := starter.Add(starters.AddOptions{
		ProjectDir:  root,
		ModulePath:  cfg.ModulePath,
		ProjectName: cfg.Name,
		Service:     service,
		Force:       force,
		Stdout:      out,
	}); err != nil {
		return fmt.Errorf("add starter %q: %w", name, err)
	}

	// Mirror `forge pack install`'s post-install tidy. Starters drop new
	// imports into the project that goimports would resolve on next
	// build, but the cold-build state is a fresh checkout where tidy has
	// not yet pulled the modules — symmetrical to pack install, which
	// already runs tidy. Best-effort: if go.mod is absent (frontend-only
	// starter, or a corrupted project) we just print a hint and bail
	// without failing the scaffold.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		fmt.Fprintln(out, "\n  Running go mod tidy...")
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = root
		tidy.Stdout = os.Stdout
		tidy.Stderr = os.Stderr
		if err := tidy.Run(); err != nil {
			// Don't surface as a starter-add failure: the files landed,
			// the user owns them, and tidy can be run manually. Some
			// starters reference packages the user must `go get`
			// themselves (StarterDeps.Go is intentionally
			// echo-not-install) so a tidy failure here is plausible.
			fmt.Fprintf(out, "  Warning: go mod tidy failed (%v) — run it manually after adding the listed Go deps.\n", err)
		}
	}

	fmt.Fprintf(out, "\nStarter '%s' scaffolded. You own these files now — forge will not regenerate them.\n", starter.Name)
	return nil
}

// helper to make starter binary tests portable against an io.Writer
// that doesn't necessarily implement os.Stdout's full surface.
var _ = os.Stdout
