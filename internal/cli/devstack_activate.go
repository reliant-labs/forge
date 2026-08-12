package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/devstack"
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// activateDevStack arms the render-context globals BEFORE the first render so
// that every render path (entity AND manifest) sees the same parallel-dev-
// stack inputs under BOTH `forge env up` and `forge env deploy`:
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
// Dev IdP identity is NOT armed here. There is no render-time resolver for
// it: the `idp-provision` job (deploy/kcl/workloads.k, run as an ordinary
// one-shot alongside every other job) converges the registration and
// PUBLISHES the result — a ConfigMap on a cluster target, a committed KCL
// file on compose/dev — and every render simply reads whatever was last
// published. See the `auth/dev-loop` skill.
//
// It returns a restore func that reverts the port store to its pre-render
// bytes — the up path calls it when its already-running guard rejects a
// render, so a rejected attempt can't drift the stable resolve_port
// assignments. Deploy ignores it (an applied render's values ARE the truth).
//
// On the primary checkout with no worktree, option("worktree") is "" so a
// KCL that keys on it composes the DEFAULT stack — historical names and
// allocate_port(base, "") == base — byte-identical to before this primitive.
func activateDevStack(projectDir, env string) (devstack.Options, func()) {
	opts := devstack.Resolve(projectDir)
	devstack.SetActive(opts)

	// Back allocate_port with the persistent, lock-guarded block registry.
	//
	// Availability-aware on FIRST use only: the registry is per-project, so
	// two different projects on one machine both ask for the default key's
	// block 0 and therefore the identical base port. For the dev IdP that is
	// fatal — forge refuses to adopt an identity provider it did not start
	// (rightly: it would mint tokens the wrong issuer signed), so the second
	// project could not bring up sign-in at all. Stepping once, at the moment
	// the port is first chosen, and memoizing the result keeps every later
	// run byte-identical, which is the property the issuer actually needs.
	kclplugin.UseBlockAllocator(func(base int, key string) (int, error) {
		return devstack.AllocatePortAvoidingForeign(projectDir, base, key, func(p int) bool { return !portInUse(p) })
	})

	// Keep resolve_port stable + up==deploy via the per-env store.
	storePath := filepath.Join(projectDir, ".forge", "ports-"+env+".json")
	restore := kclplugin.UsePortStore(storePath)

	if opts.Worktree != "" || opts.Branch != "" {
		// STDERR, not stdout: this is a diagnostic about the render context,
		// not output. On stdout it prefixed the JSON document of every
		// `--json` command that arms a devstack render (`forge env status
		// --json` is the discovery call agents and scripts parse), so the
		// stream did not decode. A human piping through a terminal still
		// sees it.
		fmt.Fprintf(os.Stderr, "[devstack] worktree=%q branch=%q\n", opts.Worktree, opts.Branch)
	}
	return opts, restore
}
