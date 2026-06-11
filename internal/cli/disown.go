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

	cmd.Flags().StringVar(&reason, "reason", "", "WHY forge's generated code couldn't express what you needed (required; recorded per path in .forge/friction.jsonl)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing .forge/checksums.json")

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
			"failed to load .forge/checksums.json", "",
			"verify the file is valid JSON; if it was hand-edited, restore it from git", err)
	}

	// Validate the full target set up-front so a typo on path 3 doesn't
	// half-apply paths 1-2.
	var unknown, notTier1, missing, targets, alreadyDisowned []string
	for _, raw := range args {
		path := normalizeUnforkPath(raw)
		entry, ok := cs.Files[path]
		if !ok {
			unknown = append(unknown, path)
			continue
		}
		if entry.Disowned {
			alreadyDisowned = append(alreadyDisowned, path)
			continue
		}
		// Legacy forked entries are Tier-1 by recording; disowning one is
		// exactly the conversion the migration performs, so allow it.
		if !isTier1Entry(entry) {
			notTier1 = append(notTier1, path)
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(path))); statErr != nil {
			missing = append(missing, path)
			continue
		}
		targets = append(targets, path)
	}
	if len(unknown) > 0 {
		return cliutil.UserErr("forge disown",
			fmt.Sprintf("%d path(s) not in .forge/checksums.json: %s", len(unknown), strings.Join(unknown, ", ")),
			"",
			"disown only operates on tracked forge-generated files; check the path spelling, or run `forge audit` to list tracked entries")
	}
	if len(notTier1) > 0 {
		return cliutil.UserErr("forge disown",
			fmt.Sprintf("%d path(s) are already user-owned (Tier-2 scaffolds): %s", len(notTier1), strings.Join(notTier1, ", ")),
			"",
			"Tier-2 files are yours from the moment forge scaffolds them — there is nothing to disown")
	}
	if len(missing) > 0 {
		return cliutil.UserErr("forge disown",
			fmt.Sprintf("%d path(s) are tracked but missing on disk: %s", len(missing), strings.Join(missing, ", ")),
			"",
			"disown records the on-disk content as yours — restore the file first (`git checkout -- <path>`), or run `forge generate` to re-emit it")
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
		fmt.Println("\n.forge/checksums.json not modified.")
		return nil
	}

	if err := cs.DisownPaths(root, targets); err != nil {
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
			"failed to save .forge/checksums.json", "",
			"check write permissions on .forge/", err)
	}

	// Record the design feedback only AFTER the save succeeded — the
	// friction log must describe disowns that actually happened.
	recordDisownFriction(root, "disown", reason, targets, os.Stdout)

	fmt.Printf("\n✅ Disowned %d file(s). They are yours now — forge will never regenerate or overwrite them.\n", len(targets))
	fmt.Println("   To return a file to forge ownership later: delete it and run `forge generate` (your content is discarded).")
	return nil
}

