package backlog

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newMigrateCmd(_ *factory.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Backfill structured frontmatter for legacy items (best-effort)",
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := migrateBacklog()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "migrated %d items\n", n)
			return nil
		},
	}
}
