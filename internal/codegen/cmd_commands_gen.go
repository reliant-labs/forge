package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateCmdCommands scaffolds internal/cli/commands.go — the user-owned
// cobra extension point newRootCmd consumes (userCommands(deps)). Written
// ONCE; never overwritten (Tier-2: the user owns the file the moment it
// exists). Second binaries register here as code with opt-in serverkit
// pieces instead of a parallel hand-rolled main().
func GenerateCmdCommands(targetDir string) error {
	cliDir := filepath.Join(targetDir, "internal", "cli")
	dest := filepath.Join(cliDir, "commands.go")

	// Never overwrite — this is user-owned code.
	if _, err := os.Stat(dest); err == nil {
		return nil
	}

	if err := os.MkdirAll(cliDir, 0755); err != nil {
		return err
	}

	content, err := templates.ProjectTemplates().Render("cli-commands.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cli-commands.go.tmpl: %w", err)
	}

	return writeUserScaffold(dest, content)
}
