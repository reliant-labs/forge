// Package cli — the nouns that only group other commands: `ui`, `machine`
// and `agent` (`dev` is dev.go). Each holds commands that once sat at
// forge's root; v1.9.0 removed those old spellings (the
// migrations/v1.9.0 playbook maps each one to its noun).
package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
)

// newUICmd builds `forge ui`. The component-library group
// (internal/cli/component) is mounted under it by NewRootCmd, which holds
// the group registry.
func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Frontend tooling: the component library and frontend source maps",
	}
	cmd.AddCommand(newFrontendSourcemapsCmd())
	return cmdutil.StrictGroup(cmd)
}

// newMachineCmd builds `forge machine`: what bounds this machine rather
// than any one project.
func newMachineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "This machine's housekeeping: heavy-job slots, storage, and forge's pinned tools",
	}
	// `slot`: the host-wide heavy-job semaphore (pkg/hostslot).
	cmd.AddCommand(newSlotCmd())
	cmd.AddCommand(newStorageCmd())
	cmd.AddCommand(newToolsCmd())
	return cmdutil.StrictGroup(cmd)
}

// newAgentCmd builds `forge agent`: the surface a coding agent reads.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "For coding agents: skills, the MCP server, and filing a forge bug",
	}
	cmd.AddCommand(newSkillCmd())
	// `mcp` serves the introspection, skill, lint and generate verbs to an
	// agent harness over stdio (mcp.go).
	cmd.AddCommand(newMCPCmd())
	// `report-bug` files a defect in forge itself with the Reliant team. It
	// is the only way forge ever sends a report (internal/bugreport).
	cmd.AddCommand(newReportBugCmd())
	return cmdutil.StrictGroup(cmd)
}

// commandRel is cmd's path relative to root ("machine slot run"), in its new
// spelling when it was typed as a kept old one (canonicalRel): the key every
// path-keyed table in this package is written in.
func commandRel(cmd, root *cobra.Command) string {
	return canonicalRel(strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), root.CommandPath())))
}
