package devstack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// PrunedBlock describes one registry entry Prune reclaimed (or, in dry-run
// mode, would reclaim).
type PrunedBlock struct {
	Key   string
	Index int
}

// Prune reclaims DEV-STACK registry entries (entry.Stack == true, see
// blocks.go) whose worktree no longer exists on disk, and returns what it
// reclaimed (or, when dryRun is true, what it WOULD reclaim, without writing
// anything).
//
// Two keys are NEVER touched, unconditionally, regardless of what git
// reports:
//
//   - A plain port-block key (Stack == false) — e.g. "prod", prod's
//     reliant-web dev-server port. It is not tied to ANY worktree, so it can
//     never look "dead"; reclaiming it would silently move a live stack's
//     port. This is the same key-kind distinction AllocateBlock records at
//     allocation time specifically so this decision doesn't have to be
//     guessed later — see blocks.go's entry doc for the incident that made
//     the distinction necessary in the first place.
//   - The default key "" (block 0). It is implicit and never stored by any
//     normal write path, but Prune checks for it explicitly rather than
//     relying on that invariant holding forever.
//
// Liveness for the remaining (stack) keys is determined by enumerating real
// worktrees via `git worktree list --porcelain` and comparing each stack key
// against the SANITIZED basename of every surviving worktree root — the
// exact derivation Worktree() uses, so a key that genuinely still backs a
// live worktree is never mistaken for dead.
//
// If that enumeration fails for ANY reason (git missing, projectDir is not a
// git checkout, the command errors), Prune reclaims NOTHING and returns the
// error. A failed enumeration gives no way to tell live from dead, and
// deleting a block that is actually still live would move a running stack's
// ports out from under it — the one mistake this command must never make.
//
// The registry mutation happens under the same lock AllocateBlock uses
// (withLock), so a concurrent `forge env up` claiming a new block cannot
// race a prune.
func Prune(projectDir string, dryRun bool) ([]PrunedBlock, error) {
	liveKeys, err := liveWorktreeKeys(projectDir)
	if err != nil {
		return nil, fmt.Errorf("enumerate live worktrees (reclaiming nothing): %w", err)
	}

	var pruned []PrunedBlock
	err = withLock(projectDir, func() error {
		reg, err := readRegistry(projectDir)
		if err != nil {
			return err
		}
		changed := false
		for key, e := range reg {
			if key == "" || !e.Stack {
				// Never a stack: the default block, or a plain port-block
				// key (e.g. "prod") that isn't tied to any worktree.
				continue
			}
			if liveKeys[key] {
				continue // worktree still exists
			}
			pruned = append(pruned, PrunedBlock{Key: key, Index: e.Block})
			if !dryRun {
				delete(reg, key)
				changed = true
			}
		}
		if dryRun || !changed {
			return nil
		}
		return writeRegistry(projectDir, reg)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(pruned, func(a, b int) bool { return pruned[a].Index < pruned[b].Index })
	return pruned, nil
}

// liveWorktreeKeys returns the set of sanitized worktree-root basenames that
// `git worktree list --porcelain` reports as currently present for
// projectDir — the exact registry key Worktree() would derive for each one.
//
// A worktree git itself marks "prunable" (its administrative record
// survives in .git/worktrees/<name> but the working directory is gone) is
// deliberately EXCLUDED from the live set: "prunable" is git's own word for
// the condition this function exists to detect. As a second, independent
// check — because a "prunable" annotation depends on git's own bookkeeping
// noticing the directory is gone, which is exactly the kind of implicit
// state this whole registry-lifecycle gap was about — a worktree root that
// no longer os.Stat()s is also excluded even if git hasn't yet marked it
// prunable.
func liveWorktreeKeys(projectDir string) (map[string]bool, error) {
	out, err := gitWorktreeListPorcelain(projectDir)
	if err != nil {
		return nil, err
	}

	keys := make(map[string]bool)
	var currentRoot string
	prunable := false
	flush := func() {
		if currentRoot != "" && !prunable {
			if _, statErr := os.Stat(currentRoot); statErr == nil {
				keys[Sanitize(filepath.Base(currentRoot))] = true
			}
		}
		currentRoot = ""
		prunable = false
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			currentRoot = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "prunable"):
			prunable = true
		case line == "":
			flush()
		}
	}
	flush()
	return keys, nil
}

func gitWorktreeListPorcelain(projectDir string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "worktree", "list", "--porcelain")
	if projectDir != "" {
		cmd.Dir = projectDir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git worktree list --porcelain (in %s): %w", projectDir, err)
	}
	return string(out), nil
}

// FirstAllocationNotice is the one-line diagnostic forge should print the
// FIRST time a key is assigned a port block — the moment AllocateBlock
// actually mutates .forge/blocks.json rather than just reading an existing
// entry.
//
// Why this needs to exist at all: a caller like `forge env render` looks
// like a pure query, but rendering a new env for the first time allocates a
// REAL block through this same path — that silent write is exactly how the
// key "prod" appeared in control-plane's registry with nobody aware a write
// had happened. The write itself is legitimate and must stay (render has to
// produce the ports `up` will really use), so this only makes it visible,
// it does not gate it.
//
// Call it at the exact point AllocateBlock decides a key has NO existing
// entry and is about to assign it the next free block — i.e. right where
// `block = nextFreeBlock(reg)` runs, before `writeRegistry` — with:
//
//	fmt.Fprintln(os.Stderr, FirstAllocationNotice(key, block))
func FirstAllocationNotice(key string, block int) string {
	return fmt.Sprintf(
		"forge: allocated new port block %d for key %q (.forge/blocks.json) — "+
			"first time this key was seen; its ports are now offset by +%d00",
		block, key, block,
	)
}
