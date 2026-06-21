package component

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/components"
	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newSearchCmd(_ *factory.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search components by keyword",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib := components.NewLibrary()
			query := strings.Join(args, " ")
			entries := lib.Search(query)
			if len(entries) == 0 {
				fmt.Printf("No components found matching %q\n", query)
				return nil
			}
			fmt.Println(components.FormatComponentList(entries))
			return nil
		},
	}
}
