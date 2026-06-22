package deploytarget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/reliant-labs/forge/internal/cluster"
)

// K8sClusterProvider is the full Go implementation for the
// K8sCluster deploy target. It wraps internal/cluster.Apply — the
// existing render-KCL → kubectl-apply → wait-rollouts pipeline that
// `forge deploy` / `forge cluster reload` / `forge up` share.
//
// The provider takes the env-wide knobs off the ServiceGroup (which
// got them from the first K8sCluster ref in the group). The per-
// service knobs are reflected on the rendered manifests by KCL — this
// provider doesn't re-apply them, it just hands the right env / image
// tag / namespace to the cluster pipeline and lets the renderer do
// the rest.
type K8sClusterProvider struct {
	// ApplyOptsBuilder lets callers customize cluster.ApplyOpts before
	// the provider invokes cluster.Apply. The forge CLI uses this to
	// plumb through MainK, EnvConfigKV, HostSkip, OneShotJobs, Prune
	// from the rendered KCL — fields the provider itself doesn't know
	// about. A nil builder means "use the group's namespace+image tag
	// and let cluster.Apply default everything else", which is enough
	// for tests but not for the real forge deploy path.
	ApplyOptsBuilder func(group ServiceGroup) cluster.ApplyOpts
}

// rollbackContext resolves the kubectl context for a single rollback
// group. The context is purely DECLARATIVE: the group's own declared
// cluster (group.Cluster, from KCL forge.K8sCluster.cluster, which IS the
// kubectl context name) is the only source. There is NO CLI override and
// NO fall-back to kubectl's current/active context. A multi-cluster
// rollback therefore routes each group to its own declared cluster, and a
// wrong-cluster rollback can't happen by relying on a globally-switched
// active context. An empty result means the group carried no declared
// cluster; the rollback path treats that as a HARD ERROR (see Rollback)
// rather than running `kubectl rollout undo` against whatever context is
// active.
func (p K8sClusterProvider) rollbackContext(group ServiceGroup) string {
	return group.Cluster
}

// Name returns the provider identifier.
func (K8sClusterProvider) Name() string { return "k8s-cluster" }

// Deploy invokes cluster.Apply for the group. The provider doesn't
// re-render KCL or re-walk services — that work is already done at
// the dispatcher layer; this just hands cluster.Apply the env-wide
// knobs (namespace, image tag) and lets it shell `kcl run` against
// the env's main.k.
func (p K8sClusterProvider) Deploy(ctx context.Context, group ServiceGroup) error {
	if group.Namespace == "" {
		return errors.New("k8s-cluster: ServiceGroup.Namespace is empty (forge.yaml or K8sCluster.namespace must declare it)")
	}
	var opts cluster.ApplyOpts
	if p.ApplyOptsBuilder != nil {
		opts = p.ApplyOptsBuilder(group)
	} else {
		// Fallback shape — tests that don't plumb a builder still get
		// a defensible default. The real forge deploy path always
		// passes a builder.
		opts = cluster.ApplyOpts{
			ImageTag:  group.ImageTag,
			Namespace: group.Namespace,
		}
	}
	if err := cluster.Apply(ctx, opts); err != nil {
		return fmt.Errorf("k8s-cluster deploy (ns=%s, cluster=%s): %w",
			group.Namespace, group.Cluster, err)
	}
	return nil
}

// Rollback runs `kubectl rollout undo deployment/<svc> -n <ns>` for
// every service in the group. Best-effort: per-service failures are
// logged and joined into the returned error, but the loop doesn't
// abort on the first failure (one stuck service shouldn't block
// rolling back the others).
//
// The function falls back to a no-op when kubectl isn't on PATH or
// the namespace is empty (an invalid group shape) — those cases
// already failed louder upstream.
func (p K8sClusterProvider) Rollback(ctx context.Context, group ServiceGroup, lastGoodTag string) error {
	if group.Namespace == "" {
		return errors.New("k8s-cluster rollback: ServiceGroup.Namespace is empty")
	}
	kctx := p.rollbackContext(group)
	// HARD ERROR on an empty context, mirroring cluster.KubectlApply: a
	// rollback runs `kubectl rollout undo`, a cluster WRITE/mutation. The
	// target cluster is declarative (forge.K8sCluster.cluster), so an empty
	// context means this group failed to carry its declared cluster.
	// Running the undo against whatever context happens to be active is the
	// same footgun as a wrong-cluster apply — refuse loudly, never fall
	// back to the current context.
	if strings.TrimSpace(kctx) == "" {
		return errors.New("k8s-cluster rollback: refusing to roll back without an explicit kubectl context: " +
			"the target cluster is declarative (forge.K8sCluster.cluster in the env's KCL) — " +
			"forge never falls back to the current context for a write")
	}
	var failures []string
	for _, svc := range group.Services {
		// Thread the `--context <ctx>` per command (not via a global
		// `kubectl config use-context`) so a concurrent rollback to a
		// different cluster can't be clobbered by another deploy's context
		// switch. kctx is the group's declared cluster (never empty here —
		// guarded above). cluster.KubectlArgs is the single point that owns
		// the per-command `--context` invariant.
		args := cluster.KubectlArgs(kctx, "rollout", "undo", "deployment/"+svc.Name, "-n", group.Namespace)
		// The annotated revision lets users see which tag we rolled
		// back from. Best-effort — failures are logged below.
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", svc.Name, err))
			fmt.Printf("  rollback %s: %v\n", svc.Name, err)
			continue
		}
		fmt.Printf("  rollback %s: ok (target tag %s)\n", svc.Name, lastGoodTag)
	}
	if len(failures) > 0 {
		return fmt.Errorf("k8s-cluster rollback: %d failure(s): %s",
			len(failures), strings.Join(failures, "; "))
	}
	return nil
}
