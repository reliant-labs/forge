package cli

import "github.com/spf13/cobra"

// newMigrateCmd creates the top-level `forge migrate` command. This is a
// sibling to `forge db migrate` (which drives golang-migrate against a live
// DB); `forge migrate` is the entrypoint for project-level migration tooling
// that operates on files in the working tree — e.g. importing migrations from
// other formats (goose, dbmate, sql-migrate) into forge's golang-migrate
// shape.
//
// The split is deliberate: `forge db migrate` requires a DSN and modifies
// database state; `forge migrate import` requires neither. Conflating them
// under a single noun made the help text inscrutable.
func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Project-level migration tooling (import, convert)",
		Long: `Project-level migration tooling.

This command group operates on migration files in the working tree. It is
distinct from ` + "`forge db migrate`" + `, which drives the golang-migrate runner
against a live database.

Examples:
  forge migrate import --from goose --src-dir ../old-project/migrations`,
	}

	cmd.AddCommand(newMigrateImportCmd())

	return cmd
}
