// File: internal/cli/lint/lint_frontend_deps.go
//
// What the frontend lanes (eslint and typecheck) do about a frontend's
// dependencies: they read them, and never install them unless the run asks.
//
// ── Why lint does not install ─────────────────────────────────────────────
//
// The lanes used to install a frontend with no node_modules first (#540), so
// a fresh worktree linted instead of reporting "could not run". That made
// `forge lint` mutate the checkout it was judging: a read-only inventory of a
// project ran `pnpm install`, and the package manager's output ("Done in
// 415ms using pnpm v12.15.0") leaked through --quiet, which promises only
// what failed plus the verdict. A linter is a gate; it reports on the tree it
// is given. Preparing the tree is `forge env up`'s, `forge build`'s and the
// CI workflow's job — each installs before it lints (the generated ci.yml
// installs every frontend in its setup steps; `task ci`'s deps-<frontend> rows
// run before its lint rows).
//
// So a frontend whose dependencies are not installed is a lane that could
// NOT run (lint_skip.go): the lane is named in the verdict with the exact
// install command, and — a skipped check is not a passed one — it fails the
// run the same way a missing buf or compiler does, in CI and locally alike,
// unless the run accepts it (--allow-skip / lint.allow_skip) or drops the
// frontend lanes outright (--skip-frontends). One policy for every lane that
// could not run, rather than a second, frontend-only, CI-only mode.
//
// `forge lint --install` keeps the old behaviour reachable: it installs what
// is missing (and refreshes a tree older than its lockfile) with the
// frontend's own package manager, frozen to the lockfile, before the lanes
// run. Its progress and the package manager's output go to stderr, and
// nowhere under --quiet; a failed install still reports the package
// manager's output, in the lane's could-not-run finding.
//
// A tree that IS installed but older than its lockfile or package.json is
// linted as it stands — failing a gate on an mtime heuristic would turn every
// branch switch red — with an info note naming the command that brings it
// current.

package lint

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/reliant-labs/forge/internal/frontenddeps"
)

// frontendDepsPolicy is how one run's frontend lanes treat dependencies.
type frontendDepsPolicy struct {
	// install is `forge lint --install`: install missing dependencies (and
	// refresh stale ones) before a lane runs. Off, lint never installs.
	install bool
	// out receives an opted-in install's progress and the package
	// manager's output; nil discards it (--quiet).
	out io.Writer
}

// frontendDeps is the run's dependency policy.
func (rc *lintRunCtx) frontendDeps() frontendDepsPolicy {
	return frontendDepsPolicy{install: rc.install, out: rc.installOut}
}

// installFrontendDeps is the opted-in install: the frontend's own package
// manager, frozen to its lockfile. A variable so tests observe the decision
// without a package manager.
var installFrontendDeps = func(ctx context.Context, out io.Writer, name, dir string) error {
	return frontenddeps.EnsureTo(ctx, out, "[lint]", name, dir, "", true)
}

// frontendInstallMu serializes opted-in installs: the typecheck lane runs its
// frontends concurrently, and two members of one workspace install at the
// same root.
var frontendInstallMu sync.Mutex

// frontendDepsState is what a lane may do with a frontend's dependencies.
type frontendDepsState struct {
	// missing: no node_modules in the frontend or any parent directory, so
	// the lane could NOT run. reason completes "<frontend>: <reason>"; fix
	// is the command that installs them.
	missing     bool
	reason, fix string
	// note, when set, is an info line for a lane that runs anyway (a stale
	// tree, or a refresh --install could not complete).
	note string
}

// check decides, for the lane `tool` ("eslint", "typecheck"), whether the
// frontend at dir has its dependencies. root bounds the walk up to a
// workspace root that hoists them (anyAncestorHasNodeModules).
func (p frontendDepsPolicy) check(ctx context.Context, name, dir, root, tool string) frontendDepsState {
	installed := anyAncestorHasNodeModules(dir, root)
	// Staleness is a question about the frontend's OWN node_modules: a
	// hoisted workspace member has none, and the root's is the workspace's
	// to keep current.
	stale := dirExists(filepath.Join(dir, "node_modules")) && frontenddeps.Stale(dir)
	if p.install && (!installed || stale) {
		frontendInstallMu.Lock()
		err := installFrontendDeps(ctx, p.out, name, dir)
		frontendInstallMu.Unlock()
		if err != nil {
			if !installed {
				return frontendDepsState{
					missing: true,
					reason:  fmt.Sprintf("dependencies are not installed and installing them (--install) failed — %s did NOT run: %v", tool, err),
					fix:     npmInstallFix(dir),
				}
			}
			return frontendDepsState{note: fmt.Sprintf("%s: refreshing its stale node_modules (--install) failed, so %s ran against the installed tree: %v", name, tool, err)}
		}
		installed = anyAncestorHasNodeModules(dir, root)
		stale = false
	}
	if !installed {
		return frontendDepsState{
			missing: true,
			reason: fmt.Sprintf("dependencies are not installed (node_modules not found in %s or any parent directory) — %s did NOT run; forge lint does not install them",
				filepath.ToSlash(dir), tool),
			fix: depsNotInstalledFix(dir),
		}
	}
	if stale {
		return frontendDepsState{note: fmt.Sprintf("%s: node_modules is older than its lockfile or package.json, so %s ran against the installed tree — %s",
			name, tool, depsNotInstalledFix(dir))}
	}
	return frontendDepsState{}
}

// depsNotInstalledFix is the remediation when lint found no dependencies: the
// frontend's own install command, or the flag that has lint run it.
func depsNotInstalledFix(dir string) string {
	return npmInstallFix(dir) + " — or run `forge lint --install`, which installs them first"
}

// installOutput is where an opted-in install reports: stderr for a human run
// (stdout carries the verdict, and --json's report), nowhere under --quiet,
// which prints only what failed plus the verdict.
func installOutput(quiet bool) io.Writer {
	if quiet {
		return io.Discard
	}
	return os.Stderr
}
