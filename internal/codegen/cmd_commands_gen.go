package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateCmdCommands scaffolds cmd/<bin>/cmd/commands.go — the user-owned
// cobra extension point newRootCmd consumes (userCommands(deps)). Written
// ONCE; never overwritten (Tier-2: the user owns the file the moment it
// exists). Second binaries register here as code with opt-in serverkit
// pieces instead of a parallel hand-rolled main().
//
// bin is the primary binary name (the cmd/<bin>/cmd directory leaf). The
// template references it for accurate doc paths and the {{.Name}} display.
func GenerateCmdCommands(targetDir, bin string) error {
	cmdDir := filepath.Join(targetDir, "cmd", bin, "cmd")
	dest := filepath.Join(cmdDir, "commands.go")

	// Never overwrite — this is user-owned code.
	if _, err := os.Stat(dest); err == nil {
		return nil
	}

	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return err
	}

	content, err := templates.ProjectTemplates().Render("cmd-tree-commands.go.tmpl", struct{ Name string }{Name: bin})
	if err != nil {
		return fmt.Errorf("render cmd-tree-commands.go.tmpl: %w", err)
	}

	return writeUserScaffold(dest, content)
}
