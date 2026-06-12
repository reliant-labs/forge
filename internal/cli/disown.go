// `forge disown <path>... --reason <text>` — one-way ownership transfer.
//
// The file lifecycle has exactly two states and one door:
//
//   - forge-owned (Tier-1): regenerated on every `forge generate`;
//     hand-edits are a hard drift error.
//   - user-owned (Tier-2): never touched after emission.
//   - `forge disown` is the one-way door from the first to the second.
//
// There is deliberately NO fork limbo (forked-but-maybe-reconciled-
// later). A fleet-wide audit of every long-lived fork found zero that
// were justified — each was a bug, a missing API, a mis-tier, or
// staleness — while the limbo state produced the worst incidents
// (silently frozen wire_gen.go). Disowning is final by design; the
// documented re-adoption path is equally simple: delete the file and
// run `forge generate` — the emitter re-emits the pristine render and
// the entry returns to Tier-1.
//
// --reason is REQUIRED. A disown means the generated code couldn't
// express what the user needed — that's design feedback, and the reason
// is the payload. It is recorded per path into .forge/friction.jsonl
// (area=disown) through the same append-only machinery as `forge
// friction add`; `forge audit --json` joins it back onto each
// disowned_files row.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
)

func newDisownCmd() *cobra.Command {
	var (
		reason string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "disown <path>... --reason <text>",
		Short: "Permanently transfer a forge-generated file to user ownership (one-way)",
		Long: `Transfer one or more forge-owned (Tier-1) generated files to user ownership.

After disowning, forge NEVER touches the file again: no regeneration, no
overwrite (not even with --force), no drift errors. The file is yours, like any
starter scaffold. This is a ONE-WAY door — there is no "un-disown" that keeps
your edits.

--reason is required. Disowning means the generated code couldn't express what
you needed; the reason is design feedback, recorded per path in
.forge/friction.jsonl (view with ` + "`forge friction list --area disown`" + `).

Re-adoption (returning the file to forge ownership) is by deletion:

  rm <path> && forge generate

The emitter re-emits the pristine render and the entry returns to Tier-1. Your
disowned content is discarded — copy anything you want to keep into a
user-owned extension point first.

Prefer NOT disowning when you can: most customizations have a designated
user-owned home (pkg/app/setup.go / app_extras.go, handlers/<svc>/authorizer.go,
…) that survives every regenerate. If the extension point can't express it,
` + "`forge friction add`" + ` records the gap so forge can grow the capability.

Example:
  forge disown pkg/app/wire_gen.go --reason "custom multi-tenant pool wiring forge can't express"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDisown(args, reason, dryRun)
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "WHY forge's generated code couldn't express what you needed (required; recorded in .forge/disowned.json and .forge/friction.jsonl)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing .forge/disowned.json")

	return cmd
}

// runDisown is the cobra RunE body, split out so tests can drive it
// directly with a synthetic args slice + flags.
func runDisown(args []string, reason string, dryRun bool) error {
	if strings.TrimSpace(reason) == "" {
		return cliutil.UserErr("forge disown",
			"--reason is required: disowning a generated file is design feedback, and the reason is the payload",
			"",
			"re-run as `forge disown <path> --reason \"<why the generated code couldn't express what you need>\"`")
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	cs, err := checksums.Load(root)
	if err != nil {
		return cliutil.WrapUserErr("forge disown",
			"failed to load the .forge ownership state", "",
			"verify .forge/disowned.json and .forge/hashes.json are valid JSON; if hand-edited, restore them from git", err)
	}

	// Validate the full target set up-front so a typo on path 3 doesn't
	// half-apply paths 1-2. Ownership is read from the files themselves:
	// any path carrying forge's certification (an embedded forge:hash
	// marker or a scoped .forge/hashes.json entry) is disownable — by
	// construction this covers EVERY generated path, with no registry to
	// fall out of date (the fr-4dfef712e9 class).
	var unknown, missing, targets, alreadyDisowned []string
	for _, raw := range args {
		path := normalizeProjectRelPath(raw)
		if cs.IsDisowned(path) {
			alreadyDisowned = append(alreadyDisowned, path)
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil {
			missing = append(missing, path)
			continue
		}
		_, hasMarker := checksums.ExtractMarker(content)
		_, hasFallback := cs.Unstampable[path]
		if !hasMarker && !hasFallback {
			unknown = append(unknown, path)
			continue
		}
		targets = append(targets, path)
	}
	if len(missing) > 0 {
		return cliutil.UserErr("forge disown",
			fmt.Sprintf("%d path(s) missing on disk: %s", len(missing), strings.Join(missing, ", ")),
			"",
			"disown records the on-disk content as yours — restore the file first (`git checkout -- <path>`), or run `forge generate` to re-emit it")
	}
	if len(unknown) > 0 {
		return cliutil.UserErr("forge disown",
			fmt.Sprintf("%d path(s) carry no forge certification (no embedded forge:hash marker): %s", len(unknown), strings.Join(unknown, ", ")),
			"",
			"only forge-certified (Tier-1) files can be disowned; scaffold-once Tier-2 files are yours from birth — there is nothing to disown. If this is a pre-migration project, run `forge generate` once to migrate off .forge/checksums.json first")
	}

	sort.Strings(targets)
	targets = uniqueStrings(targets)

	if len(alreadyDisowned) > 0 {
		for _, path := range alreadyDisowned {
			fmt.Printf("  ⏩ %s (already disowned; nothing to do)\n", path)
		}
	}
	if len(targets) == 0 {
		fmt.Println("Nothing to disown.")
		return nil
	}

	if dryRun {
		fmt.Printf("\n--dry-run: would disown %d file(s):\n", len(targets))
		for _, path := range targets {
			fmt.Printf("  - %s\n", path)
		}
		fmt.Println("\nNothing modified.")
		return nil
	}

	if err := cs.DisownPaths(root, targets, reason); err != nil {
		return cliutil.WrapUserErr("forge disown",
			"failed to record disowned content", "",
			"check read permissions on the named files", err)
	}
	for _, path := range targets {
		// Stale parked renders (legacy fork side renders, --explain-drift
		// leftovers) are meaningless for a user-owned file.
		if err := checksums.CleanSideRenders(root, path); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not clean side renders for %s: %v\n", path, err)
		}
		fmt.Printf("  ✓ disowned %s\n", path)
	}

	if err := checksums.Save(root, cs); err != nil {
		return cliutil.WrapUserErr("forge disown",
			"failed to save .forge/disowned.json", "",
			"check write permissions on .forge/", err)
	}

	// Record the design feedback only AFTER the save succeeded — the
	// friction log must describe disowns that actually happened.
	recordDisownFriction(root, "disown", reason, targets, os.Stdout)

	fmt.Printf("\n✅ Disowned %d file(s). They are yours now — forge will never regenerate or overwrite them.\n", len(targets))
	fmt.Println("   To return a file to forge ownership later: delete it and run `forge generate` (your content is discarded).")
	return nil
}

// normalizeProjectRelPath cleans a user-supplied path so it matches the
// project-relative POSIX-style keys forge records. Strips a leading
// "./" and normalizes separators, but leaves the path otherwise
// unchanged.
func normalizeProjectRelPath(raw string) string {
	p := filepath.ToSlash(raw)
	p = strings.TrimPrefix(p, "./")
	return p
}

// uniqueStrings returns the input slice with contiguous duplicates
// removed. Caller has already sorted the slice so adjacent duplicates
// catch every repeat.
func uniqueStrings(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for i := 1; i < len(s); i++ {
		if s[i] != s[i-1] {
			out = append(out, s[i])
		}
	}
	return out
}

