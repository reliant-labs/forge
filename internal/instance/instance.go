// Package instance owns forge's declarative instance-parameterization
// primitive: the identity that lets N dev stacks (one per git worktree)
// run in parallel against shared clusters without colliding.
//
// An "instance" is a short DNS-safe name (e.g. a worktree basename) plus a
// stable small integer index. forge resolves the name once per command,
// assigns/looks-up its index in a lock-guarded registry, and PUSHES both
// into KCL as options:
//
//	option("instance")        -> the sanitized name (str; "" when default)
//	option("instance_index")  -> the stable index   (int; 0  when default)
//
// KCL composes these deterministically (namespace suffix, per-instance DB
// name, NATS subject prefix, port block = base + index*100, …). The default
// (no instance) renders byte-identical to today: option("instance") == ""
// and option("instance_index") == 0, and NO -D args are emitted for them
// (so a plain `kcl run` is unchanged and the options yield their KCL
// defaults).
//
// The same Instance also scopes the resolve_port store
// (.forge/ports-<env>[-<instance>].json) so each instance gets its own
// non-colliding port block — see PortStorePath.
package instance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Instance is a resolved instance identity. The zero value is the default
// (no instance) — Name "" / Index 0 — and renders byte-identically to a
// stack with no instance parameterization.
type Instance struct {
	Name  string // sanitized, DNS-safe; "" for the default instance
	Index int    // stable small int from the registry; 0 for the default
}

// IsDefault reports whether this is the unnamed default instance.
func (i Instance) IsDefault() bool { return i.Name == "" }

// maxNameLen bounds a sanitized instance name. Instance names feed
// k8s namespace suffixes, DB names, and NATS subject prefixes — all of
// which have length ceilings (a k8s namespace is 63 chars and already
// carries a project+env prefix), so we keep the instance segment short.
const maxNameLen = 24

var (
	nonDNS      = regexp.MustCompile(`[^a-z0-9-]+`)
	dashRuns    = regexp.MustCompile(`-+`)
	leadTrailDA = regexp.MustCompile(`^-+|-+$`)
)

// Sanitize lowercases s and reduces it to a DNS-safe label: [a-z0-9-],
// collapsed dash runs, no leading/trailing dash, bounded length. Returns
// "" when nothing survives (e.g. an all-symbol branch name) — the caller
// treats that as "no instance" and falls back to the default stack.
func Sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonDNS.ReplaceAllString(s, "-")
	s = dashRuns.ReplaceAllString(s, "-")
	s = leadTrailDA.ReplaceAllString(s, "")
	if len(s) > maxNameLen {
		s = s[:maxNameLen]
		s = leadTrailDA.ReplaceAllString(s, "")
	}
	return s
}

// ResolveName derives the sanitized instance name in priority order:
//
//  1. an explicit --instance flag value (flagName), when non-empty;
//  2. else the git worktree directory basename, but ONLY when this checkout
//     is a LINKED worktree (a `git worktree add`'ed working dir) — the
//     "one stack per worktree" case.
//
// There is deliberately NO branch fallback: the PRIMARY checkout on ANY
// branch resolves to "" (the default instance), so a plain
// `forge up`/`deploy` is byte-identical to today regardless of which
// feature branch you're on. The unit of parallelism is the worktree, not
// the branch. Returns "" (default) outside a repo or on the primary
// checkout.
func ResolveName(projectDir, flagName string) string {
	if s := Sanitize(flagName); s != "" {
		return s
	}
	if name := worktreeName(projectDir); name != "" {
		if s := Sanitize(name); s != "" {
			return s
		}
	}
	return ""
}

// worktreeName returns the basename of projectDir when it is a LINKED git
// worktree (a `git worktree add`'ed checkout), else "".
//
// The parallelism unit is the WORKTREE, never the branch: branches change
// constantly and the PRIMARY checkout must always render DEFAULT (so a
// plain `forge up`/`deploy` on any feature branch is byte-identical to
// today). We therefore key strictly on "is this a linked worktree?".
//
// Detection: a linked worktree's per-worktree git dir
// (`git rev-parse --git-dir` → …/.git/worktrees/<name>) differs from the
// repo's common dir (`--git-common-dir` → the primary's …/.git). The
// primary checkout has them equal. This is git's own authoritative
// distinction — far more robust than sniffing whether `.git` is a file vs
// a directory (which submodules and some tooling also make a file).
func worktreeName(projectDir string) string {
	gitDir := gitOut(projectDir, "rev-parse", "--absolute-git-dir")
	commonDir := gitOut(projectDir, "rev-parse", "--git-common-dir")
	if gitDir == "" || commonDir == "" {
		return "" // not a git checkout
	}
	// Normalize commonDir to absolute so the comparison is exact:
	// --git-common-dir can come back relative (e.g. ".git"), and it is
	// relative to the command's working dir (projectDir), NOT the process
	// cwd — so resolve it against projectDir, not via filepath.Abs.
	if !filepath.IsAbs(commonDir) {
		base := projectDir
		if base == "" {
			if wd, err := os.Getwd(); err == nil {
				base = wd
			}
		}
		commonDir = filepath.Join(base, commonDir)
	}
	if sameDir(gitDir, commonDir) {
		return "" // primary checkout — DEFAULT instance, regardless of branch
	}
	// Linked worktree. Use the worktree's working-tree basename as the name.
	if root := gitOut(projectDir, "rev-parse", "--show-toplevel"); root != "" {
		return filepath.Base(root)
	}
	return ""
}

// sameDir reports whether two paths point at the same directory, tolerant
// of symlinks (macOS /var vs /private/var) and trailing-slash differences.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea == nil && eb == nil {
		return filepath.Clean(ra) == filepath.Clean(rb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// PortStorePath is the resolve_port store file for (env, instance). The
// DEFAULT instance keeps the historical path .forge/ports-<env>.json so a
// non-instance dev loop is byte-identical; a named instance gets its own
// file .forge/ports-<env>-<instance>.json so its port block never collides
// with another worktree's.
func PortStorePath(projectDir, env string, inst Instance) string {
	name := "ports-" + env
	if !inst.IsDefault() {
		name += "-" + inst.Name
	}
	return filepath.Join(projectDir, ".forge", name+".json")
}

// dargFor returns the `-D` KCL option binding for an int option, or "" to
// omit it (so the default render emits no extra args).
func dargInt(key string, v int) string { return key + "=" + strconv.Itoa(v) }

// DArgs returns the `-D key=value` KCL option bindings that push this
// instance INTO the render, threaded onto every render path (entity +
// manifest). It is the extension of the existing namespace/image_tag
// option seam:
//
//	option("instance")        = "<name>"   (str; QUOTED so an all-digit
//	                                         name stays str, never int)
//	option("instance_index")  = <index>    (int)
//
// The DEFAULT instance returns NO args: a plain render then sees
// option("instance") == None (KCL default "") and option("instance_index")
// == None (KCL default 0), so existing single-stack envs render exactly as
// before. Quoting matches renderDArgs' contract for forge-controlled
// string options.
func (i Instance) DArgs() []string {
	if i.IsDefault() {
		return nil
	}
	return []string{
		"instance=" + strconv.Quote(i.Name),
		dargInt("instance_index", i.Index),
	}
}

func (i Instance) String() string {
	if i.IsDefault() {
		return "default"
	}
	return fmt.Sprintf("%s(#%d)", i.Name, i.Index)
}
