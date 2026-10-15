package cli

import (
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/affected"
	"github.com/reliant-labs/forge/internal/cliutil"
)

// newCIAffectedCmd builds `forge ci affected`: which Go packages' tests a
// change can reach, and why. A project's Taskfile narrows `go test` with it; a
// human runs it to ask what a diff would run.
func newCIAffectedCmd() *cobra.Command {
	var o affected.Options
	cmd := &cobra.Command{
		Use:   "affected",
		Short: "Print the Go packages whose tests a change can reach, and why",
		Long: "Decides which packages' tests the work tree's changes since the merge-base with\n" +
			"origin/<default branch> (committed, staged, unstaged and untracked) can observe.\n\n" +
			"A package runs when its test binary links a changed package; a test-only file, a\n" +
			"package's testdata/ and a file only its tests embed reach that package alone. Any\n" +
			"other file must be claimed by forge.yaml ci.gate.affected_ignore (no test reads it)\n" +
			"or ci.gate.affected_rules (says what it selects); a file nothing claims, a rule\n" +
			"that declares `full`, the default branch, an empty diff and CI_FULL=1 all select\n" +
			"the whole module (`./...`). Every package is listed with the changed file that\n" +
			"selected it.\n\n" +
			"--packages prints only the package list on stdout (the plan goes to stderr), for a\n" +
			"Taskfile: `go test $(forge ci affected --packages)`. An empty list means no test\n" +
			"can observe the change.",
		Example: "  forge ci affected                          # the plan, with reasons\n" +
			"  forge ci affected --packages               # just the packages, for go test\n" +
			"  forge ci affected --files a.go,docs/x.md   # what if only these changed?\n" +
			"  forge ci affected --lane e2e               # packages whose -tags e2e tests are reached\n" +
			"  CI_BASE=origin/feature forge ci affected   # a stacked branch",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := projectRoot()
			if err != nil {
				return err
			}
			store, err := loadProjectStore()
			if err != nil {
				return err
			}
			o.Root, o.Gate = root, store.Config().CI.Gate
			if err := affected.Run(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return cliutil.UserErr("forge ci affected", err.Error(), "", "see `forge ci affected --help`")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Base, "base", "", "Ref the change is measured from (default origin/<default branch>; also CI_BASE)")
	f.StringVar(&o.Files, "files", "", "Judge this comma-separated file list (or @file, one path per line) instead of the work tree's diff")
	f.StringVar(&o.Lane, "lane", "unit", "unit (go test), e2e (-tags e2e) or integration (-tags integration)")
	f.BoolVar(&o.Packages, "packages", false, "Print only the package list on stdout; the plan goes to stderr")
	f.BoolVar(&o.JSON, "json", false, "Print the whole decision as JSON")
	f.BoolVar(&o.Full, "full", false, "Select the whole module (also CI_FULL=1)")
	return cmd
}
