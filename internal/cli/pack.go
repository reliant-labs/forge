package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/packs"
)

func newPackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Manage installable packs",
		Long: `Manage installable packs — pre-built, opinionated implementations
that add real, working code for specific concerns (auth, payments, etc.).

Subcommands:
  forge pack list              List available packs
  forge pack install <name>    Install a pack into the project
  forge pack remove <name>     Remove a pack from the project`,
	}

	cmd.AddCommand(newPackListCmd())
	cmd.AddCommand(newPackInstallCmd())
	cmd.AddCommand(newPackRemoveCmd())

	return cmd
}

func newPackListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available packs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackList()
		},
	}
}

func newPackInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <name>",
		Short: "Install a pack into the project",
		Long: `Install a pack into the current Forge project. This will:

  1. Read the pack manifest
  2. Render templates with project config (module path, service name, etc.)
  3. Write files to the project
  4. Add Go dependencies
  5. Record the pack in forge.project.yaml
  6. Run go mod tidy

Example:
  forge pack install jwt-auth`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackInstall(args[0])
		},
	}
}

func newPackRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a pack from the project",
		Long: `Remove a pack from the current Forge project. This will:

  1. Delete files created by the pack
  2. Remove the pack from forge.project.yaml
  3. Note: Go dependencies are NOT removed (they may be used elsewhere)

Example:
  forge pack remove jwt-auth`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackRemove(args[0])
		},
	}
}

func runPackList() error {
	available, err := packs.ListPacks()
	if err != nil {
		return fmt.Errorf("list packs: %w", err)
	}

	if len(available) == 0 {
		fmt.Println("No packs available.")
		return nil
	}

	// Check which are installed (if we're in a project)
	var installed map[string]bool
	cfg, cfgErr := loadProjectConfig()
	if cfgErr == nil {
		installed = make(map[string]bool)
		for _, name := range cfg.Packs {
			installed[name] = true
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tSTATUS\tDESCRIPTION")
	for _, p := range available {
		status := ""
		if installed != nil && installed[p.Name] {
			status = "installed"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Version, status, p.Description)
	}
	return w.Flush()
}

func runPackInstall(name string) error {
	if !packs.ValidPackName(name) {
		return fmt.Errorf("invalid pack name %q", name)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.project.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	pack, err := packs.GetPack(name)
	if err != nil {
		return err
	}

	fmt.Printf("Installing pack '%s' v%s...\n", pack.Name, pack.Version)

	if err := pack.Install(root, cfg); err != nil {
		return fmt.Errorf("install pack %q: %w", name, err)
	}

	// Write updated config
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Pack '%s' installed successfully!\n", pack.Name)
	if len(pack.Generate) > 0 {
		fmt.Printf("\nThis pack has generate hooks. Run '%s generate' to generate pack code.\n", CLIName())
	}

	return nil
}

func runPackRemove(name string) error {
	if !packs.ValidPackName(name) {
		return fmt.Errorf("invalid pack name %q", name)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.project.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	if !packs.IsInstalled(name, cfg) {
		return fmt.Errorf("pack %q is not installed", name)
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
