package backlog

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newCloseCmd(_ *factory.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "close <id>",
		Short: "Mark a backlog item fixed (sets status: fixed + fixed_at: today)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			today := time.Now().UTC().Format("2006-01-02")
			return setBacklogStatus(args[0], "fixed", today, cmd.OutOrStdout())
		},
	}
}
