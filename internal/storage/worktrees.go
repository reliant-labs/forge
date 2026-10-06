package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/openfiles"
)

// minWorktreeIdle is the floor on how long a worktree must be untouched before
// it can be removed, whatever the caller asks for.
const minWorktreeIdle = 24 * time.Hour

// Hold reasons: why the reaper deliberately left a worktree alone.
const (
	HoldLocked    = "locked"
	HoldActive    = "recently active"
	HoldInUse     = "in use"
	HoldDirty     = "dirty"
	HoldData      = "unrecognized ignored data"
	HoldUnpushed  = "unpushed"
	HoldRefused   = "git refused removal"
	HoldUnchecked = "could not be checked"
)

// defaultWorktreeRebuildable lists ignored paths that a build recreates.
// Built from a survey of the ignored content of every reliant, forge and
// control-plane worktree on the author's machine. An entry is one or more
// path segments (globs allowed per segment) matched anywhere in the ignored
// path. Data-bearing paths never belong here, and worktreeNeverRebuildable
// vetoes them even if a policy lists them.
var defaultWorktreeRebuildable = []string{
	"node_modules", "dist", "bin", ".gocache", ".next", ".next-prod", ".turbo",
	"coverage", "coverage.out", "*.tsbuildinfo", "next-env.d.ts",
	"test-results", "playwright-report", "__pycache__", ".DS_Store",
	"kcl.mod.lock", "go.work", "go.work.sum",
	".forge/generating-build", ".forge/forge.lock", ".forge/render",
	".forge/logs", ".forge/workspace-base.tag",
}

// worktreeNeverRebuildable are paths that hold application data. They are
// held no matter what the policy says.
var worktreeNeverRebuildable = []string{
	"data", ".env", ".env.*", ".forge/hostinfra", "*.db", "*.sqlite", "secrets",
}

// DefaultWorktreeRebuildable returns a copy of the built-in allowlist.
func DefaultWorktreeRebuildable() []string {
	return append([]string(nil), defaultWorktreeRebuildable...)
}

// WorktreeHold is one worktree the reaper left alone, and why.
type WorktreeHold struct {
	Path, Repo, Reason, Detail string
}

// WorktreeReport is the outcome of one classification pass.
type WorktreeReport struct {
	// Removable are worktrees that passed every check (removed when applied).
	Removable []string
	// Removed is the subset actually removed.
	Removed []string
	Held    []WorktreeHold
}

// HeldByReason counts held worktrees per reason.
func (w WorktreeReport) HeldByReason() map[string]int {
	counts := map[string]int{}
	for _, h := range w.Held {
		counts[h.Reason]++
	}
	return counts
}

type reapOptions struct {
	repos []string
	// base overrides the repository's default base branch for the merged test.
	base  string
	idle  time.Duration
	apply bool
}

// Worktrees is the explicit `forge storage worktrees` entry point. It runs the
// same classifier the GC layer does, over one repository.
func (r Runner) Worktrees(ctx context.Context, repo, base string, idle time.Duration, apply bool) error {
	if idle < minWorktreeIdle {
		return fmt.Errorf("worktree idle time must be at least %s", minWorktreeIdle)
	}
	_, err := r.reapWorktrees(ctx, reapOptions{repos: []string{repo}, base: base, idle: idle, apply: apply})
	return err
}

// worktreeLayer is the GC layer. It only removes when the policy opts in
// (worktree_reap); otherwise it previews, whatever apply says.
func (r Runner) worktreeLayer(apply bool) error {
	repos := r.worktreeRepos()
	if len(repos) == 0 {
		return nil
	}
	remove := apply && r.Policy.WorktreeReap
	if apply && !remove {
		r.print("worktrees: preview only (set \"worktree_reap\": true in the storage policy to remove)\n")
	}
	_, err := r.reapWorktrees(r.hostCtx(), reapOptions{repos: repos, idle: minWorktreeIdle, apply: remove})
	return err
}

// PrintHeldWorktrees reports the worktrees the reaper will not touch, by
// reason, for `forge storage status`.
func (r Runner) PrintHeldWorktrees(ctx context.Context) error {
	repos := r.worktreeRepos()
	if len(repos) == 0 {
		return nil
	}
	quiet := r
	quiet.Out = nil
	report, err := quiet.reapWorktrees(ctx, reapOptions{repos: repos, idle: minWorktreeIdle})
	reasons := make([]string, 0)
	counts := report.HeldByReason()
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	r.print("worktrees: %d removable, %d held\n", len(report.Removable), len(report.Held))
	for _, reason := range reasons {
		r.print("  held (%s): %d\n", reason, counts[reason])
		for _, h := range report.Held {
			if h.Reason == reason {
				r.print("    %s\n", h.Path)
			}
		}
	}
	if err != nil {
		r.print("worktrees: %v\n", err)
	}
	return nil
}

// worktreeRepos is every repository whose worktrees are in scope: the policy's
// recorded repos plus the git toplevel of each registered project.
func (r Runner) worktreeRepos() []string {
	seen := map[string]bool{}
	var repos []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			repos = append(repos, p)
		}
	}
	// Repos were filtered when recorded; a project under the temp dir is a
	// fixture or scaffold and is never a worktree owner.
	candidates := append([]string(nil), r.Policy.Repos...)
	for _, project := range r.Policy.Projects {
		if !underTempDir(project) {
			candidates = append(candidates, project)
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if b, err := r.command(r.hostCtx(), "git", "-C", dir, "rev-parse", "--show-toplevel"); err == nil {
			add(strings.TrimSpace(string(b)))
		}
	}
	return repos
}

func (r Runner) rebuildable() []string {
	if r.Policy.WorktreeRebuildable != nil {
		return r.Policy.WorktreeRebuildable
	}
	return defaultWorktreeRebuildable
}

type worktreeEntry struct {
	path, head       string
	locked, prunable bool
	bare             bool
}

func parseWorktrees(porcelain string) []worktreeEntry {
	var entries []worktreeEntry
	for _, block := range strings.Split(strings.TrimSpace(porcelain), "\n\n") {
		var e worktreeEntry
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				e.path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "HEAD "):
				e.head = strings.TrimPrefix(line, "HEAD ")
			case line == "locked" || strings.HasPrefix(line, "locked "):
				e.locked = true
			case line == "prunable" || strings.HasPrefix(line, "prunable "):
				e.prunable = true
			case line == "bare":
				e.bare = true
			}
		}
		if e.path != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

func (r Runner) reapWorktrees(ctx context.Context, o reapOptions) (WorktreeReport, error) {
	var report WorktreeReport
	var failures []error
	wd, _ := os.Getwd()
	self := openfiles.FromPaths([]string{wd})
	var snap *openfiles.Snapshot
	var snapErr error
	seenCommon := map[string]bool{}

	for _, repo := range o.repos {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		// One repository may be reached through several registered paths (a
		// project and its worktrees); classify its worktrees once.
		if b, err := r.command(ctx, "git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			common := strings.TrimSpace(string(b))
			if seenCommon[common] {
				continue
			}
			seenCommon[common] = true
		}
		if o.apply {
			// Only drops registrations whose directory is already gone.
			if _, err := r.command(ctx, "git", "-C", repo, "worktree", "prune"); err != nil {
				failures = append(failures, fmt.Errorf("worktree prune %s: %w", repo, err))
				continue
			}
		}
		b, err := r.command(ctx, "git", "-C", repo, "worktree", "list", "--porcelain")
		if err != nil {
			failures = append(failures, fmt.Errorf("list worktrees of %s: %w", repo, err))
			continue
		}
		base := o.base
		if base == "" {
			base = r.defaultBase(ctx, repo)
		}
		for index, e := range parseWorktrees(string(b)) {
			if index == 0 || e.bare || e.head == "" {
				continue
			}
			hold := func(reason, detail string) {
				report.Held = append(report.Held, WorktreeHold{Path: e.path, Repo: repo, Reason: reason, Detail: detail})
				r.print("keep worktree %s: %s%s\n", e.path, reason, suffix(detail))
			}
			if e.locked {
				hold(HoldLocked, "")
				continue
			}
			if e.prunable {
				continue
			}
			info, err := os.Stat(e.path)
			if err != nil {
				continue
			}
			if self.Holds(e.path) {
				hold(HoldInUse, "this process's working directory")
				continue
			}
			if idle := time.Since(worktreeActivity(e.path, info)); idle < o.idle {
				hold(HoldActive, fmt.Sprintf("touched %s ago", idle.Round(time.Minute)))
				continue
			}
			b, err := r.command(ctx, "git", "-C", e.path, "status", "--porcelain", "-z", "--ignored", "--untracked-files=normal")
			if err != nil {
				hold(HoldUnchecked, err.Error())
				continue
			}
			dirty, data := r.classifyStatus(e.path, string(b))
			if dirty {
				hold(HoldDirty, "")
				continue
			}
			if len(data) > 0 {
				hold(HoldData, strings.Join(data, ", "))
				continue
			}
			if !r.pushed(ctx, e.path, e.head, base) {
				hold(HoldUnpushed, "")
				continue
			}
			// lsof costs seconds, so it is taken once and only when a
			// worktree has otherwise earned removal.
			if snap == nil && snapErr == nil {
				s, err := openfiles.Take(ctx)
				if err != nil {
					snapErr = fmt.Errorf("refusing worktree cleanup: cannot determine which worktrees are in use: %w", err)
				} else {
					snap = &s
				}
			}
			if snapErr != nil {
				hold(HoldUnchecked, "in-use state unknown")
				continue
			}
			if snap.Holds(e.path) {
				hold(HoldInUse, "a running process uses it")
				continue
			}
			report.Removable = append(report.Removable, e.path)
			if !o.apply {
				r.print("removable worktree: %s (%s)\n", e.path, e.head)
				continue
			}
			// No --force: git refuses what it considers unsafe.
			if _, err := r.command(ctx, "git", "-C", repo, "worktree", "remove", e.path); err != nil {
				report.Removable = report.Removable[:len(report.Removable)-1]
				hold(HoldRefused, err.Error())
				continue
			}
			report.Removed = append(report.Removed, e.path)
			r.print("removed worktree: %s (%s)\n", e.path, e.head)
		}
	}
	if snapErr != nil {
		failures = append(failures, snapErr)
	}
	return report, errors.Join(failures...)
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// defaultBase is origin/HEAD's target, else origin/main, else "".
func (r Runner) defaultBase(ctx context.Context, repo string) string {
	if b, err := r.command(ctx, "git", "-C", repo, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(string(b)); ref != "" {
			return ref
		}
	}
	if _, err := r.command(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", "origin/main^{commit}"); err == nil {
		return "origin/main"
	}
	return ""
}

// pushed reports whether head is safe on a remote: reachable from some
// remote-tracking ref, or an ancestor of the default base.
func (r Runner) pushed(ctx context.Context, worktree, head, base string) bool {
	if b, err := r.command(ctx, "git", "-C", worktree, "for-each-ref", "--contains", head, "refs/remotes"); err == nil && strings.TrimSpace(string(b)) != "" {
		return true
	}
	if base == "" {
		return false
	}
	_, err := r.command(ctx, "git", "-C", worktree, "merge-base", "--is-ancestor", head, base)
	return err == nil
}

// classifyStatus splits `git status --porcelain -z --ignored` into whether
// anything is modified or untracked, and the ignored paths that are not on the
// rebuildable allowlist.
func (r Runner) classifyStatus(worktree, porcelain string) (dirty bool, data []string) {
	for _, entry := range strings.Split(porcelain, "\x00") {
		entry = strings.TrimRight(entry, "\n")
		if strings.TrimSpace(entry) == "" {
			continue
		}
		if !strings.HasPrefix(entry, "!! ") {
			dirty = true
			continue
		}
		data = append(data, unrecognizedIgnored(worktree, strings.TrimPrefix(entry, "!! "), r.rebuildable())...)
	}
	return dirty, data
}

// unrecognizedIgnored returns the ignored paths under rel that are not
// rebuildable. git collapses a directory holding only ignored files into one
// entry (web/ for web/node_modules/), which would hide the allowlisted child
// from the match, so an unmatched directory is judged by its contents. Walking
// stops at every allowlisted directory, so node_modules is never traversed.
func unrecognizedIgnored(worktree, rel string, allow []string) []string {
	if rebuildablePath(rel, allow) {
		return nil
	}
	full := filepath.Join(worktree, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil || !info.IsDir() {
		return []string{rel}
	}
	children, err := os.ReadDir(full)
	if err != nil || len(children) == 0 {
		return []string{rel}
	}
	var out []string
	for _, child := range children {
		name := strings.TrimSuffix(rel, "/") + "/" + child.Name()
		if child.IsDir() {
			name += "/"
		}
		out = append(out, unrecognizedIgnored(worktree, name, allow)...)
	}
	return out
}

func splitSegments(p string) []string {
	p = strings.Trim(filepath.ToSlash(p), "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func matchesRun(segments []string, pattern string) bool {
	want := splitSegments(pattern)
	if len(want) == 0 {
		return false
	}
	for start := 0; start+len(want) <= len(segments); start++ {
		ok := true
		for i, w := range want {
			if m, err := path.Match(w, segments[start+i]); err != nil || !m {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// rebuildablePath reports whether an ignored path is recreated by a build.
// The never-list is checked first so a policy cannot make data deletable.
func rebuildablePath(p string, allow []string) bool {
	segments := splitSegments(p)
	for _, deny := range worktreeNeverRebuildable {
		if matchesRun(segments, deny) {
			return false
		}
	}
	for _, pattern := range allow {
		if matchesRun(segments, pattern) {
			return true
		}
	}
	return false
}

// worktreeActivity is the newest sign of life in a worktree: its directory,
// and the index, HEAD and reflog in its git admin directory. HEAD commit age
// would call a long-lived branch someone is actively editing "old".
func worktreeActivity(worktree string, info os.FileInfo) time.Time {
	newest := info.ModTime()
	admin := worktreeAdminDir(worktree)
	if admin == "" {
		return newest
	}
	for _, name := range []string{"index", "HEAD", filepath.Join("logs", "HEAD")} {
		if fi, err := os.Stat(filepath.Join(admin, name)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// worktreeAdminDir reads the `.git` file of a linked worktree.
func worktreeAdminDir(worktree string) string {
	b, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	if dir != "" && !filepath.IsAbs(dir) {
		dir = filepath.Join(worktree, dir)
	}
	return dir
}
