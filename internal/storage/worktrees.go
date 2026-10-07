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
	HoldHidden    = "hidden-changes"
	HoldNested    = "nested-repository"
	HoldOrphans   = "unreachable-commits"
	// HoldQuarantined is a worktree a previous pass moved aside and could not
	// move back. It is still a valid worktree and is never deleted by forge.
	HoldQuarantined = "quarantined"
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
	"target", ".venv", "venv", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".gradle",
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
		if b, err := r.readGit(r.hostCtx(), dir, "rev-parse", "--show-toplevel"); err == nil {
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
	inUse := lazyOpenFiles{take: r.openSnapshot}
	seenCommon := map[string]bool{}

	for _, repo := range o.repos {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		// One repository may be reached through several registered paths (a
		// project and its worktrees); classify its worktrees once.
		if b, err := r.readGit(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
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
		b, err := r.readGit(ctx, repo, "worktree", "list", "--porcelain")
		if err != nil {
			failures = append(failures, fmt.Errorf("list worktrees of %s: %w", repo, err))
			continue
		}
		base := o.base
		if base == "" {
			base = r.defaultBase(ctx, repo)
		}
		for index, e := range parseWorktrees(string(b)) {
			if index == 0 || e.bare || e.head == "" || e.prunable {
				continue
			}
			if strings.HasPrefix(filepath.Base(e.path), quarantinePrefix) {
				detail := "left by an interrupted cleanup; move it back with `git worktree move`"
				report.Held = append(report.Held, WorktreeHold{Path: e.path, Repo: repo, Reason: HoldQuarantined, Detail: detail})
				r.print("keep worktree %s: %s (%s)\n", e.path, HoldQuarantined, detail)
				continue
			}
			reason, detail, skip := r.classifyWorktree(ctx, e, base, o.idle, self, func(p string) (bool, error) { return inUse.holds(ctx, p) })
			if skip {
				continue
			}
			if reason != "" {
				report.Held = append(report.Held, WorktreeHold{Path: e.path, Repo: repo, Reason: reason, Detail: detail})
				r.print("keep worktree %s: %s%s\n", e.path, reason, suffix(detail))
				continue
			}
			report.Removable = append(report.Removable, e.path)
			if !o.apply {
				r.print("removable worktree: %s (%s)\n", e.path, e.head)
				continue
			}
			// The batch classification can be minutes old, and anything that
			// writes between a re-check and a remove would be deleted. So the
			// worktree is moved aside first (a path-based writer then gets
			// ENOENT), re-classified at the new path, and only then removed.
			reason, detail, removed := r.reclaimWorktree(ctx, repo, e, base, o.idle, self)
			if !removed {
				report.Removable = report.Removable[:len(report.Removable)-1]
				report.Held = append(report.Held, WorktreeHold{Path: e.path, Repo: repo, Reason: reason, Detail: detail})
				r.print("keep worktree %s: %s%s\n", e.path, reason, suffix(detail))
				continue
			}
			report.Removed = append(report.Removed, e.path)
			r.print("removed worktree: %s (%s)\n", e.path, e.head)
		}
	}
	if inUse.err != nil {
		failures = append(failures, inUse.err)
	}
	return report, errors.Join(failures...)
}

// classifyWorktree decides one linked worktree: "" reason means removable.
// skip means it is not a candidate at all (gone from disk).
func (r Runner) classifyWorktree(ctx context.Context, e worktreeEntry, base string, idle time.Duration, self openfiles.Snapshot, holds func(path string) (bool, error)) (reason, detail string, skip bool) {
	if e.locked {
		return HoldLocked, "", false
	}
	info, err := os.Stat(e.path)
	if err != nil {
		return "", "", true
	}
	if self.Holds(e.path) {
		return HoldInUse, "this process's working directory", false
	}
	if since := time.Since(worktreeActivity(e.path, info)); since < idle {
		return HoldActive, fmt.Sprintf("touched %s ago", since.Round(time.Minute)), false
	}
	b, err := r.readGit(ctx, e.path, "status", "--porcelain", "-z", "--ignored", "--untracked-files=normal")
	if err != nil {
		return HoldUnchecked, err.Error(), false
	}
	dirty, data := r.classifyStatus(e.path, string(b))
	if dirty {
		return HoldDirty, "", false
	}
	if len(data) > 0 {
		return HoldData, strings.Join(data, ", "), false
	}
	// Edits to skip-worktree / assume-unchanged files are invisible to status
	// and to `git worktree remove`'s own check.
	b, err = r.readGit(ctx, e.path, "ls-files", "-v", "-z")
	if err != nil {
		return HoldUnchecked, err.Error(), false
	}
	if hidden := hiddenChanges(string(b)); len(hidden) > 0 {
		return HoldHidden, strings.Join(hidden, ", "), false
	}
	// A nested repository inside an allowlisted ignored dir (node_modules/…)
	// is deleted with its unpushed commits.
	nested, err := nestedRepositories(ctx, e.path)
	if err != nil {
		return HoldUnchecked, err.Error(), false
	}
	if len(nested) > 0 {
		return HoldNested, strings.Join(nested, ", "), false
	}
	// Commits only this worktree can reach (its reflog, per-worktree refs,
	// detached HEAD) are garbage-collected once the worktree is removed.
	orphaned, err := r.unreachableTips(ctx, e)
	if err != nil {
		return HoldUnchecked, err.Error(), false
	}
	if orphaned {
		return HoldOrphans, "commits reachable only from this worktree's reflog, refs or detached HEAD", false
	}
	if !r.pushed(ctx, e.path, e.head, base) {
		return HoldUnpushed, "", false
	}
	held, err := holds(e.path)
	if err != nil {
		return HoldUnchecked, "in-use state unknown", false
	}
	if held {
		return HoldInUse, "a running process uses it", false
	}
	return "", "", false
}

const quarantinePrefix = ".forge-reclaim-"

// reclaimWorktree removes a worktree through a quarantine move. removed is
// false when it was kept, with the reason; the worktree is then back at its
// original path, or — if the move back failed — left in quarantine, still a
// valid worktree and never deleted.
func (r Runner) reclaimWorktree(ctx context.Context, repo string, e worktreeEntry, base string, idle time.Duration, self openfiles.Snapshot) (reason, detail string, removed bool) {
	// The move rewrites the worktree's .git file and admin gitdir, which would
	// read as fresh activity, so idleness is judged here, on the original path,
	// and not again after the move.
	if info, err := os.Stat(e.path); err != nil {
		return HoldUnchecked, "disappeared during cleanup", false
	} else if since := time.Since(worktreeActivity(e.path, info)); since < idle {
		return HoldActive, fmt.Sprintf("touched %s ago", since.Round(time.Minute)), false
	}
	q := filepath.Join(filepath.Dir(e.path), fmt.Sprintf("%s%s-%d", quarantinePrefix, filepath.Base(e.path), time.Now().Unix()))
	if _, err := os.Lstat(q); err == nil {
		return HoldRefused, q + " already exists", false
	}
	if _, err := r.command(ctx, "git", "-C", repo, "worktree", "move", e.path, q); err != nil {
		return HoldRefused, err.Error(), false
	}
	if r.afterQuarantine != nil {
		r.afterQuarantine(e.path, q)
	}
	moved := e
	moved.path = q
	reason, detail = r.recheckWorktree(ctx, repo, moved, base, 0, self)
	if reason == "" {
		// No --force: git refuses what it considers unsafe.
		if _, err := r.command(ctx, "git", "-C", repo, "worktree", "remove", q); err == nil {
			return "", "", true
		} else {
			reason, detail = HoldRefused, err.Error()
		}
	}
	// git moves a worktree INTO an existing destination directory, like mv,
	// which would nest it instead of failing; so an occupied path is a failure.
	var moveErr error
	if _, statErr := os.Lstat(e.path); statErr == nil {
		moveErr = fmt.Errorf("%s exists again", e.path)
	} else {
		_, moveErr = r.command(ctx, "git", "-C", repo, "worktree", "move", q, e.path)
	}
	if err := moveErr; err != nil {
		r.print("WARNING: worktree %s could not be moved back from quarantine %s: %v (it is still a valid worktree; nothing was deleted)\n", e.path, q, err)
		return HoldQuarantined, fmt.Sprintf("left at %s (%v); was kept because: %s", q, err, reason), false
	}
	return reason, detail, false
}

// maxOrphanTips bounds the argv handed to git; a worktree with more distinct
// commits in its reflog than this is held rather than guessed at.
const maxOrphanTips = 4000

// unreachableTips reports whether any commit this worktree alone can reach is
// absent from every branch, remote-tracking ref and tag.
func (r Runner) unreachableTips(ctx context.Context, e worktreeEntry) (bool, error) {
	tips := map[string]bool{e.head: true}
	if admin := worktreeAdminDir(e.path); admin != "" {
		if data, err := os.ReadFile(filepath.Join(admin, "logs", "HEAD")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				// "<old-sha> <new-sha> <ident>\t<message>": the new-sha is the tip.
				if fields := strings.Fields(line); len(fields) >= 2 && !isZeroSHA(fields[1]) {
					tips[fields[1]] = true
				}
			}
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	b, err := r.readGit(ctx, e.path, "for-each-ref", "--format=%(objectname)", "refs/worktree", "refs/bisect")
	if err != nil {
		return false, err
	}
	for _, sha := range strings.Fields(string(b)) {
		tips[sha] = true
	}
	if len(tips) > maxOrphanTips {
		return false, fmt.Errorf("worktree reflog names more than %d distinct commits; cannot check for orphans", maxOrphanTips)
	}
	args := []string{"rev-list", "--max-count=1", "--missing=allow-any"}
	for sha := range tips {
		args = append(args, sha)
	}
	args = append(args, "--not", "--branches", "--remotes", "--tags")
	out, err := r.readGit(ctx, e.path, args...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func isZeroSHA(sha string) bool { return strings.Trim(sha, "0") == "" }

// recheckWorktree re-reads the worktree's registration and re-runs the full
// classification with a fresh lsof snapshot. Any failure to re-check holds.
func (r Runner) recheckWorktree(ctx context.Context, repo string, e worktreeEntry, base string, idle time.Duration, self openfiles.Snapshot) (string, string) {
	b, err := r.readGit(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return HoldUnchecked, err.Error()
	}
	var fresh *worktreeEntry
	for _, cur := range parseWorktrees(string(b)) {
		if cur.path == e.path {
			c := cur
			fresh = &c
			break
		}
	}
	if fresh == nil || fresh.head == "" {
		return HoldUnchecked, "no longer registered"
	}
	// A fresh snapshot on purpose: this re-check exists to catch what changed
	// since the pass's snapshot, so it must not reuse it.
	var fresher lazyOpenFiles
	reason, detail, skip := r.classifyWorktree(ctx, *fresh, base, idle, self, func(p string) (bool, error) { return fresher.holds(ctx, p) })
	if skip {
		return HoldUnchecked, "disappeared during cleanup"
	}
	return reason, detail
}

// hiddenChanges lists files git was told to ignore changes to. -v tags: H
// tracked, S skip-worktree, lowercase = assume-unchanged (or both).
func hiddenChanges(lsFiles string) []string {
	var out []string
	for _, entry := range strings.Split(lsFiles, "\x00") {
		if len(entry) < 3 {
			continue
		}
		if tag := entry[0]; tag == 'S' || (tag >= 'a' && tag <= 'z') {
			out = append(out, entry[2:])
		}
	}
	return out
}

// maxNestedWalkEntries bounds the walk; a tree too large to inspect is held.
const maxNestedWalkEntries = 1_000_000

// nestedRepositories finds a `.git` entry anywhere under the worktree other
// than its own top-level one, including inside allowlisted ignored dirs.
// Submodules have one too, and are held with everything else (safe direction).
func nestedRepositories(ctx context.Context, worktree string) ([]string, error) {
	var found []string
	seen := 0
	err := filepath.WalkDir(worktree, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if seen++; seen%4096 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		if seen > maxNestedWalkEntries {
			return fmt.Errorf("worktree has more than %d entries; cannot check for nested repositories", maxNestedWalkEntries)
		}
		if d.Name() != ".git" || p == filepath.Join(worktree, ".git") {
			return nil
		}
		rel, _ := filepath.Rel(worktree, p)
		found = append(found, rel)
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return found, err
}

// lazyOpenFiles answers "does a running process use this path?" from one
// open-files snapshot. lsof costs seconds, so the snapshot is taken on the
// first question — only once a worktree has otherwise earned removal — and
// never again; a failure to take it is kept, so every later question gets the
// same answer and the caller reports it once.
type lazyOpenFiles struct {
	snap *openfiles.Snapshot
	err  error
	// take supplies the snapshot; the pass's shared one in a GC pass.
	take func(context.Context) (openfiles.Snapshot, error)
}

func (l *lazyOpenFiles) holds(ctx context.Context, path string) (bool, error) {
	if l.snap == nil && l.err == nil {
		take := l.take
		if take == nil {
			take = openfiles.Take
		}
		s, err := take(ctx)
		if err != nil {
			l.err = fmt.Errorf("refusing worktree cleanup: cannot determine which worktrees are in use: %w", err)
		} else {
			l.snap = &s
		}
	}
	if l.err != nil {
		return false, l.err
	}
	return l.snap.Holds(path), nil
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// defaultBase is origin/HEAD's target, else origin/main, else "".
func (r Runner) defaultBase(ctx context.Context, repo string) string {
	if b, err := r.readGit(ctx, repo, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(string(b)); ref != "" {
			return ref
		}
	}
	if _, err := r.readGit(ctx, repo, "rev-parse", "--verify", "--quiet", "origin/main^{commit}"); err == nil {
		return "origin/main"
	}
	return ""
}

// pushed reports whether head is safe on a remote: reachable from some
// remote-tracking ref, or an ancestor of the default base.
func (r Runner) pushed(ctx context.Context, worktree, head, base string) bool {
	if b, err := r.readGit(ctx, worktree, "for-each-ref", "--contains", head, "refs/remotes"); err == nil && strings.TrimSpace(string(b)) != "" {
		return true
	}
	if base == "" {
		return false
	}
	_, err := r.readGit(ctx, worktree, "merge-base", "--is-ancestor", head, base)
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

// readGit runs a read-only git command. --no-optional-locks matters: `git
// status` otherwise rewrites <admin>/index when its stat cache is stale, which
// would reset the very idle clock worktreeActivity reads.
func (r Runner) readGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return r.command(ctx, "git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
}
