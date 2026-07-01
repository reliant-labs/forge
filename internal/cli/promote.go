package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

// newPromoteCmd is `forge promote <version> --to <env>`: bind an env to a
// release. This is the "promote, don't rebuild" half of the build-once model —
// a pure pointer move. It reads the release ledger `forge build --release`
// wrote, resolves each image's digest, and records env→release in the binding
// ledger (.forge/env-releases.json). No build runs; the bytes that were cut as
// <version> are, by construction, the bytes the env will deploy.
func newPromoteCmd() *cobra.Command {
	var toEnv string

	cmd := &cobra.Command{
		Use:   "promote <version> --to <env>",
		Short: "Bind an environment to a release (build once, promote — no rebuild)",
		Long: `Bind an environment to an already-built release.

` + "`forge build --release <version>`" + ` builds the env-agnostic images ONCE,
captures their content-addressed digests, and writes a release ledger at
.forge/releases/<version>.json. ` + "`forge promote`" + ` advances that release to
an environment BY REFERENCE: it records env → release (with the resolved
per-image digests snapshotted) in .forge/env-releases.json. No image is rebuilt
— the exact bytes cut as <version> are what the env ships.

` + "`forge deploy <env>`" + ` then pins those SAME digests, so every env promoted
to the same release deploys byte-identical images. This eliminates the per-env
rebuild that re-cross-compiles (and can drift arch/tag) for every environment.

Examples:
  forge build --release v1.4.0 --push ghcr.io/acme   # build once, cut the release
  forge promote v1.4.0 --to staging                  # bind staging → v1.4.0
  forge deploy staging                               # ships v1.4.0's digests
  forge promote v1.4.0 --to prod                     # same digests advance to prod
  forge deploy prod                                  # the bytes that passed staging`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if toEnv == "" {
				return fmt.Errorf("--to <env> is required: name the environment to bind to release %q", args[0])
			}
			return runPromote(args[0], toEnv)
		},
	}

	cmd.Flags().StringVar(&toEnv, "to", "", "Environment to bind to the release (required)")

	return cmd
}

// runPromote resolves the release ledger for version and records the env→release
// binding. Resolving the digests at promote time (and snapshotting them into the
// binding) is deliberate: it makes "the bytes that passed staging ARE the bytes
// in prod" a checkable invariant — the digests are frozen the moment the env is
// promoted, independent of any later edit/move of the release file.
func runPromote(version, env string) error {
	projectDir := projectDirForKCL()

	rel, err := ReadRelease(projectDir, version)
	if err != nil {
		return fmt.Errorf("read release %q: %w", version, err)
	}
	if rel == nil {
		return fmt.Errorf("release %q not found at %s.\n"+
			"  Cut it first with: forge build --release %s --push <registry>",
			version, releasePath(projectDir, version), version)
	}

	resolved, err := resolveReleaseDigests(*rel)
	if err != nil {
		return err
	}

	er, err := ReadEnvReleases(projectDir)
	if err != nil {
		return fmt.Errorf("read env-release bindings: %w", err)
	}
	prev, hadPrev := er.Bindings[env]
	er.Bindings[env] = EnvBinding{
		Release:    version,
		Resolved:   resolved,
		PromotedAt: nowRFC3339(),
	}
	if err := WriteEnvReleases(projectDir, *er); err != nil {
		return fmt.Errorf("write env-release bindings: %w", err)
	}

	if hadPrev && prev.Release != version {
		fmt.Printf("Promoted env %q: %s → %s\n", env, prev.Release, version)
	} else {
		fmt.Printf("Promoted env %q → release %s\n", env, version)
	}
	images := make([]string, 0, len(resolved))
	for name := range resolved {
		images = append(images, name)
	}
	sort.Strings(images)
	for _, name := range images {
		fmt.Printf("  %-20s %s\n", name, resolved[name])
	}
	fmt.Printf("  Binding: %s\n", envReleasesPath(projectDir))
	fmt.Printf("  Deploy:  forge deploy %s\n", env)
	return nil
}
