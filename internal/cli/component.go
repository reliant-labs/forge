package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/components"
)

func newComponentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "component",
		Short: "Manage UI components from the component library",
		Long:  "List, search, and install UI components from Forge's built-in component library.",
	}
	cmd.AddCommand(newComponentListCmd())
	cmd.AddCommand(newComponentSearchCmd())
	cmd.AddCommand(newComponentInstallCmd())
	return cmd
}

func newComponentListCmd() *cobra.Command {
	var category string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all available components",
		RunE: func(cmd *cobra.Command, args []string) error {
			lib := components.NewLibrary()
			entries := lib.List("", category)
			fmt.Println(components.FormatComponentList(entries))
			return nil
		},
	}
	cmd.Flags().StringVarP(&category, "category", "c", "", "filter by category (layouts, charts, diagrams, deck, ui)")
	return cmd
}

func newComponentSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search components by keyword",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib := components.NewLibrary()
			query := strings.Join(args, " ")
			entries := lib.Search(query)
			if len(entries) == 0 {
				fmt.Printf("No components found matching %q\n", query)
				return nil
			}
			fmt.Println(components.FormatComponentList(entries))
			return nil
		},
	}
}

func newComponentInstallCmd() *cobra.Command {
	var targetDir string
	cmd := &cobra.Command{
		Use:   "install <component-names...>",
		Short: "Install components into your project",
		Long: `Install one or more components from the library into your project's
src/components/ui/ directory. If --dir is not specified, the command
auto-detects the nearest frontend directory.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib := components.NewLibrary()
			dir := targetDir
			if dir == "" {
				detected := detectComponentsDir()
				if detected == "" {
					return fmt.Errorf("could not auto-detect components directory; use --dir to specify")
				}
				dir = detected
			}

			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("create directory %s: %w", dir, err)
			}

			for _, name := range args {
				content, err := lib.Get(name)
				if err != nil {
					similar := lib.FindSimilar(name)
					if len(similar) > 0 {
						return fmt.Errorf("component %q not found. Did you mean: %s", name, strings.Join(similar, ", "))
					}
					return fmt.Errorf("component %q not found", name)
				}
				dest := filepath.Join(dir, name+".tsx")
				if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
					return fmt.Errorf("write component %s: %w", name, err)
				}
				fmt.Printf("  Installed %s -> %s\n", name, dest)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetDir, "dir", "d", "", "target directory (default: auto-detect nearest frontend)")
	return cmd
}

// detectComponentsDir looks for a frontends/*/src/components/ui/ directory
// relative to the current working directory.
func detectComponentsDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Check frontends directory
	frontendsDir := filepath.Join(cwd, "frontends")
	entries, err := os.ReadDir(frontendsDir)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(frontendsDir, e.Name(), "src", "components", "ui")
		if info, err := os.Stat(filepath.Join(frontendsDir, e.Name(), "src")); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}
