package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateCmdCommands scaffolds cmd/commands.go — the user-owned cobra
// extension point the generated cmd/main.go consumes (userCommands()).
// Written ONCE; never overwritten (Tier-2: the user owns the file the
// moment it exists). Second binaries register here as code with opt-in
// serverkit pieces instead of a parallel hand-rolled main().
func GenerateCmdCommands(targetDir string) error {
	cmdDir := filepath.Join(targetDir, "cmd")
	dest := filepath.Join(cmdDir, "commands.go")

	// Never overwrite — this is user-owned code.
	if _, err := os.Stat(dest); err == nil {
		return nil
	}

	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return err
	}

	content, err := templates.ProjectTemplates().Render("cmd-commands.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cmd-commands.go.tmpl: %w", err)
	}

	return writeUserScaffold(dest, content)
}
