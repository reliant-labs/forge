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

// ResolveName derives the raw (pre-sanitization) instance name in priority
// order:
//
//  1. an explicit --instance flag value (flagName), when non-empty;
//  2. else the git worktree directory basename, when this checkout is a
//     LINKED worktree (a separate working dir off the main repo) — the
//     common "one stack per worktree" case;
//  3. else the current git branch name.
//
// It returns the SANITIZED name (possibly "" → default). The git lookups
// are best-effort: outside a repo, or on a detached HEAD with no worktree,
// the chain simply yields "".
func ResolveName(projectDir, flagName string) string {
	if s := Sanitize(flagName); s != "" {
		return s
	}
	if name := worktreeName(projectDir); name != "" {
		if s := Sanitize(name); s != "" {
			return s
		}
	}
	if br := gitBranch(projectDir); br != "" {
		if s := Sanitize(br); s != "" {
			return s
		}
	}
	return ""
}

// worktreeName returns the basename of projectDir when it is a LINKED git
// worktree (not the primary checkout), else "". A linked worktree's top
// level holds a `.git` FILE (a gitdir pointer) rather than a directory;
// the primary checkout holds a `.git` directory. Keying on "is this a
// linked worktree?" means a plain single-checkout repo doesn't silently
// acquire an instance from its own directory name — only the deliberate
// multi-worktree workflow does.
func worktreeName(projectDir string) string {
	root := gitToplevel(projectDir)
	if root == "" {
		return ""
	}
	info, err := os.Stat(filepath.Join(root, ".git"))
	if err != nil || info.IsDir() {
		return "" // primary checkout (or no .git) — not a linked worktree
	}
	return filepath.Base(root)
}

func gitToplevel(dir string) string {
	return gitOut(dir, "rev-parse", "--show-toplevel")
}

func gitBranch(dir string) string {
	br := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if br == "HEAD" { // detached
		return ""
	}
	return br
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
