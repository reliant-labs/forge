package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Worktrees is explicit maintenance, never part of the scheduled cache GC.
// Git owns removal and refuses dirty/locked trees; branches are never deleted.
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
		wd, _ := os.Getwd()
		if wd == path || strings.HasPrefix(wd, path+string(filepath.Separator)) {
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
		b, err = r.command(ctx, "git", "-C", path, "status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(b))) != 0 {
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
		r.print("stale merged worktree: %s (%s)\n", path, head)
		if apply {
			if _, err = r.command(ctx, "git", "-C", repo, "worktree", "remove", path); err != nil {
				return err
			}
		}
	}
	return nil
}
