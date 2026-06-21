package pack

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/packs"
)

func newInstallCmd(f *factory.Factory) *cobra.Command {
	var configPairs []string
	cmd := &cobra.Command{
		Use: "install <name>",
		// `add` is the alias to mirror `forge add operator/worker/crd` — the
		// "install vs add" inconsistency was a real LLM friction point during
		// the control-plane-next port. Both verbs map to the same RunE.
		Aliases: []string{"add"},
		Short:   "Install a pack into the project",
		Long: `Install a pack into the current Forge project. This will:

  1. Read the pack manifest
  2. Render templates with project config (module path, service name, etc.)
  3. Write files to the project
  4. Add Go dependencies
  5. Record the pack in forge.yaml
  6. Run go mod tidy

Per-pack config knobs declared in pack.yaml (under config.defaults) can be
overridden at install time with --config key=value (repeatable). The
override is shallow-merged on top of defaults and surfaced to templates as
{{ .PackConfig.<key> }}.

Examples:
  forge pack install jwt-auth
  forge pack install auth-ui --config provider=clerk`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := packsFeatureGate(f); err != nil {
				return err
			}
			return runPackInstall(cmd.Context(), args[0], configPairs)
		},
	}
	cmd.Flags().StringSliceVar(&configPairs, "config", nil,
		"Override pack config values (key=value). Repeatable; e.g. --config provider=clerk")
	return cmd
}

func newRemoveCmd(f *factory.Factory) *cobra.Command {
	return &cobra.Command{
		Use: "remove <name>",
		// `uninstall` is accepted as an alias for symmetry with `install`.
		Aliases: []string{"uninstall"},
		Short:   "Remove a pack from the project",
		Long: `Remove a pack from the current Forge project. This will:

  1. Delete files created by the pack
  2. Remove the pack from forge.yaml
  3. Note: Go dependencies are NOT removed (they may be used elsewhere)

Example:
  forge pack remove jwt-auth`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := packsFeatureGate(f); err != nil {
				return err
			}
			return runPackRemove(args[0])
		},
	}
}

func runPackInstall(ctx context.Context, name string, configPairs []string) error {
	ctxLabel := fmt.Sprintf("forge pack add %s", name)

	if !packs.ValidPackName(name) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid pack name %q", name),
			"",
			"run 'forge pack list' to see available packs (names are lowercase letters, digits, hyphens)")
	}

	root, err := cmdutil.ProjectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel,
			"read project config",
			configPath,
			"verify forge.yaml is valid YAML and you are in a forge project root",
			err)
	}

	// Sanity-check the pack exists before doing dependency resolution so
	// the error message points at the actual root cause rather than at a
	// missing-producer chain.
	if _, err := packs.GetPack(name); err != nil {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("pack %q is not available in this forge build", name),
			"",
			"run 'forge pack list' to see installable packs; check the pack name spelling")
	}

	// Resolve the full install set: requested pack + any transitive
	// `depends_on` packs that aren't installed yet. ResolveInstallOrder
	// returns the existing-installed packs first (preserving order),
	// then the new packs in topological order — producers before
	// consumers. So if the user runs `forge pack add api-key` in a
	// fresh project, the resolved order is [audit-log, api-key].
	order, err := packs.ResolveInstallOrder([]string{name}, cfg.Packs)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel,
			"resolve pack dependencies",
			"",
			"run 'forge pack list --deps' to inspect the pack dependency graph",
			err)
	}

	// Carve out the packs we need to install (skip what's already there).
	alreadyInstalled := map[string]bool{}
	for _, n := range cfg.Packs {
		alreadyInstalled[n] = true
	}
	var toInstall []string
	for _, n := range order {
		if alreadyInstalled[n] {
			continue
		}
		toInstall = append(toInstall, n)
	}
	// If the user re-installs a pack that's already in cfg.Packs,
	// ResolveInstallOrder won't surface it (because installedSet skips
	// emission). But we still want to honour the resync semantics for
	// the explicitly-requested pack — append it back if we elided it.
	if len(toInstall) == 0 || toInstall[len(toInstall)-1] != name {
		if alreadyInstalled[name] {
			toInstall = append(toInstall, name)
		}
	}

	overrides, err := packs.ParseConfigOverrides(configPairs)
	if err != nil {
		return err
	}

	// Auto-install dep banner: when we're pulling in transitive packs,
	// surface the chain so the user knows install ordering wasn't a
	// surprise. We only print the banner when there's >1 pack to install
	// AND the user only asked for one.
	if len(toInstall) > 1 {
		var deps []string
		for _, n := range toInstall {
			if n != name {
				deps = append(deps, n)
			}
		}
		fmt.Printf("Pack '%s' depends on: %v — installing in topological order.\n", name, deps)
	}

	// Aggregate the PendingProtoGenerate signal across every pack in the
	// install set. Even one pack adding a .proto means the whole cluster's
	// `go mod tidy` was deferred, so the closing hint must fire exactly
	// once at the tail — printing it per-pack would be both noisy and
	// confusing (e.g. api-key after audit-log would each emit it).
	pendingProtoGenerate := false

	for _, packName := range toInstall {
		pack, err := packs.GetPack(packName)
		if err != nil {
			return err
		}

		// Config overrides only apply to the EXPLICITLY-requested pack.
		// Transitive deps install with their pack-defined defaults — the
		// user wasn't asking to configure those packs, only to satisfy
		// the dep. If you want to configure a transitive dep, install
		// it explicitly first.
		var thisOverrides map[string]any
		if packName == name {
			thisOverrides = overrides
		}

		if packs.IsInstalled(packName, cfg) {
			fmt.Printf("Re-installing pack '%s' v%s (resync — existing files preserved)...\n", pack.Name, pack.Version)
		} else {
			fmt.Printf("Installing pack '%s' v%s...\n", pack.Name, pack.Version)
		}
		if len(thisOverrides) > 0 {
			fmt.Printf("  Config overrides: %v\n", thisOverrides)
		}

		installResult, installErr := pack.InstallWithConfig(ctx, root, cfg, thisOverrides)
		if installResult != nil && installResult.PendingProtoGenerate {
			pendingProtoGenerate = true
		}

		// Persist cfg.Packs after EVERY successful pack so a partial
		// failure mid-chain leaves a coherent forge.yaml. If install
		// errored, still try to persist (InstallWithConfig appends to
		// cfg.Packs before running `go get` / `go mod tidy`).
		if writeErr := generator.WriteProjectConfigFile(cfg, configPath); writeErr != nil {
			if installErr != nil {
				return fmt.Errorf("install pack %q: %w (additionally, update project config failed: %v)", packName, installErr, writeErr)
			}
			return fmt.Errorf("update project config: %w", writeErr)
		}

		if installErr != nil {
			return fmt.Errorf("install pack %q: %w", packName, installErr)
		}

		fmt.Printf("\n✅ Pack '%s' installed successfully!\n", pack.Name)
		if len(pack.Generate) > 0 {
			fmt.Printf("\nThis pack has generate hooks. Run '%s generate' to generate pack code.\n", cmdutil.Name())
		}
		// Wiring the user must do by hand — printed at the moment of
		// install, not buried in a README. A pack whose code has zero
		// call sites until the user edits setup/server wiring MUST say
		// so here (see Pack.PostInstall).
		if pi := strings.TrimSpace(pack.PostInstall); pi != "" {
			fmt.Printf("\nNext steps for '%s':\n", pack.Name)
			for _, line := range strings.Split(pi, "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
	}

	// Pending-proto hint: at least one pack in the install cluster emitted
	// a `.proto` file (or rode on top of a previously-emitted one), so
	// `go mod tidy` and `buf generate` were deferred. Print one clear,
	// closing instruction so the user knows the install is half-done by
	// design and the next step is theirs.
	if pendingProtoGenerate {
		fmt.Printf("\nRun `%s generate` to compile new proto definitions and finish `go mod tidy`.\n", cmdutil.Name())
	}

	return nil
}

func runPackRemove(name string) error {
	ctxLabel := fmt.Sprintf("forge pack remove %s", name)
	if !packs.ValidPackName(name) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid pack name %q", name),
			"",
			"run 'forge pack list' to see installed packs (names are lowercase letters, digits, hyphens)")
	}

	root, err := cmdutil.ProjectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel,
			"read project config",
			configPath,
			"verify forge.yaml is valid YAML",
			err)
	}

	if !packs.IsInstalled(name, cfg) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("pack %q is not installed", name),
			"",
			"run 'forge pack list' to see which packs are installed")
	}

	pack, err := packs.GetPack(name)
	if err != nil {
		return err
	}

	fmt.Printf("Removing pack '%s'...\n", pack.Name)

	if err := pack.Remove(root, cfg); err != nil {
		return fmt.Errorf("remove pack %q: %w", name, err)
	}

	// Write updated config
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Pack '%s' removed.\n", pack.Name)
	fmt.Println("Note: Go dependencies were not removed (they may be used by other code).")

	return nil
}
