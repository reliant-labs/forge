package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Capture budget for the dirty-tree hash (F-16). Past either bound the tree is
// recorded as unhashed rather than making a developer wait: a clean tree's
// hash is HEAD^{tree} and costs nothing, so the bound only ever applies to a
// dirty checkout, which no protected path relies on.
const (
	CaptureTreeTimeout  = 5 * time.Second
	CaptureMaxDirtySize = 2 << 30 // bytes of untracked-unignored + modified content
)

// CaptureOptions are the facts CaptureProvenance cannot learn from git.
type CaptureOptions struct {
	// ForgeVersion is the rendering binary's version.
	ForgeVersion string
	// WorktreeKey is forge's devstack key for the checkout ("" = primary).
	WorktreeKey string
	// Host names the machine (a daemon id, or HostID()). Never a raw
	// hostname.
	Host string
	// Attestation is the CI identity, when there is one.
	Attestation *Attest
}

// CaptureProvenance records where the content in dir came from. It is
// best-effort in exactly one way: a directory that is not a git checkout
// yields a Provenance with no commit (and an error only when git itself is
// broken in a way the caller should hear about). It NEVER touches the user's
// index: the tree hash is computed through a temporary one.
//
// Exported so the daemon and CI capture provenance with the same code forge
// does — the field set is ledger vocabulary every backend must agree on.
func CaptureProvenance(ctx context.Context, dir string, opts CaptureOptions) (Provenance, error) {
	p := Provenance{
		ForgeVersion: opts.ForgeVersion,
		Attestation:  opts.Attestation,
		Worktree:     Worktree{Key: opts.WorktreeKey, Host: opts.Host},
	}
	top, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		// Not a git checkout: nothing more to record, and not an error.
		p.Worktree.Label = filepath.Base(dir)
		p.Worktree.Path = dir
		return p, nil
	}
	p.Worktree.Path = top

	commit, err := gitOut(ctx, top, "rev-parse", "HEAD")
	if err != nil {
		// A repository with no commits yet.
		p.Worktree.Label = filepath.Base(top)
		return p, nil
	}
	p.Commit = commit
	if b, err := gitOut(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		p.Branch = b
	}
	if t, err := gitOut(ctx, top, "describe", "--tags", "--exact-match", "HEAD"); err == nil {
		p.Tag = t
	}
	p.Worktree.Label = p.Branch
	if p.Worktree.Label == "" {
		p.Worktree.Label = filepath.Base(top)
	}
	p.Repo = canonicalRemote(ctx, top)

	status, err := gitOut(ctx, top, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return p, fmt.Errorf("capture provenance: git status: %w", err)
	}
	p.Dirty = status != ""
	if !p.Dirty {
		tree, err := gitOut(ctx, top, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return p, fmt.Errorf("capture provenance: resolve HEAD tree: %w", err)
		}
		p.Tree = tree
		return p, nil
	}
	if dirtySize(top, status) > CaptureMaxDirtySize {
		return p, nil // unhashed: Provenance.Unhashed reports it
	}
	tree, err := dirtyTree(ctx, top)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return p, nil // unhashed, by budget
		}
		return p, fmt.Errorf("capture provenance: hash working tree: %w", err)
	}
	p.Tree = tree
	return p, nil
}

// dirtyTree hashes exactly what is in the working tree — tracked changes and
// untracked-unignored files — through a TEMPORARY index, so the user's staged
// state is never read or written. Two captures of the same dirty state give
// the same tree.
func dirtyTree(ctx context.Context, top string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CaptureTreeTimeout)
	defer cancel()
	tmp, err := os.CreateTemp("", "forge-provenance-index-*")
	if err != nil {
		return "", err
	}
	idx := tmp.Name()
	_ = tmp.Close()
	// git refuses to read an empty file as an index; it must not exist.
	_ = os.Remove(idx)
	defer os.Remove(idx)
	env := append(os.Environ(), "GIT_INDEX_FILE="+idx)
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"add", "-A"}} {
		if _, err := gitOutEnv(ctx, top, env, args...); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
	}
	out, err := gitOutEnv(ctx, top, env, "write-tree")
	if err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	return out, err
}

// dirtySize estimates the bytes `git add -A` would hash: the sizes of every
// path git status reports. Deleted paths cost nothing.
func dirtySize(top, porcelain string) int64 {
	var total int64
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		if fi, err := os.Stat(filepath.Join(top, path)); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		if total > CaptureMaxDirtySize {
			return total
		}
	}
	return total
}

// canonicalRemote returns origin's URL as "host/owner/repo": no scheme, no
// credentials, no ".git". "" when there is no origin.
func canonicalRemote(ctx context.Context, top string) string {
	url, err := gitOut(ctx, top, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return CanonicalRepo(url)
}

// CanonicalRepo normalizes a git remote URL to "host/owner/repo", so the SSH
// and HTTPS spellings of one repository compare equal and no credential in a
// URL is ever recorded.
func CanonicalRepo(url string) string {
	u := strings.TrimSpace(url)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if at := strings.Index(u, "@"); at >= 0 && at < strings.Index(u+"/", "/") {
			u = u[at+1:]
		}
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") {
		// scp-like: git@github.com:owner/repo
		u = strings.Replace(u[at+1:], ":", "/", 1)
	}
	if i := strings.Index(u, "/"); i >= 0 {
		host := u[:i]
		if c := strings.LastIndex(host, ":"); c >= 0 {
			host = host[:c] // drop a port
		}
		u = strings.ToLower(host) + u[i:]
	}
	return u
}

// HostID is a stable, non-identifying machine id: the first 16 hex of
// sha256(hostname). Used when no daemon id is available.
func HostID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(h))
	return hex.EncodeToString(sum[:])[:16]
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutEnv(ctx, dir, nil, args...)
}

func gitOutEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
