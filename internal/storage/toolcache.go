package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/toolcache"
)

// toolCacheRoot is the shared tool cache this Runner maintains: ToolCacheRoot,
// else the real one ($FORGE_TOOLS_CACHE or <UserCacheDir>/forge/tools). Under
// `go test` an unset root is refused, the same rule and for the same reason
// as the source cache (sources.go): a test about something else must never
// reclaim the developer's real tools.
func (r Runner) toolCacheRoot() (string, error) {
	if r.ToolCacheRoot != "" {
		return r.ToolCacheRoot, nil
	}
	if testing.Testing() {
		return "", fmt.Errorf("storage.Runner.ToolCacheRoot is unset under test: " +
			"set it to a t.TempDir() (an unset root would reclaim the developer's real forge tool cache)")
	}
	return toolcache.Root()
}

// Tools reclaims entries of forge's shared tool cache that nothing has used
// for tool_cache_unused, beyond each tool's tool_cache_keep newest. Every
// pinned-version bump of a gate tool, golangci-lint or codegen plugin leaves
// the previous build behind, and nothing else ever removes one.
//
// The mechanism and its safety rules (the per-key lock, the re-check under it,
// the open-file scan, dating entries from before the last-use marker) live in
// internal/toolcache beside the code that installs the entries; the budgets
// come from this machine's storage policy like every other layer's.
//
// A cache that cannot be located is not a failure: there is nothing to reclaim.
func (r Runner) Tools(apply bool) error {
	root, err := r.toolCacheRoot()
	if err != nil {
		r.print("skip tool cache (%v)\n", err)
		return nil
	}
	// Validate rejects an unparseable value on a loaded policy; an unset one
	// means toolcache's default, never "evict everything".
	maxAge, parseErr := time.ParseDuration(r.Policy.ToolCacheUnused)
	if parseErr != nil {
		maxAge = 0
	}
	ctx := r.hostCtx()
	del := r.deletes()
	policy := toolcache.EvictPolicy{
		MaxAge: maxAge, Keep: r.Policy.ToolCacheKeep, Ctx: ctx,
		RemoveTree: func(path string) error { return del.removeTree(ctx, path) },
	}
	if r.openShared != nil || r.OpenPaths != nil {
		policy.InUse = r.sharedInUse(ctx)
	}
	got, err := toolcache.Evict(root, time.Now(), policy, apply, r.Out)
	if err != nil {
		if fsDistress(err) {
			err = del.observe(root, err)
		}
		if stopsPass(err) {
			r.print("tool cache: stopped (%v)\n", err)
		}
		return err
	}
	if len(got.Removed) > 0 {
		r.print("tool cache: %d entries, %.1f GiB (%d retained, %d of them in use)\n",
			len(got.Removed), float64(got.Bytes)/float64(GiB), got.Kept, len(got.Held))
	}
	return nil
}
