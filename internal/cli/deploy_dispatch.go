package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// buildDeployGroups walks the rendered entities and produces the
// deploy groups the dispatcher will dispatch in turn.
//
// Services that carry K8sCluster fields group natively by the
// (Cluster, Namespace, Registry) tuple. The namespace defaults to the
// deploy-time fallback when KCL leaves it blank — typically the
// user-supplied --namespace or the auto-computed `<project>-<env>`
// shape.
//
// external and compose services flow through GroupServices unchanged.
// host / build-only / no-deploy services are skipped.
//
// buildDeployGroupsWithOpts is the dry-run-aware variant — callers
// that want to plumb --dry-run through to External / Compose use this
// shape. buildDeployGroups stays as the legacy shape so older call
// sites don't need to be touched.
func buildDeployGroupsWithOpts(envName string, entities *KCLEntities, fallbackNamespace string, dryRun bool) ([]deploytarget.ServiceGroup, error) {
	groups, err := buildDeployGroups(envName, entities, fallbackNamespace)
	if err != nil {
		return nil, err
	}
	if dryRun {
		for i := range groups {
			groups[i].DryRun = true
		}
	}
	return groups, nil
}

func buildDeployGroups(envName string, entities *KCLEntities, fallbackNamespace string) ([]deploytarget.ServiceGroup, error) {
	if entities == nil {
		return nil, nil
	}

	// Resolve the bundle's secret provider once. For a dotenv provider,
	// All() returns the resolved key→value map we inline into the runtime
	// env of External/Compose services (env_file overrides on conflict —
	// see the merge in compose.go / external.go deployOne). External/none
	// providers return nil (those resolve secrets out-of-band), so the
	// merge is a no-op for them. K8sCluster services deliberately do NOT
	// receive this map: they get rendered Secret objects + secretKeyRef.
	prov, err := secretProviderFromEntities(entities, projectDirForKCL())
	if err != nil {
		return nil, fmt.Errorf("secret provider: %w", err)
	}
	secretEnv := prov.All()

	var raw []deploytarget.RawService
	for _, svc := range entities.Services {
		switch svc.Deploy.Type {
		case "cluster":
			c := svc.Deploy.Cluster
			if c == nil {
				continue
			}
			namespace := c.Namespace
			if namespace == "" {
				namespace = fallbackNamespace
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				K8sCluster: &deploytarget.RawK8sCluster{
					Cluster:   c.Cluster,
					Namespace: namespace,
					Registry:  c.Registry,
					Domain:    c.Domain,
					Spec: &deploytarget.K8sClusterSpec{
						Replicas: c.Replicas,
						Platform: c.Platform,
						Ports:    c.Ports,
					},
				},
			})
		case "external":
			e := svc.Deploy.External
			if e == nil {
				continue
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				External: &deploytarget.ExternalSpec{
					// Image is hoisted from the surrounding Service.image
					// so the ${IMAGE} substitution token resolves without
					// forcing the user to duplicate the string on the
					// deploy block.
					Image:       svc.Image,
					DeployCmd:   e.DeployCmd,
					RollbackCmd: e.RollbackCmd,
					HealthCmd:   e.HealthCmd,
					EnvFile:     e.EnvFile,
					Env:         e.Env,
				},
				Secrets: secretEnv,
			})
		case "compose":
			cm := svc.Deploy.Compose
			if cm == nil {
				continue
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				Compose: &deploytarget.ComposeSpec{
					ComposeFile: cm.ComposeFile,
					Service:     cm.Service,
					EnvFile:     cm.EnvFile,
				},
				Secrets: secretEnv,
			})
		default:
			// host, build-only, "" — skipped.
		}
	}
	return deploytarget.GroupServices(envName, raw)
}

// dispatchDeployGroups runs every group through its provider. Per-
// group failures abort the loop (deploy.go's pre-v2 behavior was
// fail-fast on the single apply) but each failure is wrapped to
// include the provider id + group target so users can tell at a
// glance which group failed.
//
// Rollback: when a group's Deploy fails AND lastGoodTag is non-empty,
// the function asks the provider to roll back to lastGoodTag. Rollback
// errors are logged but the original Deploy error is still returned —
// rollback is a recovery affordance, not a way to mask the underlying
// failure.
func dispatchDeployGroups(ctx context.Context, registry *deploytarget.Registry, groups []deploytarget.ServiceGroup, lastGoodTag string) error {
	if registry == nil {
		return errors.New("deploy dispatch: nil provider registry")
	}
	for _, group := range groups {
		p := registry.Lookup(group.ProviderID)
		if p == nil {
			return fmt.Errorf("deploy dispatch: no provider for %q (group: %s)", group.ProviderID, deploytarget.FormatGroupSummary(group))
		}
		fmt.Printf("\n%s\n", deploytarget.FormatGroupSummary(group))
		if err := p.Deploy(ctx, group); err != nil {
			if lastGoodTag != "" {
				if rerr := p.Rollback(ctx, group, lastGoodTag); rerr != nil {
					fmt.Printf("  Note: rollback also failed: %v\n", rerr)
				}
			}
			return fmt.Errorf("deploy %s: %w", group.ProviderID, err)
		}
	}
	return nil
}

// rollbackDeployGroups is the `forge deploy <env> --rollback`
// dispatcher. For each group it looks up the previously-recorded
// last-good tag (per service, from .forge/state) and asks the
// provider to revert there.
//
// Per-provider error contract:
//
//   - k8s-cluster: `kubectl rollout undo deployment/<svc>` doesn't
//     need a state file (the cluster tracks the previous ReplicaSet),
//     so the dispatcher hands the provider the empty tag and lets
//     kubectl do the work. Missing-Deployment is the provider's
//     concern, not the dispatcher's.
//   - external / compose: per-service state file is required. A
//     missing file produces a clear `no previous deploy state
//     recorded` error so the user knows there's nothing to revert.
//
// Group-level failures abort the loop — partial rollbacks are still
// recovery (a service that can't roll back is louder than a service
// that quietly stays on the new tag).
func rollbackDeployGroups(ctx context.Context, registry *deploytarget.Registry, groups []deploytarget.ServiceGroup, projectDir string) error {
	if registry == nil {
		return errors.New("rollback dispatch: nil provider registry")
	}
	if len(groups) == 0 {
		fmt.Println("Nothing to roll back — no deploy targets declared for this env.")
		return nil
	}
	for _, group := range groups {
		p := registry.Lookup(group.ProviderID)
		if p == nil {
			return fmt.Errorf("rollback dispatch: no provider for %q (group: %s)", group.ProviderID, deploytarget.FormatGroupSummary(group))
		}
		fmt.Printf("\n%s (rollback)\n", deploytarget.FormatGroupSummary(group))

		// For external/compose, validate each service has a state
		// file BEFORE the provider's Rollback runs — so we can fail
		// the whole group with a precise per-service message rather
		// than letting the provider emit a partial-rollback error.
		if group.ProviderID == "external" || group.ProviderID == "compose" {
			if err := requireRollbackState(projectDir, group); err != nil {
				return fmt.Errorf("rollback %s: %w", group.ProviderID, err)
			}
		}

		// lastGoodTag is empty for the k8s-cluster path (kubectl owns
		// the revision history). For external/compose, the provider
		// reads its own per-service state file inside Rollback — the
		// dispatcher-supplied lastGoodTag is a fallback only, and we
		// leave it empty so the state-file tag always wins.
		if err := p.Rollback(ctx, group, ""); err != nil {
			return fmt.Errorf("rollback %s: %w", group.ProviderID, err)
		}
	}
	return nil
}

// requireRollbackState confirms every service in an external/compose
// group has a recorded last-good deploy. Surfaces a clear per-service
// error when one is missing — `forge deploy <env> --rollback` against
// a service that's never deployed should refuse rather than
// silently no-op or guess.
func requireRollbackState(projectDir string, group deploytarget.ServiceGroup) error {
	for _, svc := range group.Services {
		st, err := deploytarget.ReadDeployState(projectDir, group.ProviderID, group.Env, svc.Name)
		if err != nil {
			return err
		}
		if st == nil {
			return fmt.Errorf("no previous deploy state recorded for %s at %s; cannot rollback", svc.Name, group.Env)
		}
	}
	return nil
}

// applyOptsBuilderFromContext returns an ApplyOptsBuilder closure
// that captures the deploy-wide opts (mainK, image tag, env config,
// dry-run, prune, host-skip, one-shot jobs, kube-context) and emits a
// per-group cluster.ApplyOpts. For K8sCluster groups the group's
// Namespace overrides the closure's namespace — that's the new path
// where the per-service deploy block dictates the namespace rather than
// forge.yaml.
//
// Context is purely DECLARATIVE: every K8sCluster group already carries
// its target cluster in group.Cluster (populated from the KCL
// `forge.K8sCluster.cluster`, which IS the kubectl context name), so the
// per-group kubectl context is derived from group.Cluster. This is the
// "can't deploy the wrong env to the wrong cluster" property — the binding
// lives in the env's KCL, not in whatever context happens to be active,
// and there is no CLI override. A multi-cluster env (rare) therefore
// applies each group to ITS OWN declared cluster context. When a group
// declares no cluster (host-only / compose), Context stays empty — and the
// cluster.KubectlApply chokepoint refuses an empty context rather than
// falling back to the active one.
func applyOptsBuilderFromContext(mainK, imageTag, fallbackNamespace, env string, envCfgKV map[string]string, dryRun, prune bool, hostSkip map[string]struct{}, oneShotJobs, targets []string, groups []deploytarget.ServiceGroup) func(deploytarget.ServiceGroup) cluster.ApplyOpts {
	scopeFor := clusterScopeForGroups(groups)
	return func(group deploytarget.ServiceGroup) cluster.ApplyOpts {
		ns := group.Namespace
		if ns == "" {
			ns = fallbackNamespace
		}
		return cluster.ApplyOpts{
			MainK:        mainK,
			ImageTag:     imageTag,
			Namespace:    ns,
			Env:          env,
			Context:      resolveGroupContext(group),
			EnvConfigKV:  envCfgKV,
			DryRun:       dryRun,
			DryRunFramed: true,
			Prune:        prune,
			HostSkip:     hostSkip,
			OneShotJobs:  oneShotJobs,
			Targets:      targets,
			ClusterScope: scopeFor(group),
		}
	}
}

// clusterScopeForGroups returns a closure that maps one k8s-cluster group
// to the cluster.GroupScope that filters the env's rendered manifest
// stream to THAT group's cluster — the load-bearing half of the
// multi-cluster fix.
//
// KCL renders the whole env as a single manifest stream (every service's
// workloads, the namespace, gateways, CRDs, infra). Each k8s group applies
// it to its OWN declared `--context`, so without scoping a two-cluster env
// dumps the entire bundle onto BOTH clusters: the secondary cluster
// receives the primary's whole stack (and hard-fails on CRDs it doesn't
// have), and the primary receives the secondary's service. The GroupScope
// partitions the stream so each cluster gets only what belongs on it (see
// cluster.ScopeManifestsToGroup for the per-document ownership rule).
//
// SINGLE-CLUSTER no-op: when every k8s group declares the SAME cluster
// (the common dev-k8s / staging / prod case), there is no second cluster to
// isolate, so the closure returns nil and the apply path is byte-identical
// to the pre-fix behaviour. Scoping engages ONLY when >1 distinct cluster
// is declared across the k8s groups.
//
// PRIMARY cluster: the cluster the env's bundle was rendered against — its
// Namespace, Gateways, CRDs, infra, and every ungrouped app-labelled
// manifest (operators / CronJobs) follow it. We pick the cluster that the
// MOST services target (the bulk of the env); the lone proxy-on-a-second-
// cluster pattern this fix targets makes that unambiguous, and ties break
// deterministically by cluster name so the choice is stable across runs.
func clusterScopeForGroups(groups []deploytarget.ServiceGroup) func(deploytarget.ServiceGroup) *cluster.GroupScope {
	// Distinct clusters across the k8s groups, and the apps each owns.
	clusters := map[string]struct{}{}
	appsByCluster := map[string][]string{}
	svcCountByCluster := map[string]int{}
	for _, g := range groups {
		if g.ProviderID != "k8s-cluster" || g.Cluster == "" {
			continue
		}
		clusters[g.Cluster] = struct{}{}
		for _, s := range g.Services {
			appsByCluster[g.Cluster] = append(appsByCluster[g.Cluster], s.Name)
			svcCountByCluster[g.Cluster]++
		}
	}
	// Single-cluster (or no-cluster) env: nothing to isolate — no-op.
	if len(clusters) < 2 {
		return func(deploytarget.ServiceGroup) *cluster.GroupScope { return nil }
	}

	primary := primaryCluster(svcCountByCluster)
	return func(group deploytarget.ServiceGroup) *cluster.GroupScope {
		if group.ProviderID != "k8s-cluster" || group.Cluster == "" {
			return nil
		}
		own := map[string]struct{}{}
		for _, name := range appsByCluster[group.Cluster] {
			own[name] = struct{}{}
		}
		other := map[string]struct{}{}
		for c, apps := range appsByCluster {
			if c == group.Cluster {
				continue
			}
			for _, name := range apps {
				other[name] = struct{}{}
			}
		}
		return &cluster.GroupScope{
			OwnApps:   own,
			OtherApps: other,
			Primary:   group.Cluster == primary,
		}
	}
}

// primaryCluster picks the env's primary cluster from the per-cluster
// service counts: the cluster the MOST services target. Ties break by
// cluster name (lexicographically smallest) so the choice is deterministic
// across runs. The primary is where the env's shared resources (Namespace,
// Gateways, CRDs, infra, operators, CronJobs) land; secondary clusters
// receive only their own services' workloads. Empty input returns "".
func primaryCluster(svcCountByCluster map[string]int) string {
	primary := ""
	best := -1
	for c, n := range svcCountByCluster {
		if n > best || (n == best && c < primary) {
			best = n
			primary = c
		}
	}
	return primary
}

// resolveGroupContext picks the kubectl context for a single deploy
// group. The kubectl context is purely DECLARATIVE: the declared cluster
// (group.Cluster, from KCL `forge.K8sCluster.cluster`) IS the kubectl
// context name, and it is the ONLY source — there is no CLI override. An
// empty result means the group declares no cluster (host-only / compose);
// the cluster.KubectlApply chokepoint refuses an empty context on a write
// rather than falling back to kubectl's current context.
func resolveGroupContext(group deploytarget.ServiceGroup) string {
	return group.Cluster
}

// declaredEnvContext returns the env-wide kubectl context for the
// consumers that don't iterate groups per-target: the secrets pre-apply,
// the empty-groups direct cluster.Apply, and the rollback provider. It is
// the first declared K8sCluster cluster (group.Cluster, from KCL
// `forge.K8sCluster.cluster`) — there is no CLI override. Empty when no
// cluster is declared (host-only / compose); the apply chokepoint refuses
// an empty context on a write rather than using kubectl's current one.
//
// A multi-cluster env's per-group dispatch still routes each group to its
// own declared cluster via resolveGroupContext; this single value covers
// only the env-wide single-cluster paths, which already assume one
// namespace per env.
func declaredEnvContext(groups []deploytarget.ServiceGroup) string {
	for _, g := range groups {
		if g.ProviderID == "k8s-cluster" && g.Cluster != "" {
			return g.Cluster
		}
	}
	return ""
}
