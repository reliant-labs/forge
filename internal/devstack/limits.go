package devstack

import (
	"fmt"
	"sync"
)

// The parallel-dev-stack ceiling armed for this process's subsequent
// allocations — see [SetMaxStacks]. It is a process-global, mirroring the
// Active/SetActive git-facts pair in options.go and the blockAlloc /
// UseBlockAllocator hook in internal/kclplugin: one process-wide value set
// ONCE before the first render, read by every allocation that follows.
//
// The zero value (0) is UNARMED, meaning unbounded — exactly the behavior
// every allocation had before this ceiling existed. That is deliberate, not
// just a convenient zero value: this package has no opinion on forge.yaml or
// its dev_stack.max_stacks default, so every existing devstack test, plus
// any caller that never calls SetMaxStacks (`forge env devstack port`,
// `forge ci`, `forge generate`), keeps allocating without limit exactly as
// before this change. Only the up/deploy render path
// (internal/cli/devstack_activate.go) arms a concrete ceiling, resolved from
// forge.yaml via config.DevStackConfig.EffectiveMaxStacks().
var (
	maxStacksMu sync.RWMutex
	maxStacks   int
)

// SetMaxStacks arms the ceiling for this process's subsequent NEW block
// allocations. n <= 0 disarms it (unbounded). Call once, before the first
// allocation, on the up/deploy path only — the same discipline SetActive
// already requires of its callers.
func SetMaxStacks(n int) {
	maxStacksMu.Lock()
	maxStacks = n
	maxStacksMu.Unlock()
}

// MaxStacksLimit returns the armed ceiling (0 meaning unbounded/unarmed). A
// project's cluster port pre-map (e.g. k3d.yaml's hand-mapped host-port
// range) should be derived from this SAME number rather than hand-
// maintained separately — the duplication between an unbounded allocator and
// a hand-maintained pre-map comment is the root cause this ceiling exists to
// close (see DevStackConfig in internal/config).
func MaxStacksLimit() int {
	maxStacksMu.RLock()
	defer maxStacksMu.RUnlock()
	return maxStacks
}

// checkCeiling refuses a NEW block at or past the armed ceiling. It must
// NEVER be consulted for a key that already holds a registry entry — an
// already-issued block resolves to its recorded port forever, regardless of
// where MaxStacks is set later. AllocateBlock and AllocatePortAvoidingForeign
// both return via lookupBlock, before ever reaching this, for exactly that
// reason.
func checkCeiling(key string, block int) error {
	limit := MaxStacksLimit()
	if limit <= 0 || block < limit {
		return nil
	}
	return fmt.Errorf(
		"refusing to allocate a NEW port block for key %q: this repo has already reached the "+
			"%d-stack ceiling (dev_stack.max_stacks in forge.yaml), counting %d stacks already "+
			"registered in .forge/blocks.json at the primary checkout.\n"+
			"\n"+
			"A block multiplies into a project's cluster port pre-map (e.g. k3d.yaml's hand-mapped\n"+
			"host-port range) and into the dev IdP's baked-in `iss` claim and registered redirect URIs.\n"+
			"Handing out one past the ceiling is how a stack allocates cleanly here and then fails much\n"+
			"later as an unrelated \"gateway unreachable\" — nowhere near this cause.\n"+
			"\n"+
			"Remedy, in order:\n"+
			"  1. `forge env devstack list` shows which worktrees hold the %d blocks. If a stack there is\n"+
			"     one you have finished with, remove its worktree (`git worktree remove <path>`) and run\n"+
			"     `forge env devstack prune` to reclaim the block.\n"+
			"  2. If every listed stack is genuinely still in use, this is NOT a stale-state problem and\n"+
			"     pruning will reclaim nothing — you need a wider ceiling. Raise dev_stack.max_stacks in\n"+
			"     forge.yaml, then run `forge generate` to REGENERATE the cluster port pre-map for the\n"+
			"     new range, then recreate the cluster so it forwards the new host ports. Raising the\n"+
			"     number alone renders ports the cluster never mapped.",
		key, limit, limit, limit)
}
