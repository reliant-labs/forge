package baseimage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// LockRel is the project-relative path of the base-image lock. Unlike the
// per-developer runtime state under .forge/state, the lock is a COMMITTED
// source-of-truth artifact: it pins every build (CI, every dev, a standalone
// `docker build` via the ARG defaults) to identical base bytes, so it must be
// shared. It lives under .forge/ for discoverability alongside forge's other
// project files; the consuming project's .gitignore carves it back IN with a
// `!.forge/base-images.lock.json` exception (the same pattern .forge/releases
// uses).
const LockRel = ".forge/base-images.lock.json"

// Lock is the committed pin set: each declared tag → its mirrored,
// digest-addressed reference. It is the single source of truth `forge build`
// reads to inject `--build-arg BASE_<slug>=<Ref>`, and the value a Dockerfile
// author copies into the `ARG BASE_<slug>=<default>` so a standalone
// `docker build` stays equivalently pinned.
type Lock struct {
	// MirrorPrefix is the prefix every Entry.Ref was resolved through,
	// recorded so a reader can see (and a re-pin can detect a change to) the
	// mirror without re-reading forge.yaml.
	MirrorPrefix string `json:"mirror_prefix"`
	// Entries is the pin set, keyed and sorted by upstream Tag for a stable
	// diff. One entry per declared base.
	Entries []LockEntry `json:"entries"`
}

// LockEntry is one pinned base image.
type LockEntry struct {
	// Tag is the upstream human reference declared in forge.yaml
	// (e.g. "alpine:3.21"). The stable key.
	Tag string `json:"tag"`
	// Arg is the Dockerfile build-arg name forge injects this pin under
	// (BASE_<slug>). Recorded so a human reading the lock can map a FROM line
	// back to its tag without re-deriving the slug.
	Arg string `json:"arg"`
	// Ref is the fully-pinned mirrored reference: <prefix>/<repo>@<digest>.
	// This is the build-arg VALUE and the Dockerfile ARG default.
	Ref string `json:"ref"`
	// Digest is the multi-arch INDEX digest (sha256:…) the tag resolved to
	// through the mirror, broken out for readability / auditing. Redundant
	// with the tail of Ref but cheap to carry.
	Digest string `json:"digest"`
}

// Entry returns the pin for a tag, or (zero, false) when absent.
func (l *Lock) Entry(tag string) (LockEntry, bool) {
	for _, e := range l.Entries {
		if e.Tag == tag {
			return e, true
		}
	}
	return LockEntry{}, false
}

// BuildArgs returns the `KEY=VALUE` build-arg strings for every locked entry,
// in stable (tag-sorted) order. The caller appends each as `--build-arg`. A
// nil/empty lock yields nil so the build path no-ops cleanly.
func (l *Lock) BuildArgs() []string {
	if l == nil {
		return nil
	}
	args := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		args = append(args, e.Arg+"="+e.Ref)
	}
	return args
}

// ReadLock loads the lock at <projectDir>/.forge/base-images.lock.json.
// A missing file returns (nil, nil) — the not-yet-pinned state, which the
// build path treats as "no base-image injection" and `--repin-bases` treats
// as "create it". A present-but-malformed file returns (nil, err).
func ReadLock(projectDir string) (*Lock, error) {
	path := filepath.Join(projectDir, LockRel)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read base-image lock %s: %w", path, err)
	}
	var lk Lock
	if err := json.Unmarshal(data, &lk); err != nil {
		return nil, fmt.Errorf("parse base-image lock %s: %w", path, err)
	}
	return &lk, nil
}

// WriteLock persists the lock, sorting entries by tag so the on-disk file has
// a stable, review-friendly diff regardless of declaration order. Creates
// .forge/ lazily. 0o644 — the lock is non-secret and committed.
func WriteLock(projectDir string, lk *Lock) (string, error) {
	path := filepath.Join(projectDir, LockRel)
	sort.Slice(lk.Entries, func(i, j int) bool { return lk.Entries[i].Tag < lk.Entries[j].Tag })
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, fmt.Errorf("create .forge dir: %w", err)
	}
	data, err := json.MarshalIndent(lk, "", "  ")
	if err != nil {
		return path, fmt.Errorf("marshal base-image lock: %w", err)
	}
	// Trailing newline so the committed file is POSIX-clean and re-pins that
	// change nothing produce no whitespace-only diff.
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return path, fmt.Errorf("write base-image lock %s: %w", path, err)
	}
	return path, nil
}

// LockMatchesDeclared reports whether an existing lock already pins EXACTLY
// the declared tag set through the declared mirror — i.e. a re-pin would only
// re-confirm the same digests. Used by the build path to decide whether the
// lock is stale relative to forge.yaml (declared a tag the lock is missing, or
// the mirror moved) and warn, without forcing a network resolve on every
// build. A nil lock against a non-empty declaration is "stale" (true→needs
// pin). Digests are NOT compared here (the lock is the source of truth for
// those); this only checks the tag SET and mirror agree.
func (l *Lock) LockMatchesDeclared(d Declared) bool {
	if l == nil {
		return len(d.Tags) == 0
	}
	if l.MirrorPrefix != d.MirrorPrefix {
		return false
	}
	have := map[string]bool{}
	for _, e := range l.Entries {
		have[e.Tag] = true
	}
	if len(have) != len(d.Tags) {
		return false
	}
	for _, t := range d.Tags {
		if !have[t] {
			return false
		}
	}
	return true
}
