package backlog

import (
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newOpenCmd(_ *factory.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "open <id>",
		Short: "Reopen a backlog item (sets status: open, clears fixed_at)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setBacklogStatus(args[0], "open", "", cmd.OutOrStdout())
		},
	}
}
