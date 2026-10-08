package buildtarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/reliant-labs/forge/pkg/release"
)

// Source is the git checkout a ShellBuild's command ran in, captured just
// before the command started.
//
// It is what ties a ShellBuild's image to a commit. A ShellBuild whose cwd is
// a SIBLING checkout (`cwd = "../reliant"`) builds whatever that checkout
// holds, and the project's own commit says nothing about it — which is how a
// release once shipped a days-old sibling build while go.mod pinned a newer
// commit. Recording the checkout's HEAD and dirty flag beside the digest is
// what lets a release be checked against the commit the project pins, and lets
// a reader answer "which commit is in this image" without rebuilding it.
//
// It describes the directory the command RAN IN. A command that exports some
// other commit (`git archive <sha>`) and builds that is invisible to forge;
// this is the checkout it was handed.
type Source struct {
	// Dir is the checkout's top-level directory on this machine. Local
	// state only: it is never copied into a release.
	Dir string `json:"dir"`
	// Repo is the canonical origin remote ("github.com/org/repo"); "" when
	// the checkout has no origin.
	Repo string `json:"repo,omitempty"`
	// Module is the Go module the checkout declares — the go.mod nearest the
	// cwd, at or below the top level — or "" when there is none. It is how a
	// sibling checkout is matched to the project's go.mod require.
	Module string `json:"module,omitempty"`
	// ModuleDir is that go.mod's directory relative to Dir ("." at the top
	// level). A module in a subdirectory tags its versions "<dir>/vX.Y.Z".
	ModuleDir string `json:"module_dir,omitempty"`
	// Commit is HEAD, full hex.
	Commit string `json:"commit"`
	// Dirty: tracked or untracked-unignored changes existed when the command
	// started — the same rule release provenance applies to the project.
	Dirty bool `json:"dirty"`
	// Project is true when the checkout is the project's OWN repository (a
	// ShellBuild with no cwd, or `cwd = "."`). Its commit is then the
	// release's own, which release provenance already records and checks.
	Project bool `json:"project,omitempty"`
}

// External reports whether the checkout is a repository other than the
// project's — a sibling whose commit the project must pin.
func (s *Source) External() bool { return s != nil && !s.Project }

// ForRelease is the part of the source a release ledger records: no path,
// because a release is shared beyond this machine.
func (s *Source) ForRelease() *release.BuildSource {
	if s == nil || s.Commit == "" {
		return nil
	}
	return &release.BuildSource{Repo: s.Repo, Commit: s.Commit, Dirty: s.Dirty}
}

// ResolveCommit resolves rev (a tag, a ref) to a full commit in this
// checkout's object store, and fails when the checkout does not have it — a
// tag nobody fetched is a different problem from a checkout on the wrong
// commit, and the two get different fixes.
func (s *Source) ResolveCommit(ctx context.Context, rev string) (string, error) {
	return gitLine(ctx, s.Dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// CaptureSource reports the git checkout dir belongs to.
//
// (nil, nil) means dir is not inside a git work tree, or the repository has no
// commit yet: a ShellBuild over a plain directory has no commit to record, and
// that is not an error. An error means git itself could not answer — missing
// from PATH, or a status that failed — which a caller guarding a release must
// not mistake for "nothing to check".
func CaptureSource(ctx context.Context, dir, projectDir string) (*Source, error) {
	top, err := gitLine(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("capture the source checkout of %s: %w", dir, err)
		}
		return nil, nil
	}
	head, err := gitLine(ctx, top, "rev-parse", "HEAD")
	if err != nil {
		return nil, nil
	}
	// --no-optional-locks: the checkout is usually someone else's working
	// copy (a sibling repo another process is using), and a status that
	// refreshes the index takes a lock it has no business holding.
	status, err := gitLine(ctx, top, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("capture the source checkout of %s: %w", dir, err)
	}
	src := &Source{Dir: top, Commit: head, Dirty: status != ""}
	if url, err := gitLine(ctx, top, "remote", "get-url", "origin"); err == nil {
		src.Repo = release.CanonicalRepo(url)
	}
	src.Module, src.ModuleDir = nearestModule(dir, top)
	if projectTop, err := gitLine(ctx, projectDir, "rev-parse", "--show-toplevel"); err == nil {
		src.Project = projectTop == top
	}
	return src, nil
}

// nearestModule returns the module path declared by the go.mod nearest dir,
// searching upward no further than top, and that go.mod's directory relative
// to top. ("", "") when there is none.
func nearestModule(dir, top string) (module, moduleDir string) {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		d = dir
	}
	if t, err := filepath.EvalSymlinks(top); err == nil {
		top = t
	}
	for {
		if data, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil { //nolint:gosec // a go.mod inside the build's own checkout
			if mod := modfile.ModulePath(data); mod != "" {
				rel, rerr := filepath.Rel(top, d)
				if rerr != nil {
					rel = "."
				}
				return mod, filepath.ToSlash(rel)
			}
		}
		parent := filepath.Dir(d)
		if d == top || parent == d {
			return "", ""
		}
		d = parent
	}
}

// gitLine runs git in dir and returns its trimmed stdout.
func gitLine(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
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
