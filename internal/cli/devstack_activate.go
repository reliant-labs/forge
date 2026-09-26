package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
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
//
// purpose decides whether the render may claim machine-local port state at
// all — see renderPurpose. A render of an env that runs nowhere on this
// machine arms none of the WRITING halves: allocate_port resolves to its base
// port and resolve_port reads its store without writing it.
func activateDevStack(ctx context.Context, projectDir, env string, purpose renderPurpose) (devstack.Options, func()) {
	// Every render starts unable to write files. Only a command that goes
	// on to MATERIALIZE the env re-arms it (armMaterializer), after this.
	kclplugin.UseFileWriter("")

	opts := devstack.Resolve(projectDir)
	devstack.SetActive(opts)
	if opts.Worktree != "" || opts.Branch != "" {
		// STDERR, not stdout: this is a diagnostic about the render context,
		// not output. On stdout it prefixed the JSON document of every
		// `--json` command that arms a devstack render (`forge env status
		// --json` is the discovery call agents and scripts parse), so the
		// stream did not decode. A human piping through a terminal still
		// sees it.
		fmt.Fprintf(os.Stderr, "[devstack] worktree=%q branch=%q\n", opts.Worktree, opts.Branch)
	}

	// Back fp.dev_stacks() with the registry's DEV-STACK roster, so a KCL
	// module that emits one config block per running stack (control-plane's
	// per-stack NATS accounts) enumerates stacks rather than parsing
	// .forge/blocks.json and mistaking a port-block key for a worktree.
	//
	// Armed for every env render — it only READS the registry — and NOT on
	// the generate path, where it returns empty so a tracked file generated
	// from it is byte-identical on every machine.
	kclplugin.UseDevStacks(func() ([]string, error) {
		return devstack.ListStacks(projectDir)
	})

	storePath := filepath.Join(projectDir, ".forge", "ports-"+env+".json")
	lastActivatedRunsHere = true
	if purpose == renderDeclaration && !envRunsOnThisMachine(ctx, projectDir, env) {
		lastActivatedRunsHere = false
		// Nothing about this env runs here, so there is no local port for
		// allocate_port to protect: it resolves to base, deterministically,
		// and neither port store is written.
		kclplugin.UseBlockAllocator(nil)
		kclplugin.UsePortStoreReadOnly(storePath)
		fmt.Fprintf(os.Stderr, "[devstack] env %q declares no local cluster or host process: "+
			"allocate_port resolves to its base port and no port block is claimed\n", env)
		return opts, func() {}
	}

	// Arm the parallel-dev-stack ceiling from forge.yaml's dev_stack.max_stacks
	// (config.DefaultMaxStacks when unset), so AllocateBlock refuses a NEW
	// block the project's cluster port pre-map was never widened to cover.
	// Loaded fresh here rather than threaded through every caller's already-
	// loaded *config.ProjectConfig, so none of its call sites across
	// up/deploy/render has to thread a config through. A load failure here means the command's own earlier config load already
	// failed and it never reached this point, so DefaultMaxStacks is a safe,
	// inert fallback rather than a silently-unbounded one.
	maxStacks := config.DefaultMaxStacks
	if cfg, err := config.LoadProjectDir(projectDir); err == nil {
		maxStacks = cfg.DevStack.EffectiveMaxStacks()
	}
	devstack.SetMaxStacks(maxStacks)

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
	restore := kclplugin.UsePortStore(storePath)
	return opts, restore
}

// armMaterializer lets the KCL fp.write_file builtin write files for the rest
// of this process's renders of env. It is the one decision separating a
// render that CHECKS from a render that BUILDS: `forge env up`'s bring-up and
// an applying `forge env deploy` of an env that runs here call it; `env
// render`, `env deploy --dry-run`, `env config`, status, doctor, lint, ci and
// generate never do, so a module's generated files are written by the
// commands that launch the env and by nothing that only reads it.
//
// A deploy of an env that runs nowhere on this machine does not arm it
// either: a file a dev KCL generates for a LOCAL process (control-plane's
// shared NATS config) has no consumer when the env is a cloud cluster.
//
// It reads activateDevStack's answer rather than probing again: the probe is
// itself a render, and it disarms the port allocator activateDevStack just
// armed. Call it immediately after activateDevStack for the same env.
func armMaterializer(projectDir string) {
	if !lastActivatedRunsHere {
		return
	}
	kclplugin.UseFileWriter(projectDir)
}

// lastActivatedRunsHere records activateDevStack's verdict on whether the env
// it just armed runs on this machine, for armMaterializer. One command
// activates one env at a time, so a process-level record is enough.
var lastActivatedRunsHere bool

// renderPurpose is WHY a command renders an env, and it decides whether that
// render may claim machine-local port state.
//
// allocate_port exists for parallel LOCAL dev stacks: it memoizes a port block
// per key in the primary checkout's .forge/blocks.json, bounded by
// dev_stack.max_stacks. That state means something only to a process that will
// actually bind the port on this machine. Arming it for every render is what
// let `forge env render prod` from a linked worktree register a NEW block for
// "prod-<worktree>" — a permanent leak per throwaway worktree, and once the
// registry reached the ceiling, a read-only prod render that FAILED with
// "refusing to allocate a NEW port block".
type renderPurpose int

const (
	// renderDeclaration: this command renders the env to print it or ship it
	// (`forge env render`, `forge env deploy`, the env-scoped cluster
	// lifecycle). The allocator is armed only when the env's own declaration
	// targets this machine — see envRunsOnThisMachine.
	//
	// It is the zero value on purpose: a caller that never says why it is
	// rendering claims nothing it cannot justify.
	renderDeclaration renderPurpose = iota
	// renderToLaunch: this command runs the env's processes on THIS machine
	// (`forge env up`, including its deploy phase), or reports on what up
	// launched (`forge env status`). The allocator is armed unconditionally:
	// `forge env up prod --target reliant-web` runs a local dev server for a
	// cloud env, and it needs its own block exactly as a dev stack does.
	renderToLaunch
)

// envRunsOnThisMachine reports whether env's declaration targets this machine,
// decided by a probe render with the block allocator DISARMED — so the probe
// itself claims nothing, whatever the answer turns out to be.
//
// A probe that fails to render answers false. The real render fails the same
// way immediately afterwards and reports the error in its own words, and
// answering false means it does so without having claimed a block first.
func envRunsOnThisMachine(ctx context.Context, projectDir, env string) bool {
	kclplugin.UseBlockAllocator(nil)
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil {
		return false
	}
	return entitiesTargetThisMachine(entities)
}

// entitiesTargetThisMachine reports whether anything the env declares runs on
// this machine: a k3d cluster forge creates, a host process, a docker-compose
// or host-infra service, or a workload bound for a local cluster context.
//
// The answer comes from the declaration, never from the env's NAME: "prod" is
// not special, and a project whose staging env targets a local k3d cluster
// gets the same port blocks its dev env does.
//
// A cluster forge only DIALS (a GKE context, a ClusterClient marked external)
// does not count, and neither does a frontend shipped to Firebase or a static
// bucket — `forge env up` is the only thing that ever runs a frontend locally,
// and it renders with renderToLaunch.
func entitiesTargetThisMachine(e *KCLEntities) bool {
	if e == nil {
		return false
	}
	for _, c := range e.Clusters {
		// Only the k3d provider is implemented, and a k3d cluster runs here.
		if c.Provider == "" || c.Provider == "k3d" || isLocalCluster(c.Context) {
			return true
		}
	}
	for _, svc := range e.Services {
		switch svc.Deploy.Type {
		case "host", "compose", "host-infra":
			return true
		case "cluster":
			if c := svc.Deploy.Cluster; c != nil && isLocalCluster(c.Cluster) {
				return true
			}
		case "simple-backend":
			if sb := svc.Deploy.SimpleBackend; sb != nil && isLocalCluster(sb.Cluster) {
				return true
			}
		}
	}
	for _, chart := range e.HelmCharts {
		if isLocalCluster(chart.Cluster) {
			return true
		}
	}
	return false
}
