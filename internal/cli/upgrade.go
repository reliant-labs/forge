package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/generator"
)

func newUpgradeCmd() *cobra.Command {
	var (
		check bool
		force bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update frozen project files from latest Forge templates",
		Long: `Detect template drift on frozen files (files written at 'forge new' time but
not updated by 'forge generate') and apply updates from newer Forge templates.

Files that haven't been modified by the user are updated automatically.
User-modified files show a diff and are skipped unless --force is used.

Examples:
  forge upgrade          # Show diffs and apply updates
  forge upgrade --check  # Dry-run: only show what would change
  forge upgrade --force  # Apply all updates, even for user-modified files`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade(check, force)
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "Dry-run: only show what would change, don't write files")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite user-modified files without prompting")

	return cmd
}

func runUpgrade(check, force bool) error {
	configPath, err := findProjectConfigFile()
	if err != nil {
		return err
	}

	cfg, err := loadProjectConfigFrom(configPath)
	if err != nil {
		return err
	}

	projectDir := filepath.Dir(configPath)

	if check {
		fmt.Println("forge upgrade --check (dry run)")
	} else {
		fmt.Println("forge upgrade")
	}
	fmt.Println()

	results, err := generator.Upgrade(projectDir, cfg, force, check)
	if err != nil {
		return err
	}

	var updated, userModified, upToDate, skipped int
	for _, r := range results {
		switch r.Status {
		case generator.UpgradeUpToDate:
			upToDate++
			fmt.Fprintf(os.Stdout, "  %-35s up to date\n", r.Path)
		case generator.UpgradeUpdated:
			updated++
			if check {
				fmt.Fprintf(os.Stdout, "  %-35s would update\n", r.Path)
			} else {
				fmt.Fprintf(os.Stdout, "  %-35s updated\n", r.Path)
			}
		case generator.UpgradeUserModified:
			userModified++
			fmt.Fprintf(os.Stdout, "  %-35s user-modified (skipped)\n", r.Path)
			if r.Diff != "" {
				// Indent the diff for readability
				for _, line := range splitLines(r.Diff) {
					fmt.Fprintf(os.Stdout, "    %s\n", line)
				}
			}
		case generator.UpgradeSkipped:
			skipped++
			fmt.Fprintf(os.Stdout, "  %-35s skipped\n", r.Path)
		}
	}

	fmt.Println()

	// Summary
	parts := []string{}
	if updated > 0 {
		verb := "Updated"
		if check {
			verb = "Would update"
		}
		parts = append(parts, fmt.Sprintf("%s %d file(s)", verb, updated))
	}
	if userModified > 0 {
		parts = append(parts, fmt.Sprintf("%d user-modified (use --force to overwrite)", userModified))
	}
	if upToDate > 0 {
		parts = append(parts, fmt.Sprintf("%d up to date", upToDate))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}

	if len(parts) > 0 {
		for i, p := range parts {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Print(p)
		}
		fmt.Println()
	}

	return nil
}

// splitLines splits a string into lines, handling both \n and \r\n.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := make([]string, 0)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
