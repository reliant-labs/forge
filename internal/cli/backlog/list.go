package backlog

import (
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newListCmd(_ *factory.Factory) *cobra.Command {
	var (
		areaFilter   string
		statusFilter string
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List backlog items (filterable by area and status)",
		RunE: func(cmd *cobra.Command, args []string) error {
			items, err := loadBacklog()
			if err != nil {
				return err
			}
			items = filterBacklog(items, areaFilter, statusFilter)
			if jsonOut {
				return writeBacklogJSON(cmd.OutOrStdout(), items)
			}
			return writeBacklogTable(cmd.OutOrStdout(), items)
		},
	}
	cmd.Flags().StringVar(&areaFilter, "area", "", "Filter by area (e.g. codegen, testing)")
	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status (open, fixed)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON instead of a tab-separated table")
	return cmd
}
