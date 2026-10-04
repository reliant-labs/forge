package cli

import "fmt"

// Bundle.lifecycle — WHO APPLIES an environment.
//
// An env either is applied to DIRECTLY by forge, or is reconciled from a
// bundle by something else (the Flux path). Nothing else in a Bundle answers
// that: `control_plane` says whether anything is hosted, and the derived
// pkg/release EnvKind says what KIND of hosting. Neither is the apply path —
// a hosted env can still be a developer's throwaway cluster, and an env with
// no control plane at all can still be a real environment nobody should
// kubectl-apply into by hand.
//
// So the env DECLARES it, in one field, and this file is the Go half: the
// vocabulary, and the one predicate every consumer asks.
const (
	// lifecycleLocal is a developer's own cluster. Direct apply is the
	// point: the inner loop is `forge env up` against a cluster only this
	// machine can see.
	lifecycleLocal = "local"
	// lifecycleEphemeral is a throwaway per-run cluster (CI / e2e), created
	// and deleted inside one run. Routing it through a reconciler would add
	// a control loop with nothing to converge to.
	lifecycleEphemeral = "ephemeral"
)

// DirectApplyAllowed reports whether forge may apply to this environment
// DIRECTLY, rather than recording a bundle for a reconciler to converge.
//
// True in two cases, and they are different reasons:
//
//  1. the env DECLARES `lifecycle = "local" | "ephemeral"` — it is a cluster
//     forge creates and can delete, so applying to it is the intended path.
//     KCL has already checked that claim against every cluster the env
//     targets, so a GKE context cannot reach here by declaring itself local;
//  2. the env TARGETS NO CLUSTER AT ALL. A host-only or compose-only env has
//     nothing to apply, so "may forge apply directly" is vacuously true.
//     Answering false here would describe such an env as needing a
//     reconciler, which is the opposite of true: there is no cluster for one
//     to reconcile against.
//
// An env that declares nothing AND targets a cluster is a REAL environment,
// and this returns false for it. That default is deliberate: an env whose
// author forgot to declare it is treated as production, not as scratch.
//
// Every client-side apply path refuses an env this returns false for
// ([refuseDirectApply]); those envs deploy bundle -> record -> Flux.
func DirectApplyAllowed(e *KCLEntities) bool {
	if e == nil {
		// No render, so no declaration and no target. Nothing is known to
		// need a reconciler, and the callers here are notice/projection
		// paths that must not invent one.
		return true
	}
	switch e.Lifecycle {
	case lifecycleLocal, lifecycleEphemeral:
		return true
	}
	return !kclEntitiesHaveK8sCluster(e)
}

// refuseDirectApply is the ONE refusal every client-side cluster apply makes
// for an env that fails [DirectApplyAllowed].
//
// A deploy that names only declared platform charts is cluster bootstrap, not
// an env apply, and passes (charts install through installPlatformCharts).
// A dry run applies nothing, so it passes too.
func refuseDirectApply(env string, e *KCLEntities, targets []string, dryRun bool) error {
	if dryRun || DirectApplyAllowed(e) {
		return nil
	}
	if charts, rest := splitPlatformChartTargets(e, targets); len(charts) > 0 && len(rest) == 0 {
		return nil
	}
	return fmt.Errorf("%s is reconciled from its bundle and forge will not apply to its cluster directly; "+
		"run `forge env deploy %s [<version>]`, which records the promotion and waits for Flux. "+
		"If %s is a developer's own k3d cluster, declare `lifecycle = \"local\"` on its Bundle", env, env, env)
}
