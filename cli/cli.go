// Package cli provides the public entry point for embedding Forge's CLI
// as a subcommand in other cobra-based CLIs.
package cli

import (
	"github.com/spf13/cobra"

	internalcli "github.com/reliant-labs/forge/internal/cli"
)

// NewRootCmd returns the fully assembled Forge root command.
// When embedded in another CLI (e.g. "reliant forge"), the CLIName()
// function automatically adjusts user-facing command references.
func NewRootCmd() *cobra.Command {
	return internalcli.NewRootCmd()
}

// SetVersion sets the version, build date, and git commit for the Forge CLI.
func SetVersion(version, buildDate, gitCommit string) {
	internalcli.SetVersion(version, buildDate, gitCommit)
}
