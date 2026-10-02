package storage

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/openfiles"
)

// Worktrees is explicit maintenance, never part of the scheduled cache GC.
// Git owns removal and refuses dirty/locked trees; branches are never deleted.
//
// `git worktree remove` without --force still deletes IGNORED files, and those
// are exactly where application data lives in a worktree: reliant's ./data/
// databases, a gitignored .env, forge hostinfra's postgres directory under
// .forge/hostinfra/. So "clean" here is stricter than git's: a worktree holding
// ANY ignored or untracked file is retained, and so is one any live process is
// using (an open file, or a cwd anywhere inside it).
func (r Runner) Worktrees(ctx context.Context, repo, base string, age time.Duration, apply bool) error {
	if age < 24*time.Hour {
		return fmt.Errorf("worktree age must be at least 24h")
	}
	b, err := r.command(ctx, "git", "-C", repo, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return err
	}
	baseSHA := strings.TrimSpace(string(b))
	b, err = r.command(ctx, "git", "-C", repo, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	self := openfiles.FromPaths([]string{wd})
	// Taken once, and only when there is a candidate: lsof costs seconds.
	var inUse *openfiles.Snapshot
	for index, entry := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		if index == 0 || strings.Contains(entry, "\nlocked") || strings.Contains(entry, "\nprunable") {
			continue
		}
		var path, head string
		for _, line := range strings.Split(entry, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			}
			if strings.HasPrefix(line, "HEAD ") {
				head = strings.TrimPrefix(line, "HEAD ")
			}
		}
		if path == "" || head == "" {
			continue
		}
		if self.Holds(path) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) < age {
			continue
		}
		if _, err = r.command(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", head, baseSHA); err != nil {
			continue
		}
		// --ignored is the load-bearing flag: without it an ignored database
		// is invisible here and deleted by the remove below.
		b, err = r.command(ctx, "git", "-C", path, "status", "--porcelain", "--ignored", "--untracked-files=normal")
		if err != nil {
			return err
		}
		if status := strings.TrimSpace(string(b)); status != "" {
			if strings.HasPrefix(status, "!!") || strings.Contains(status, "\n!!") {
				r.print("keep worktree %s: it holds ignored files (application data is never removed)\n", path)
			}
			continue
		}
		b, err = r.command(ctx, "git", "-C", path, "show", "-s", "--format=%ct", "HEAD")
		if err != nil {
			return err
		}
		stamp, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			return err
		}
		if time.Since(time.Unix(stamp, 0)) < age {
			continue
		}
		if inUse == nil {
			snap, err := openfiles.Take(ctx)
			if err != nil {
				// Fail closed: without the snapshot, a worktree a dev stack
				// is running from is indistinguishable from an abandoned one.
				return fmt.Errorf("refusing worktree cleanup: cannot determine which worktrees are in use: %w", err)
			}
			inUse = &snap
		}
		if inUse.Holds(path) {
			r.print("keep worktree %s: in use by a running process\n", path)
			continue
		}
		r.print("stale merged worktree: %s (%s)\n", path, head)
		if apply {
			if _, err = r.command(ctx, "git", "-C", repo, "worktree", "remove", path); err != nil {
				return err
			}
		}
	}
	return nil
}
