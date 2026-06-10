// `forge unfork <file>...` — undo Tier-1 fork status.
//
// `forge generate --accept` lets a user mark a Tier-1 file as "forked":
// forge stops trying to regenerate it and the stomp guard ignores it.
// That's the right escape hatch when the user really did mean to take
// ownership of a generated file. But until now there was no way to
// reverse the decision.
//
// FRICTION 2026-06-03 (cp-forge port-workers): pkg/app/bootstrap.go and
// pkg/app/wire_gen.go were marked forked from an earlier round. Sibling
// files (app_gen.go) kept regenerating and emitted symbols
// (`Workers *Workers`) the forked bootstrap.go didn't carry. The build
// broke. Even `forge generate --force` skipped the forked entries —
// fork is intentionally stickier than --force. The only workaround was
// hand-editing `.forge/checksums.json`.
//
// This command makes the inverse legal: name the file(s), drop the
// `forked` flag, and the next `forge generate` will re-render the
// templates over them. We refuse on:
//
//   - unknown paths (no checksum entry; nothing to unfork)
//   - non-Tier-1 paths (Tier 2 is "scaffold once, never overwrite" — it
//     has no notion of fork status to undo)
//
// The --all flag unforks every currently-forked entry in one shot. We
// print the list and require a y/N confirmation unless --yes is set —
// undoing every fork at once is a sharp tool and a user who typo'd
// --all deserves a beat to back out.
package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
)

func newUnforkCmd() *cobra.Command {
	var (
		dryRun    bool
		all       bool
		assumeYes bool
	)

	cmd := &cobra.Command{
		Use:   "unfork [file ...]",
		Short: "Undo Tier-1 fork status so a forked generated file re-tracks the template",
		Long: `Undo the "forked" flag on a Tier-1 generated file. After unfork the next
` + "`forge generate`" + ` will re-render the template over the file.

` + "`forge generate --accept`" + ` is the inverse: it marks a Tier-1 file as forked so
forge stops trying to regenerate it. ` + "`forge unfork`" + ` exists for the case where a
user wants the file back under forge ownership — usually after a sibling
generated file regenerates and emits symbols the forked file doesn't carry.

Behavior:
  forge unfork <path>...       Drop the forked flag on each named path.
  forge unfork --all           Unfork every currently-forked entry (with confirm).
  forge unfork --all --yes     Same, without the confirm prompt.
  forge unfork <path> --dry-run Print what would change without touching state.

Refuses to unfork:
  - paths that have no entry in .forge/checksums.json (nothing to unfork)
  - paths tagged Tier-2 (scaffold-once files have no fork notion)

Example:
  forge unfork pkg/app/bootstrap.go
  forge unfork pkg/app/bootstrap.go pkg/app/wire_gen.go
  forge unfork --all --dry-run`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnfork(args, dryRun, all, assumeYes)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing .forge/checksums.json")
	cmd.Flags().BoolVar(&all, "all", false, "Unfork every currently-forked entry in .forge/checksums.json")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "Auto-confirm the --all confirmation prompt")

	return cmd
}

// runUnfork is the cobra RunE body, split out so tests can drive it
// directly with a synthetic args slice + flags.
func runUnfork(args []string, dryRun, all, assumeYes bool) error {
	root, err := projectRoot()
	if err != nil {
		return err
	}

	if all && len(args) > 0 {
		return cliutil.UserErr("forge unfork",
			"--all and positional path arguments are mutually exclusive",
			"",
			"pass either a list of paths OR --all, not both")
	}
	if !all && len(args) == 0 {
		return cliutil.UserErr("forge unfork",
			"no paths specified",
			"",
			"pass one or more paths to unfork, or --all to unfork every forked entry")
	}

	cs, err := checksums.Load(root)
	if err != nil {
		return cliutil.WrapUserErr("forge unfork",
			"failed to load .forge/checksums.json", "",
			"verify the file is valid JSON; if it was hand-edited, restore it from git", err)
	}

	// Resolve the target path set.
	var targets []string
	if all {
		for path, entry := range cs.Files {
			if entry.Forked {
				targets = append(targets, path)
			}
		}
		if len(targets) == 0 {
			fmt.Println("No forked Tier-1 entries found in .forge/checksums.json — nothing to do.")
			return nil
		}
		sort.Strings(targets)
	} else {
		// Validate each user-supplied path is in the checksum map and is
		// a Tier-1 entry. Refuse on unknown / non-Tier-1 paths up-front
		// rather than partial-failing mid-loop.
		var unknown, wrongTier []string
		for _, raw := range args {
			path := normalizeUnforkPath(raw)
			entry, ok := cs.Files[path]
			if !ok {
				unknown = append(unknown, path)
				continue
			}
			if !isTier1Entry(entry) {
				wrongTier = append(wrongTier, path)
				continue
			}
			targets = append(targets, path)
		}
		if len(unknown) > 0 {
			return cliutil.UserErr("forge unfork",
				fmt.Sprintf("%d path(s) not in .forge/checksums.json: %s", len(unknown), strings.Join(unknown, ", ")),
				"",
				"unfork only operates on tracked Tier-1 generated files; check the path spelling, or run `forge audit` to list tracked entries")
		}
		if len(wrongTier) > 0 {
			return cliutil.UserErr("forge unfork",
				fmt.Sprintf("%d path(s) are not Tier-1 generated files (Tier-2 scaffolds have no fork notion): %s", len(wrongTier), strings.Join(wrongTier, ", ")),
				"",
				"unfork only applies to Tier-1 (regenerated-every-run) files; Tier-2 files are scaffold-once and forge never auto-overwrites them")
		}
		// De-duplicate so the same path passed twice doesn't print twice.
		sort.Strings(targets)
		targets = uniqueStrings(targets)
	}

	// Partition targets into already-clean vs actually-forked. Touching
	// an already-clean entry is a no-op; report it so the user knows.
	var willFlip, alreadyClean []string
	for _, path := range targets {
		entry := cs.Files[path]
		if entry.Forked {
			willFlip = append(willFlip, path)
		} else {
			alreadyClean = append(alreadyClean, path)
		}
	}

	if len(alreadyClean) > 0 {
		for _, path := range alreadyClean {
			fmt.Printf("  ⏩ %s (not forked; nothing to do)\n", path)
		}
	}

	if len(willFlip) == 0 {
		fmt.Println("Nothing to unfork.")
		return nil
	}

	// --all guard: a user who typo'd --all wipes the entire fork set in
	// one shot. Confirm interactively unless --yes is set or --dry-run.
	if all && !assumeYes && !dryRun {
		fmt.Printf("\nAbout to unfork %d entry(s):\n", len(willFlip))
		for _, path := range willFlip {
			fmt.Printf("  - %s\n", path)
		}
		fmt.Printf("\nProceed? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		if ans != "y" && ans != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	if dryRun {
		fmt.Printf("\n--dry-run: would unfork %d entry(s):\n", len(willFlip))
		for _, path := range willFlip {
			fmt.Printf("  - %s\n", path)
		}
		fmt.Println("\n.forge/checksums.json not modified.")
		return nil
	}

	for _, path := range willFlip {
		entry := cs.Files[path]
		entry.Forked = false
		cs.Files[path] = entry
		// The parked side renders (.forge/render*, see checksums/render.go)
		// exist solely so a fork can be reconciled later; once the file is
		// back under forge ownership they're stale.
		if err := checksums.CleanSideRenders(root, path); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not clean side renders for %s: %v\n", path, err)
		}
		fmt.Printf("  ✓ unforked %s\n", path)
	}

	if err := checksums.Save(root, cs); err != nil {
		return cliutil.WrapUserErr("forge unfork",
			"failed to save .forge/checksums.json", "",
			"check write permissions on .forge/", err)
	}

	fmt.Printf("\n✅ Unforked %d entry(s). Run `forge generate` to re-render the templates over the named file(s).\n", len(willFlip))
	return nil
}

// normalizeUnforkPath cleans a user-supplied path so it can be looked up
// in cs.Files. Strips a leading "./" and normalizes separators, but
// leaves the path otherwise unchanged — the checksum map keys are
// project-relative POSIX-style paths.
func normalizeUnforkPath(raw string) string {
	p := filepath.ToSlash(raw)
	p = strings.TrimPrefix(p, "./")
	return p
}

// isTier1Entry reports whether a checksum entry is Tier-1 for the
// purposes of unfork. Tier 0 (legacy, pre-tier checksums) is treated as
// Tier-1 because all pre-tier writes were Tier-1 emitters; the stomp
// guard makes the same assumption.
func isTier1Entry(e checksums.FileChecksumEntry) bool {
	return e.Tier == 0 || e.Tier == 1
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
