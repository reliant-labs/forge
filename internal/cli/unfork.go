// `forge unfork <file>...` — LEGACY-FORK MIGRATION TOOLING (one release).
//
// The fork state is gone: a generated file is either forge-owned
// (Tier-1, regenerated every run) or user-owned (Tier-2, never
// touched), and `forge disown` is the one-way door between them. The
// `forge generate` pipeline automatically converts any legacy
// `forked: true` checksum entry to the disowned state, so on most
// projects this command never needs to run.
//
// unfork survives exactly one release for the projects that want to
// settle a legacy fork DELIBERATELY instead of letting the automatic
// migration pick for them:
//
//   - `forge unfork <path>` converts a legacy forked entry to disowned
//     (same outcome as the automatic migration, available without
//     running a full generate).
//   - `forge unfork --readopt <path>` returns a legacy forked OR
//     disowned entry to forge ownership: the on-disk file content is
//     DISCARDED (deleted) and the next `forge generate` re-emits the
//     pristine render.
//   - `forge unfork --merge <path>` three-way merges the frozen file
//     with the last template render older forge versions parked under
//     .forge/render* — the migration aid for keeping hand-edits while
//     re-adopting. See unfork_merge.go.
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
		merge     bool
		readopt   bool
	)

	cmd := &cobra.Command{
		Use:   "unfork [file ...]",
		Short: "Migrate a legacy forked entry to disowned, or re-adopt it (legacy-fork migration tool; removed next release)",
		Long: `MIGRATION TOOLING for legacy forks — this command will be removed next release.

The fork state no longer exists. Files are either forge-owned (Tier-1,
regenerated every run) or user-owned (Tier-2, never touched); ` + "`forge disown`" + `
is the one-way door between them. ` + "`forge generate`" + ` automatically converts any
legacy ` + "`forked: true`" + ` entry left by an older forge to the disowned state.

unfork exists to settle a legacy fork deliberately instead:

  forge unfork <path>...          Convert legacy forked entry(s) to disowned
                                  (file stays as-is, user-owned forever).
  forge unfork --readopt <path>   Return the file to forge ownership: the
                                  on-disk content is DISCARDED (deleted) and the
                                  next ` + "`forge generate`" + ` re-emits the template.
  forge unfork --merge <path>     Three-way merge the frozen file with the last
                                  parked render, then re-adopt. Keeps your
                                  hand-edits where the merge resolves cleanly.
  forge unfork --all              Convert every legacy forked entry (with confirm).
  forge unfork <path> --dry-run   Print what would change without touching state.

Refuses to operate on:
  - paths that have no entry in .forge/checksums.json
  - paths that are neither legacy-forked nor (for --readopt/--merge) disowned`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "ℹ️  `forge unfork` is legacy-fork migration tooling and will be removed next release. New workflows: `forge disown <path> --reason` (give up a file) / delete + `forge generate` (re-adopt one).")
			if merge {
				// --merge is its own flow: it needs the side renders and
				// git, and its conflict outcome intentionally leaves the
				// entry user-owned. Mixing it with --all (mass convert),
				// --readopt, or --dry-run would blur "reconcile" with
				// "discard"; refuse instead.
				if all || dryRun || readopt {
					return cliutil.UserErr("forge unfork",
						"--merge cannot be combined with --all, --readopt, or --dry-run",
						"",
						"pass explicit path(s) with --merge; use plain `forge unfork` / `forge unfork --readopt` for the non-merge flows")
				}
				return runUnforkMerge(args)
			}
			return runUnfork(args, dryRun, all, assumeYes, readopt)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing .forge/checksums.json")
	cmd.Flags().BoolVar(&all, "all", false, "Convert every legacy forked entry in .forge/checksums.json to disowned")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "Auto-confirm the --all confirmation prompt")
	cmd.Flags().BoolVar(&merge, "merge", false, "Three-way merge the frozen file with the last parked render (ours=on-disk, base=.forge/render-base, theirs=.forge/render), re-adopting on a clean merge")
	cmd.Flags().BoolVar(&readopt, "readopt", false, "Return the file(s) to forge ownership: DISCARDS the on-disk content (deletes the file); run `forge generate` afterwards to re-emit the pristine render")

	return cmd
}

// runUnfork is the cobra RunE body, split out so tests can drive it
// directly with a synthetic args slice + flags.
func runUnfork(args []string, dryRun, all, assumeYes, readopt bool) error {
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
			"pass one or more paths, or --all to convert every legacy forked entry")
	}
	if all && readopt {
		return cliutil.UserErr("forge unfork",
			"--all cannot be combined with --readopt",
			"",
			"re-adoption discards file content — name the paths explicitly")
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
			fmt.Println("No legacy forked entries found in .forge/checksums.json — nothing to do.")
			return nil
		}
		sort.Strings(targets)
	} else {
		// Validate each user-supplied path up-front rather than
		// partial-failing mid-loop. Plain unfork (convert to disowned)
		// needs a legacy forked entry; --readopt also accepts disowned.
		var unknown, notEligible []string
		for _, raw := range args {
			path := normalizeUnforkPath(raw)
			entry, ok := cs.Files[path]
			if !ok {
				unknown = append(unknown, path)
				continue
			}
			eligible := entry.Forked || (readopt && entry.Disowned)
			if !eligible {
				notEligible = append(notEligible, path)
				continue
			}
			targets = append(targets, path)
		}
		if len(unknown) > 0 {
			return cliutil.UserErr("forge unfork",
				fmt.Sprintf("%d path(s) not in .forge/checksums.json: %s", len(unknown), strings.Join(unknown, ", ")),
				"",
				"unfork only operates on tracked generated files; check the path spelling, or run `forge audit` to list tracked entries")
		}
		if len(notEligible) > 0 {
			if readopt {
				return cliutil.UserErr("forge unfork",
					fmt.Sprintf("%d path(s) are neither legacy-forked nor disowned: %s", len(notEligible), strings.Join(notEligible, ", ")),
					"",
					"forge already owns these files — nothing to re-adopt")
			}
			return cliutil.UserErr("forge unfork",
				fmt.Sprintf("%d path(s) are not legacy forked entries: %s", len(notEligible), strings.Join(notEligible, ", ")),
				"",
				"plain unfork only converts legacy `forked: true` entries to disowned; to give up a forge-owned file use `forge disown <path> --reason`, to re-adopt a disowned file delete it and run `forge generate` (or use `forge unfork --readopt <path>`)")
		}
		// De-duplicate so the same path passed twice doesn't print twice.
		sort.Strings(targets)
		targets = uniqueStrings(targets)
	}

	// --all guard: confirm interactively unless --yes is set or --dry-run.
	if all && !assumeYes && !dryRun {
		fmt.Printf("\nAbout to convert %d legacy forked entry(s) to disowned:\n", len(targets))
		for _, path := range targets {
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
		verb := "convert to disowned"
		if readopt {
			verb = "re-adopt (delete file + return entry to Tier-1)"
		}
		fmt.Printf("\n--dry-run: would %s %d entry(s):\n", verb, len(targets))
		for _, path := range targets {
			fmt.Printf("  - %s\n", path)
		}
		fmt.Println("\n.forge/checksums.json not modified.")
		return nil
	}

	if readopt {
		for _, path := range targets {
			// Discard the user content: re-adoption is by deletion, the
			// same contract as the documented disowned-file flow. The
			// next generate re-emits the pristine render (the Tier-2
			// writer chokepoint clears the disowned marker on re-emit;
			// flipping the entry here just makes the intent immediate
			// and keeps audit truthful between now and that run).
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil && !os.IsNotExist(err) {
				return cliutil.WrapUserErr("forge unfork",
					fmt.Sprintf("could not remove %s", path), "",
					"check file permissions; re-adoption discards the on-disk content", err)
			}
			entry := cs.Files[path]
			entry.Tier = 1
			entry.Disowned = false
			entry.DisownedAt = ""
			entry.Forked = false
			entry.Accepted = false
			entry.ForkedAt = ""
			cs.Files[path] = entry
			if err := checksums.CleanSideRenders(root, path); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: could not clean side renders for %s: %v\n", path, err)
			}
			fmt.Printf("  ✓ re-adopted %s (file deleted; entry returned to forge ownership)\n", path)
		}
	} else {
		for _, path := range targets {
			entry := cs.Files[path]
			entry.Tier = 2
			entry.Disowned = true
			if entry.DisownedAt == "" {
				entry.DisownedAt = entry.ForkedAt
			}
			entry.Forked = false
			entry.Accepted = false
			entry.ForkedAt = ""
			cs.Files[path] = entry
			fmt.Printf("  ✓ converted %s to disowned (user-owned; forge will never regenerate it)\n", path)
		}
	}

	if err := checksums.Save(root, cs); err != nil {
		return cliutil.WrapUserErr("forge unfork",
			"failed to save .forge/checksums.json", "",
			"check write permissions on .forge/", err)
	}

	if readopt {
		fmt.Printf("\n✅ Re-adopted %d entry(s). Run `forge generate` to re-emit the pristine render(s).\n", len(targets))
	} else {
		fmt.Printf("\n✅ Converted %d entry(s) to disowned. To re-adopt one later: delete the file and run `forge generate`.\n", len(targets))
	}
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

// isTier1Entry reports whether a checksum entry is Tier-1. Tier 0
// (legacy, pre-tier checksums) is treated as Tier-1 because all
// pre-tier writes were Tier-1 emitters; the stomp guard makes the same
// assumption.
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
