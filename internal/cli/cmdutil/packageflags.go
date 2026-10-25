package cmdutil

import "github.com/spf13/cobra"

// AdapterBaseURLDefault is --base-url's value when it is passed bare
// (`--base-url`, no `=ENV_VAR`): the adapter's base_url leaf binds
// <NAME>_BASE_URL. It doubles as the placeholder `--help` shows.
const AdapterBaseURLDefault = "<NAME>_BASE_URL"

// AddPackageScaffoldFlags registers the flags every package-scaffolding verb
// shares (`forge package new`, `forge scaffold package`, `forge scaffold
// adapter`), so the three spellings cannot drift apart.
//
//   - --base-url opts an adapter into a config block with one base_url leaf.
//     An adapter talks to SOME downstream, not necessarily one HTTP API with a
//     base URL (a queue, a vendor SDK, a storage gateway), so the leaf is
//     asked for, never assumed.
//   - --dry-run prints what the scaffold would write and changes nothing.
func AddPackageScaffoldFlags(cmd *cobra.Command) {
	cmd.Flags().String("base-url", "", "adapter only: compose a config block with a base_url leaf, bound to the env var given (bare --base-url binds "+AdapterBaseURLDefault+"); omit for an adapter with no single base URL")
	cmd.Flags().Lookup("base-url").NoOptDefVal = AdapterBaseURLDefault
	cmd.Flags().Bool("dry-run", false, "print the files and config edits the scaffold would make, and write nothing")
}
