package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/docs"
)

func newDocsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Documentation generation commands",
		Long: `Generate documentation from project metadata.

Parses proto definitions, Go contract interfaces, and project configuration
to produce markdown or Hugo-compatible documentation.

Examples:
  forge docs generate                              # Generate all docs
  forge docs generate --format=hugo                # Generate Hugo-compatible docs
  forge docs generate --generators=api,config      # Generate only API and config docs
  forge docs generate --output=./my-docs           # Custom output directory`,
	}

	cmd.AddCommand(newDocsGenerateCmd())
	return cmdutil.StrictGroup(cmd)
}

func newDocsGenerateCmd() *cobra.Command {
	var (
		outputDir  string
		format     string
		generators string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate documentation from project metadata",
		Long: `Generate documentation by parsing proto services, config annotations,
contract interfaces, and project configuration.

Generated docs include a "DO NOT EDIT" header and can be regenerated at any time.
Customize output by providing your own templates via the custom_templates_dir config.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := requireFeature(config.FeatureDocs)
			if err != nil {
				return err
			}

			fmt.Printf("📚 Generating documentation for project: %s\n\n", store.Meta().Name)

			var overrides *docs.Overrides
			if outputDir != "" || format != "" || generators != "" {
				overrides = &docs.Overrides{
					OutputDir: outputDir,
					Format:    format,
				}
				if generators != "" {
					overrides.Generators = strings.Split(generators, ",")
				}
			}

			// docs.Run prints the one line that carries proof — the file
			// count and the output directory. The unconditional "✅
			// Documentation generation complete!" that used to follow it
			// claimed success even for `--generators=bogus` (zero
			// generators selected, zero files written) and for a project
			// with docs.enabled: false. Both are errors now, and the
			// count line is the whole report.
			return docs.Run(".", store.Config(), overrides)
		},
	}

	cmd.Flags().StringVar(&outputDir, "output", "", "Output directory (default: docs/generated)")
	cmd.Flags().StringVar(&format, "format", "", "Output format: markdown or hugo (default: markdown)")
	cmd.Flags().StringVar(&generators, "generators", "", "Comma-separated list of generators to run (default: all)")

	return cmd
}
