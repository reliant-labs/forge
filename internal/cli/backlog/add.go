package backlog

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newAddCmd(_ *factory.Factory) *cobra.Command {
	var (
		severity string
		area     string
	)
	cmd := &cobra.Command{
		Use:   "add <title>",
		Short: "Append a new backlog item with structured frontmatter",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			title := args[0]
			if severity == "" {
				return fmt.Errorf("--severity is required")
			}
			if area == "" {
				return fmt.Errorf("--area is required")
			}

			file, err := backlogFilePath()
			if err != nil {
				return err
			}

			items, err := loadBacklog()
			if err != nil {
				return err
			}

			id := nextBacklogID(items)
			today := time.Now().UTC().Format("2006-01-02")
			section := renderNewItem(id, severity, area, today, title)

			if err := appendToFile(file, section); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&severity, "severity", "", "Severity: low | moderate | high | critical")
	cmd.Flags().StringVar(&area, "area", "", "Area / subsystem (e.g. codegen, testing, scaffold)")
	_ = cmd.MarkFlagRequired("severity")
	_ = cmd.MarkFlagRequired("area")
	return cmd
}
