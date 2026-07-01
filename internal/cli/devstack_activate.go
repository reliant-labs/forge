package cli

import (
	"fmt"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/devstack"
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// activateDevStack arms the render-context globals BEFORE the first render so
// that every render path (entity AND manifest) sees the same parallel-dev-
// stack inputs under BOTH `forge up` and `forge deploy`:
//
//   - devstack.SetActive — pushes option("worktree") + option("branch") into
//     KCL (the raw git facts; the KCL author decides which to key on).
//   - kclplugin.UseBlockAllocator — backs forge.allocate_port(base, key) with
//     the lock-guarded, persistent block registry (.forge/blocks.json), so a
//     keyed port resolves to the SAME deterministic base+block*100 under up
//     and deploy. This is the up-vs-deploy port fix, now via the block
//     registry instead of a per-instance port store.
//   - kclplugin.UsePortStore — keeps the GENERAL resolve_port primitive
//     stable across runs and identical under up/deploy via the historical
//     .forge/ports-<env>.json store (resolve_port is still availability-
//     stepping, so its store remains the source of truth).
//
// It returns a restore func that reverts the resolve_port store to its
// pre-render bytes — the up path calls it when its already-running guard
// rejects a render so a rejected attempt can't drift the stable resolve_port
// assignments. Deploy ignores it (an applied render's ports ARE the truth).
//
// On the primary checkout with no worktree, option("worktree") is "" so a
// KCL that keys on it composes the DEFAULT stack — historical names and
// allocate_port(base, "") == base — byte-identical to before this primitive.
func activateDevStack(projectDir, env string) (devstack.Options, func()) {
	opts := devstack.Resolve(projectDir)
	devstack.SetActive(opts)

	// Back allocate_port with the persistent, lock-guarded block registry.
	kclplugin.UseBlockAllocator(func(base int, key string) (int, error) {
		return devstack.AllocatePort(projectDir, base, key)
	})

	// Keep resolve_port stable + up==deploy via the per-env store.
	storePath := filepath.Join(projectDir, ".forge", "ports-"+env+".json")
	restore := kclplugin.UsePortStore(storePath)

	if opts.Worktree != "" || opts.Branch != "" {
		fmt.Printf("[devstack] worktree=%q branch=%q\n", opts.Worktree, opts.Branch)
	}
	return opts, restore
}
