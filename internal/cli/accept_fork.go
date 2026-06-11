// `forge generate accept-fork <path>...` — silence the fork-skip
// warning for known-accepted Tier-1 forks.
//
// The end-of-pipeline `reportForkedSkips` summary names every Tier-1
// file whose entry carries `forked: true` so the user knows the
// just-rendered content was dropped on the floor. After the first
// report fires the path's `accepted` flag is flipped, so the next run
// stays quiet — that's the "loud once, then quiet" UX.
//
// `accept-fork` does the same flip up-front, without waiting for the
// first warning to fire. Use case: porting a project with many known-
// long-lived forks (cp-forge has 11 of pkg/app/* + handlers/*
// authorizer_gen.go + KCL config_gen.k). Flipping all 11 in one shot
// silences them BEFORE the next `forge generate` so the operator
// isn't bombarded with 11 lines of fork notices they already know
// about.
//
// Refuses on:
//   - unknown paths (no checksum entry; nothing to accept)
//   - non-forked paths (no `forked: true` to silence; user probably
//     meant `forge generate --accept` to flip BOTH forked AND accepted)
//
// `forge generate unfork` clears both `forked` AND `accepted` so a
// future re-fork goes loud again. That's intentional — the loud/quiet
// dance hangs off `accepted`, and unfork is meant to take the file
// back under forge ownership entirely.
package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
)

func newAcceptForkCmd() *cobra.Command {
	var (
		dryRun bool
		reason string
	)

	cmd := &cobra.Command{
		Use:   "accept-fork <path>...",
		Short: "Silence the fork-skip warning for the named forked Tier-1 paths",
		Long: `Flip the ` + "`accepted: true`" + ` flag on each named path's checksum entry so
the end-of-pipeline forked-skip report stays quiet for it.

You are confirming you'll maintain this fork forever — future template updates
won't apply to these files. If you want forge to resume regenerating, run
` + "`forge generate unfork <path>...`" + ` instead.

Forks are design feedback: a forked file means the generated code couldn't
express what you needed. Pass --reason to record WHY, per accepted path, into
.forge/friction.jsonl (view with ` + "`forge friction list --area fork`" + `). Without
--reason a placeholder entry is still recorded.

Behavior:
  forge generate accept-fork <path>...          Flip accepted on each named path.
  forge generate accept-fork <path>... --reason "<why>"  Same, recording the fork rationale.
  forge generate accept-fork <path>... --dry-run Print what would change without writing.

Refuses to accept-fork:
  - paths that have no entry in .forge/checksums.json (nothing to accept)
  - paths that aren't currently marked ` + "`forked: true`" + ` (use ` + "`forge generate --accept`" + `
    on a Tier-1 hand-edit first; or this command on an already-forked path)

Example (bulk-silence a cohort of known-accepted forks):
  forge generate accept-fork \
    pkg/app/bootstrap.go pkg/app/wire_gen.go pkg/app/migrate.go \
    pkg/app/bootstrap_testing.go \
    --reason "custom multi-tenant bootstrap wiring forge can't express"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAcceptFork(args, dryRun, reason)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing .forge/checksums.json")
	cmd.Flags().StringVar(&reason, "reason", "", "WHY these forks exist. Recorded per accepted path in .forge/friction.jsonl as design feedback; view with 'forge friction list --area fork'.")

	return cmd
}

// runAcceptFork is the cobra RunE body, split out so tests can drive
// it directly with a synthetic args slice + flags. reason is the
// --reason text recorded per flipped path (empty ⇒ placeholder entry +
// nudge; see friction_fork.go).
func runAcceptFork(args []string, dryRun bool, reason string) error {
	root, err := projectRoot()
	if err != nil {
		return err
	}

	cs, err := checksums.Load(root)
	if err != nil {
		return cliutil.WrapUserErr("forge generate accept-fork",
			"failed to load .forge/checksums.json", "",
			"verify the file is valid JSON; if it was hand-edited, restore it from git", err)
	}

	// Validate up-front so a typo on path 3 doesn't half-apply paths 1-2.
	// Reuses normalizeUnforkPath for the same leading-./ trim semantics
	// `forge generate unfork` already gives the user — the two commands
	// take the same shape of argument list.
	var unknown, notForked, targets []string
	for _, raw := range args {
		path := normalizeUnforkPath(raw)
		entry, ok := cs.Files[path]
		if !ok {
			unknown = append(unknown, path)
			continue
		}
		if !entry.Forked {
			notForked = append(notForked, path)
			continue
		}
		targets = append(targets, path)
	}
	if len(unknown) > 0 {
		return cliutil.UserErr("forge generate accept-fork",
			fmt.Sprintf("%d path(s) not in .forge/checksums.json: %s", len(unknown), strings.Join(unknown, ", ")),
			"",
			"accept-fork only operates on tracked Tier-1 generated files; check the path spelling, or run `forge audit` to list tracked entries")
	}
	if len(notForked) > 0 {
		return cliutil.UserErr("forge generate accept-fork",
			fmt.Sprintf("%d path(s) are not currently marked `forked: true`: %s", len(notForked), strings.Join(notForked, ", ")),
			"",
			"accept-fork only silences ALREADY-forked paths; to mark a hand-edited Tier-1 file as a fork in the first place, run `forge generate --accept`")
	}

	// Dedupe so the same path passed twice doesn't double-print.
	sort.Strings(targets)
	targets = uniqueStrings(targets)

	// Partition: already-accepted (no-op) vs needs-flip.
	var willFlip, alreadyAccepted []string
	for _, path := range targets {
		entry := cs.Files[path]
		if entry.Accepted {
			alreadyAccepted = append(alreadyAccepted, path)
		} else {
			willFlip = append(willFlip, path)
		}
	}

	if len(alreadyAccepted) > 0 {
		for _, path := range alreadyAccepted {
			fmt.Printf("  ⏩ %s (already accepted; nothing to do)\n", path)
		}
	}

	if len(willFlip) == 0 {
		fmt.Println("Nothing to accept-fork.")
		return nil
	}

	if dryRun {
		fmt.Printf("\n--dry-run: would accept-fork %d entry(s):\n", len(willFlip))
		for _, path := range willFlip {
			fmt.Printf("  - %s\n", path)
		}
		fmt.Println("\n.forge/checksums.json not modified.")
		return nil
	}

	for _, path := range willFlip {
		entry := cs.Files[path]
		entry.Accepted = true
		cs.Files[path] = entry
		fmt.Printf("  ✓ accept-forked %s\n", path)
	}

	if err := checksums.Save(root, cs); err != nil {
		return cliutil.WrapUserErr("forge generate accept-fork",
			"failed to save .forge/checksums.json", "",
			"check write permissions on .forge/", err)
	}

	// Capture the fork rationale only AFTER the save succeeded — the
	// friction log must describe accepts that actually happened. Only
	// the just-flipped paths are recorded: already-accepted paths got
	// their entry on the run that accepted them, and a --dry-run never
	// reaches here (friction log stays untouched when nothing changes).
	recordForkFriction(root, "accept-fork", reason, willFlip, os.Stdout)

	fmt.Printf("\n✅ Accept-forked %d entry(s). The forge generate fork-skip report will stay quiet for them.\n", len(willFlip))
	fmt.Println("   (You are confirming you'll maintain these forks forever; future template updates won't apply.)")
	fmt.Println("   To reverse, run: forge generate unfork <path>...")
	return nil
}
