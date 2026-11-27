package cli

// Removing the per-worktree driver entries an earlier forge left behind, in
// EVERY worktree of the clone.
//
// An earlier forge kept the driver command per worktree, in config.worktree
// (merge.forge-generated.driver, and forge-merge-driver.<identity>.command),
// naming one binary by absolute path with no fallback. Git reads
// config.worktree after the shared config, so such an entry shadows the
// shared, self-healing command (driverCommandLine) in that worktree — and
// `git worktree add` copies the config.worktree of the worktree it runs in
// into the new one, so a stale entry in the main worktree's
// $GIT_COMMON_DIR/config.worktree reaches every worktree made after it. When
// the binary is pruned, every merge there fails with "…/forge: not found"
// (forge reports 1b2d77d0, 316e4d7b, 3b4b7d99: control-plane's main
// worktree and all 36 of its linked worktrees carried the same stale pair).
//
// wireGeneratedMergeDriver already removed the entry from the worktree forge
// runs in. That left every other worktree shadowed until forge happened to
// run there, which a worktree made only to merge never does. So the clean-up
// covers every worktree of the clone. It reads the config.worktree files
// directly — a clone can have hundreds of worktrees, and spawning git for
// each would put seconds in front of every writing command — and spawns git
// only for a file that names forge's keys.
//
// Only forge's own stale entries go: a driver command in the shape forge
// writes (… merge-generated %O %A %B …) that cannot hand a merge to the
// pinned forge — one from before the hand-off (no `[ -x ]` guard) or one whose
// binary is gone — and a forge-merge-driver registration whose binary is gone.
// A command forge never writes, or a usable delegating one someone set on
// purpose, is left alone, as is every other key in the file.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// forgeDriverShapeRE matches a driver command forge wrote, in any shape it
// has written: the merge-generated subcommand with git's placeholders.
var forgeDriverShapeRE = regexp.MustCompile(` merge-generated %O %A %B( |$)`)

// staleWorktreeDriverEntry reports whether key = value, read from a
// config.worktree, is forge's own entry that shadows the shared driver
// without being able to merge through the pinned forge.
func staleWorktreeDriverEntry(key, value string) bool {
	switch {
	case key == "merge."+generatedMergeDriver+".driver":
		return forgeDriverShapeRE.MatchString(value) && (!driverCommandDelegates(value) || !driverCommandUsable(value))
	case strings.HasPrefix(key, pinnedDriverSection+".") && strings.HasSuffix(key, ".command"):
		return !driverCommandUsable(value)
	}
	return false
}

// removeStaleWorktreeDrivers removes forge's stale per-worktree driver
// entries from every config.worktree under commonDir (the clone's common git
// directory) and returns the worktrees it cleaned, by path when it can tell.
func removeStaleWorktreeDrivers(commonDir string) []string {
	type admin struct{ config, worktree string }
	// The main worktree's own file, then each linked worktree's.
	files := []admin{{filepath.Join(commonDir, "config.worktree"), filepath.Dir(commonDir)}}
	entries, _ := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(commonDir, "worktrees", e.Name())
		wt := dir
		// gitdir names the linked worktree's .git file.
		if data, err := os.ReadFile(filepath.Join(dir, "gitdir")); err == nil {
			if p := strings.TrimSpace(string(data)); p != "" {
				wt = filepath.Dir(p)
			}
		}
		files = append(files, admin{filepath.Join(dir, "config.worktree"), wt})
	}
	var cleaned []string
	for _, f := range files {
		data, err := os.ReadFile(f.config)
		if err != nil || (!bytes.Contains(data, []byte(generatedMergeDriver)) && !bytes.Contains(data, []byte(pinnedDriverSection))) {
			continue
		}
		if removeStaleDriverEntries(f.config) {
			cleaned = append(cleaned, f.worktree)
		}
	}
	return cleaned
}

// removeStaleDriverEntries removes forge's stale driver entries from one
// config.worktree file, and reports whether it removed any. A key is removed
// only when every value it holds is stale.
func removeStaleDriverEntries(file string) bool {
	out, err := exec.CommandContext(context.Background(), "git", "config", "--file", file, "--get-regexp",
		`^(merge\.`+regexp.QuoteMeta(generatedMergeDriver)+`\.driver|`+regexp.QuoteMeta(pinnedDriverSection)+`\..*\.command)$`).Output()
	if err != nil {
		return false
	}
	stale := map[string]bool{}
	var order []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		key, value, _ := strings.Cut(line, " ")
		if key == "" {
			continue
		}
		s, seen := stale[key]
		if !seen {
			order = append(order, key)
			s = true
		}
		stale[key] = s && staleWorktreeDriverEntry(key, value)
	}
	removed := false
	for _, key := range order {
		if !stale[key] {
			continue
		}
		if exec.CommandContext(context.Background(), "git", "config", "--file", file, "--unset-all", key).Run() != nil {
			continue
		}
		removed = true
		if key == "merge."+generatedMergeDriver+".driver" {
			_ = exec.CommandContext(context.Background(), "git", "config", "--file", file, "--unset-all", "merge."+generatedMergeDriver+".name").Run()
		}
	}
	return removed
}

// staleWorktreeDriversChange is the notice line for the worktrees
// removeStaleWorktreeDrivers cleaned.
func staleWorktreeDriversChange(cleaned []string) string {
	shown := cleaned
	more := ""
	if len(shown) > 3 {
		shown, more = shown[:3], fmt.Sprintf(" and %d more", len(cleaned)-3)
	}
	return fmt.Sprintf("removed forge's stale per-worktree driver entries (config.worktree) from %d worktree(s): %s%s. They named a forge that is gone or predates the hand-off, and shadowed the shared command there",
		len(cleaned), strings.Join(shown, ", "), more)
}
