// Package toolcache is the one machine-wide directory every tool forge
// installs for itself lives in: the gate's pinned linters and scanners
// (internal/cigate), forge lint's golangci-lint build (internal/cli/lint), and
// the codegen plugins and air that `forge generate` and `forge env up` pin
// (internal/cli). Every worktree of every project on the machine shares it.
//
// # Layout
//
//	$FORGE_TOOLS_CACHE (default <UserCacheDir>/forge/tools)/
//	  <kind>/                      gate, golangci-lint, pinned
//	    <key>/                     one ENTRY: a tool at one version (+toolchain)
//	      <binary>
//	      .forge-tool              last-use marker (its mtime)
//	    .locks/<key>.lock          flock(2) per key; never deleted
//	    .build-<key>-*/            an install in progress
//	    .forge-evicting-<key>-*/   an entry being removed by Evict
//
// An entry's key names everything that decides its bytes (tool, version and,
// where it matters, the Go toolchain), so an entry is never rewritten for a
// different version: a bump is a new key beside the old one, and a checkout
// still on the old pin keeps working.
//
// # Concurrency
//
// Many forge processes, from many worktrees, resolve the same key at once:
// every fresh project's first generate pins protoc-gen-go, and parallel gate
// runs pin golangci-lint. Ensure serializes them per key: a reader checks the
// entry under a SHARED lock, and an installer builds under an EXCLUSIVE one
// and re-checks first, so N concurrent misses run one build and N-1 waits
// instead of N builds. The install itself is published by renaming a complete
// staging directory into place, so nobody — with or without the lock — can
// exec a half-written binary.
//
// # Last use, and eviction
//
// Ensure touches the entry's .forge-tool marker on every resolution. Evict
// (forge storage gc's tool-cache layer) removes entries unused for longer
// than the policy allows, beyond the newest few of each tool. It takes the
// key's exclusive lock and re-reads the marker before it renames an entry
// away, so an entry being resolved at that moment is either seen as freshly
// used, or gone before the resolver looks (which then reinstalls it).
//
//forge:exclude-contract: a filesystem cache with package-level operations, not an injected application component
package toolcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvRoot relocates the whole tool cache: a read-only home directory, a CI
// runner that caches one directory, or a test that must not touch the
// developer's real cache.
const EnvRoot = "FORGE_TOOLS_CACHE"

// The kinds of entry forge installs. Each is a directory under the root.
const (
	// KindGate holds `forge ci gate`'s pinned tools (internal/cigate).
	KindGate = "gate"
	// KindGolangci holds forge lint's own golangci-lint builds.
	KindGolangci = "golangci-lint"
	// KindPinned holds go.mod-pinned codegen plugins and the dev loop's air.
	KindPinned = "pinned"
)

// kinds lists every kind Evict maintains.
var kinds = []string{KindGate, KindGolangci, KindPinned}

const (
	// UsedMarker is the file whose mtime records an entry's last use.
	UsedMarker = ".forge-tool"
	// lockDir holds one lock file per key, beside the entries.
	lockDir = ".locks"
	// stagingPrefix names an install in progress.
	stagingPrefix = ".build-"
	// evictingPrefix names an entry Evict has renamed out of the cache.
	evictingPrefix = ".forge-evicting-"
)

// lockPoll is how often a held lock is retried while waiting on it.
var lockPoll = 50 * time.Millisecond

// Root is the tool cache root: $FORGE_TOOLS_CACHE, else
// <UserCacheDir>/forge/tools.
func Root() (string, error) {
	if dir := os.Getenv(EnvRoot); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the user cache directory for forge's tool cache: %w (set %s)", err, EnvRoot)
	}
	return filepath.Join(base, "forge", "tools"), nil
}

// KindDir is the directory holding one kind of entry.
func KindDir(kind string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, kind), nil
}

// Ensure resolves the entry <kindDir>/<key>, installing it when ready reports
// it unusable, and returns its directory.
//
// ready(dir) answers whether the entry at dir is usable as is (the binary
// exists and reports the pinned version). build(ctx, stage) installs the
// entry's files into stage, an empty directory that becomes the entry when
// build succeeds. build runs at most once across every concurrent caller that
// missed; the others wait for it and use its result. Ensure does not re-check
// ready after a build: a caller that wants its own message for a build that
// produced the wrong thing verifies the returned directory itself.
func Ensure(ctx context.Context, kindDir, key string, ready func(dir string) bool, build func(ctx context.Context, stage string) error) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(kindDir, lockDir), 0o755); err != nil {
		return "", err
	}
	dir := filepath.Join(kindDir, key)
	lock := lockPath(kindDir, key)

	hit := false
	if err := withLock(ctx, lock, false, func() error {
		if hit = ready(dir); hit {
			markUsed(dir)
		}
		return nil
	}); err != nil {
		return "", err
	}
	if hit {
		return dir, nil
	}
	if err := withLock(ctx, lock, true, func() error {
		// Another process installed it while this one waited.
		if ready(dir) {
			markUsed(dir)
			return nil
		}
		return publish(ctx, kindDir, key, build)
	}); err != nil {
		return "", err
	}
	return dir, nil
}

// publish builds into a staging directory and renames it into place. The
// caller holds the key's exclusive lock.
func publish(ctx context.Context, kindDir, key string, build func(context.Context, string) error) error {
	stage, err := os.MkdirTemp(kindDir, stagingPrefix+key+"-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := build(ctx, stage); err != nil {
		return err
	}
	// MkdirTemp makes 0700; the cache may be shared by several users.
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := touch(filepath.Join(stage, UsedMarker), time.Now()); err != nil {
		return err
	}
	dir := filepath.Join(kindDir, key)
	if _, err := os.Lstat(dir); err == nil {
		// An entry ready() rejected: a corrupt or wrong build, or one from a
		// forge that wrote binaries into the entry in place. Nothing can be
		// installing it (we hold the lock); move it aside first, so the
		// rename below never lands INSIDE it.
		trash := filepath.Join(kindDir, fmt.Sprintf("%s%s-%d", evictingPrefix, key, time.Now().UnixNano()))
		if err := os.Rename(dir, trash); err != nil {
			return fmt.Errorf("replace unusable tool cache entry %s: %w", dir, err)
		}
		_ = removeAllWritable(trash)
	}
	return os.Rename(stage, dir)
}

// markUsed records that the entry at dir was just used. A failure costs at
// most one reinstall, so it is not reported.
func markUsed(dir string) {
	_ = touch(filepath.Join(dir, UsedMarker), time.Now())
}

// touch sets path's mtime to now, creating it empty when missing. Only the
// mtime is written, so an interrupted touch cannot damage anything.
func touch(path string, now time.Time) error {
	if err := os.Chtimes(path, now, now); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(path, now, now)
}

func lockPath(kindDir, key string) string {
	return filepath.Join(kindDir, lockDir, key+".lock")
}

// validKey rejects a key that is not one path element of an entry name.
func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, ".") || strings.ContainsAny(key, `/\`) || key != filepath.Base(key) {
		return fmt.Errorf("toolcache: invalid entry key %q", key)
	}
	return nil
}

// withLock runs fn holding path's lock, exclusive or shared, waiting for it
// until ctx ends. The lock is released when fn returns.
func withLock(ctx context.Context, path string, exclusive bool, fn func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		ok, err := tryLock(f, exclusive)
		if err != nil {
			return fmt.Errorf("lock %s: %w", path, err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", path, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
	defer unlock(f)
	return fn()
}
