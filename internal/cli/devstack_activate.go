package cli

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/instance"
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// activateInstance resolves the instance identity for this command and
// arms BOTH render-context globals BEFORE the first render:
//
//   - instance.SetActive — so every render (entity AND manifest) pushes
//     option("instance")/option("instance_index") into KCL;
//   - kclplugin.UsePortStore — so resolve_port reads/writes the SAME
//     persisted, instance-scoped store under BOTH `forge up` and
//     `forge deploy`. This is the kill-the-up-vs-deploy-drift fix: the
//     port store is now the single source of truth for a given
//     (env, instance, role), identical under both commands.
//
// It returns the resolved instance and a restore func that reverts the
// port store to its pre-render bytes — the up path calls it when its
// already-running guard rejects a render so a rejected attempt can't drift
// the stable assignments. Deploy ignores the restore (it has no such
// guard; an applied render's ports ARE the truth).
//
// The default instance (no --instance; primary checkout, not a linked
// worktree) keeps the historical .forge/ports-<env>.json path, so a plain
// single-stack up/deploy is byte-identical to before this primitive landed.
func activateInstance(projectDir, env, flagInstance string) (instance.Instance, func(), error) {
	inst, err := instance.Resolve(projectDir, flagInstance)
	if err != nil {
		return instance.Instance{}, func() {}, fmt.Errorf("resolve instance: %w", err)
	}
	instance.SetActive(inst)

	storePath := instance.PortStorePath(projectDir, env, inst)
	restore := kclplugin.UsePortStore(storePath)
	if !inst.IsDefault() {
		fmt.Printf("[instance] %s  (ns suffix -%s, port block +%d, store %s)\n",
			inst, inst.Name, inst.Index*100, storePath)
	}
	return inst, restore, nil
}
