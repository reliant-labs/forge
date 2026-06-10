// `forge unfork --merge <file>...` — three-way reconcile of a forked
// Tier-1 file with the latest template render.
//
// A fork freezes a generated file while its templates (and sibling
// files) keep evolving. Plain `forge unfork` is all-or-nothing: the
// next generate re-renders the template over the file, discarding the
// user's customizations. --merge offers the middle path:
//
//	ours   = the on-disk file (the user's fork, with their edits)
//	base   = .forge/render-base/<path> (first render captured after the
//	         fork — approximately what the user forked FROM)
//	theirs = .forge/render/<path> (the latest render, refreshed on
//	         every generate run while the file was forked)
//
// `git merge-file` computes ours+theirs relative to base. On a clean
// merge the file ends up as "latest render + the user's delta", the
// entry is unforked, and the merged content is folded into the render
// history so the next `forge generate` passes the drift guard. On
// conflict the file gets standard conflict markers and STAYS forked —
// the caller (human or LLM agent) resolves the markers, then runs
// `forge unfork <path>` + `forge generate`.
//
// We shell out to `git merge-file` rather than vendoring a diff3
// implementation: git is already a hard dependency of every realistic
// forge workflow (generate --check, deploy, CI), and merge-file is the
// battle-tested reference behavior agents already know how to read.
package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
)

// runUnforkMerge is the `forge unfork --merge <path>...` body. Each
// path is validated (tracked, Tier-1, forked, side renders present)
// before any merge runs, so a multi-path invocation doesn't partially
// fail after mutating state.
func runUnforkMerge(args []string) error {
	root, err := projectRoot()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return cliutil.UserErr("forge unfork --merge",
			"no paths specified",
			"",
			"pass one or more forked Tier-1 paths to merge, e.g. `forge unfork --merge pkg/app/wire_gen.go`")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return cliutil.UserErr("forge unfork --merge",
			"git not found on PATH",
			"",
			"--merge shells out to `git merge-file` for the three-way merge; install git or reconcile the fork manually against .forge/render/<path>")
	}

	cs, err := checksums.Load(root)
	if err != nil {
		return cliutil.WrapUserErr("forge unfork --merge",
			"failed to load .forge/checksums.json", "",
			"verify the file is valid JSON; if it was hand-edited, restore it from git", err)
	}

	// Validate every target up-front.
	type mergeTarget struct {
		rel    string
		ours   string // absolute path to the user's forked file
		base   string // absolute path to .forge/render-base/<rel>
		theirs string // absolute path to .forge/render/<rel>
	}
	var targets []mergeTarget
	for _, raw := range args {
		rel := normalizeUnforkPath(raw)
		entry, ok := cs.Files[rel]
		if !ok {
			return cliutil.UserErr("forge unfork --merge",
				fmt.Sprintf("%s is not in .forge/checksums.json", rel),
				"",
				"--merge only operates on tracked Tier-1 generated files; check the path spelling, or run `forge audit` to list tracked entries")
		}
		if !isTier1Entry(entry) {
			return cliutil.UserErr("forge unfork --merge",
				fmt.Sprintf("%s is not a Tier-1 generated file (Tier-2 scaffolds have no fork notion)", rel),
				"",
				"--merge only applies to Tier-1 (regenerated-every-run) files")
		}
		if !entry.Forked {
			return cliutil.UserErr("forge unfork --merge",
				fmt.Sprintf("%s is not forked — there is nothing to reconcile", rel),
				"",
				"forge already owns this file and regenerates it on every `forge generate`")
		}
		t := mergeTarget{
			rel:    rel,
			ours:   filepath.Join(root, rel),
			base:   filepath.Join(root, checksums.RenderBaseDir, rel),
			theirs: filepath.Join(root, checksums.RenderDir, rel),
		}
		if _, err := os.Stat(t.ours); err != nil {
			return cliutil.WrapUserErr("forge unfork --merge",
				fmt.Sprintf("forked file %s is missing on disk", rel), "",
				"restore the file (e.g. `git checkout -- <path>`), or use plain `forge unfork` + `forge generate` to recreate it from the template", err)
		}
		// Pre-existing forks (created before side renders existed) have
		// no parked base/theirs. One `forge generate` run produces both.
		if _, err := os.Stat(t.theirs); err != nil {
			return cliutil.UserErr("forge unfork --merge",
				fmt.Sprintf("no side render found at %s", checksums.SideRenderRelPath(rel)),
				"",
				"run `forge generate` once first — it parks the fresh render (and the merge base) for every forked file, then re-run `forge unfork --merge`")
		}
		if _, err := os.Stat(t.base); err != nil {
			return cliutil.UserErr("forge unfork --merge",
				fmt.Sprintf("no merge base found at %s", checksums.SideRenderBaseRelPath(rel)),
				"",
				"run `forge generate` once first — it parks the fresh render (and the merge base) for every forked file, then re-run `forge unfork --merge`")
		}
		targets = append(targets, t)
	}

	conflicted := 0
	for _, t := range targets {
		merged, conflicts, err := gitMergeFile(t.ours, t.base, t.theirs, t.rel)
		if err != nil {
			return cliutil.WrapUserErr("forge unfork --merge",
				fmt.Sprintf("git merge-file failed for %s", t.rel), "",
				"inspect the error output; the fork was left untouched", err)
		}

		// Both outcomes write the merge output over the forked file —
		// for conflicts that's the standard markers-in-file workflow
		// every git user (and LLM agent) already knows how to resolve.
		if err := os.WriteFile(t.ours, merged, 0o644); err != nil {
			return fmt.Errorf("write merged %s: %w", t.rel, err)
		}

		if conflicts > 0 {
			conflicted++
			fmt.Fprintf(os.Stderr, "⚠ %s: %d conflict(s) — conflict markers written to the file. Still forked.\n", t.rel, conflicts)
			fmt.Fprintf(os.Stderr, "   Resolve the <<<<<<< markers, then run `forge unfork %s` and `forge generate`.\n", t.rel)
			continue
		}

		// Clean merge: the file is now "latest render + user delta".
		// Unfork and fold the merged content into the render history so
		// the next generate's drift guard treats it as a known state and
		// re-renders over it cleanly (rather than erroring or demanding
		// --accept, which would just re-create the fork).
		cs.RecordFile(t.rel, merged)
		entry := cs.Files[t.rel]
		entry.Tier = 1
		entry.Forked = false
		entry.ForkedAt = ""
		cs.Files[t.rel] = entry
		if err := checksums.CleanSideRenders(root, t.rel); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not clean side renders for %s: %v\n", t.rel, err)
		}
		fmt.Printf("  ✓ merged %s (clean) — unforked\n", t.rel)
		fmt.Printf("    Review the merged result, then run `forge generate`. Forge owns the file again and the\n")
		fmt.Printf("    next generate re-renders it from the template — move surviving customizations to a\n")
		fmt.Printf("    user-owned extension point%s before regenerating.\n", extensionPointSuffix(t.rel))
	}

	if err := checksums.Save(root, cs); err != nil {
		return cliutil.WrapUserErr("forge unfork --merge",
			"failed to save .forge/checksums.json", "",
			"check write permissions on .forge/", err)
	}

	if conflicted > 0 {
		return cliutil.UserErr("forge unfork --merge",
			fmt.Sprintf("%d file(s) merged with conflicts (markers written in place; entries remain forked)", conflicted),
			"",
			"resolve the conflict markers in each file, then run `forge unfork <path>` followed by `forge generate`")
	}
	return nil
}

// extensionPointSuffix renders " (e.g. <hint>)" when the path has a
// designated user-owned extension point, else "". Reuses the drift-hint
// mapping so the merge flow and the stomp-guard error teach the same
// destinations.
func extensionPointSuffix(rel string) string {
	if hint := tier1ExtensionPointHint(rel); hint != "" {
		return " (" + hint + ")"
	}
	return ""
}

// gitMergeFile runs `git merge-file -p` over ours/base/theirs and
// returns the merge output plus the conflict count.
//
// merge-file's exit status is the number of conflicts on a conflicted
// merge (clamped to 127), 0 on clean, and "negative" (255 once it hits
// a shell) on hard errors. We treat 1..127 as conflicts and anything
// else as a real failure surfaced with stderr attached.
func gitMergeFile(ours, base, theirs, rel string) ([]byte, int, error) {
	cmd := exec.Command("git", "merge-file", "-p",
		"-L", "ours (your fork: "+rel+")",
		"-L", "base (render at fork time)",
		"-L", "theirs (latest render)",
		ours, base, theirs)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return []byte(stdout.String()), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 1 && code <= 127 {
			return []byte(stdout.String()), code, nil
		}
	}
	return nil, 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
}
