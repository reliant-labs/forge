package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/deploy"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
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
	// A Bundle that declares a control plane deploys THROUGH it — every
	// workload, as one hosted group. This is the single selection point, and
	// it is declarative: the same KCL chooses the same destination on every
	// machine, with no flag that could route a hosted env to a kubeconfig.
	if entities.ControlPlane != nil {
		return buildHostedGroups(envName, entities)
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
		case "simple-backend":
			sb := svc.Deploy.SimpleBackend
			if sb == nil {
				continue
			}
			namespace := sb.Namespace
			if namespace == "" {
				namespace = fallbackNamespace
			}
			// A SimpleBackend deploys through the k8s-cluster PROVIDER,
			// not one of its own, and that is the design rather than a
			// shortcut. Its objects come from pkg/deploy.Render (expanded
			// from the forge.dev declaration record at manifest
			// extraction), and they are ordinary Deployment/Service/PVC
			// objects. So apply, prune, rollout-wait, the per-group
			// --context discipline and multi-cluster scoping are the ones
			// K8sClusterProvider already implements against clusters forge
			// did not create.
			//
			// The tier stays VISIBLE where visibility matters: the entity
			// contract keeps the `simple-backend` discriminator, so `forge
			// env render` and `forge project audit` report it.
			//
			// Registry is deliberately empty. The image is the app owner's
			// own, fully qualified and pinned, and the group key is
			// (cluster, namespace, registry), so these group by their own
			// namespace rather than joining an ordinary cluster group that
			// happens to share one.
			domain := ""
			if len(sb.Spec.Domains) > 0 {
				domain = sb.Spec.Domains[0]
			}
			var ports []int
			for _, p := range sb.Spec.Ports {
				ports = append(ports, int(p))
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				K8sCluster: &deploytarget.RawK8sCluster{
					Cluster:   sb.Cluster,
					Namespace: namespace,
					Domain:    domain,
					Spec: &deploytarget.K8sClusterSpec{
						// Always 1. The spec declares no replicas: storageGiB
						// renders a ReadWriteOnce PVC that a multi-replica
						// Deployment cannot roll over.
						Replicas: 1,
						Ports:    ports,
						// The PVC this tier CREATES, named for the
						// observer. A claim forge creates is one it must be
						// able to read back, because an unbound PVC shows up
						// on the Deployment only as "0/1 ready", which names
						// the symptom and not the cause.
						OwnedClaims: simpleBackendOwnedClaims(svc.Name, sb),
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
					ComposeFile:        cm.ComposeFile,
					Service:            cm.Service,
					EnvFile:            cm.EnvFile,
					Wait:               cm.Wait,
					WaitTimeoutSeconds: cm.WaitTimeout,
					Env:                cm.Env,
				},
				Secrets: secretEnv,
			})
		case "host-infra":
			hi := svc.Deploy.HostInfra
			if hi == nil {
				continue
			}
			// No Secrets layer: a host-infra instance's credentials are the
			// ones the declaration states, and the app's DSN is composed from
			// the SAME declaration. Injecting a secret store's values here
			// would give forge a second, independent opinion about the
			// password — which is how the two ever disagree.
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				HostInfra: &deploytarget.HostInfraSpec{
					Engine:          hi.Engine,
					Port:            hi.Port,
					Database:        hi.Database,
					User:            hi.User,
					Password:        hi.Password,
					DataDir:         hi.DataDir,
					Version:         hi.Version,
					IDPDatabase:     hi.IDPDatabase,
					IDPDatabasePort: hi.IDPDatabasePort,
					IDPMasterKey:    hi.IDPMasterKey,
					IDPStepsFile:    hi.IDPStepsFile,
					IDPPATPath:      hi.IDPPATPath,
				},
			})
		default:
			// host, build-only, "" — skipped.
		}
	}
	return deploytarget.GroupServices(envName, raw)
}

// simpleBackendOwnedClaims names the PersistentVolumeClaims forge emits
// for one SimpleBackend service: one, the renderer's own PVC name, when the
// service declares storage, and none otherwise.
//
// The name comes from pkg/deploy.PVCName, the SAME function Render names the
// claim with, so the observer and the renderer cannot disagree about it. The
// KCL-era version re-derived "<name>-data" by hand and needed a test to keep
// the two copies in step.
func simpleBackendOwnedClaims(svcName string, sb *SimpleBackendSpec) []string {
	if sb == nil || sb.Spec.StorageGiB <= 0 {
		return nil
	}
	return []string{deploy.PVCName(svcName)}
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

// rollbackDeployGroups is the `forge env deploy <env> --rollback`
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
// error when one is missing — `forge env deploy <env> --rollback` against
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

// applyOptsContext carries the deploy-wide envelope that
// applyOptsBuilderFromContext captures once and threads into every
// per-group cluster.ApplyOpts it emits. Grouping these into one struct
// keeps the builder's signature to a single declarative parameter.
type applyOptsContext struct {
	MainK             string
	ImageTag          string
	FallbackNamespace string
	Env               string
	EnvCfgKV          map[string]string
	DryRun            bool
	Prune             bool
	HostSkip          map[string]struct{}
	Targets           []string
	Groups            []deploytarget.ServiceGroup
	Entities          *KCLEntities
	ImageDigests      map[string]string
	HelmCharts        []cluster.HelmChartSpec
	Rollout           cluster.RolloutPolicy
	// OnStream and OnRollout are the optional observation callbacks the
	// --json report installs (nil in text mode). Threaded through the builder
	// so a multi-group dispatch reports every group's stream and every
	// group's rollout outcomes into ONE document.
	OnStream  func(string)
	OnRollout func(cluster.RolloutObservation)
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
func applyOptsBuilderFromContext(p applyOptsContext) func(deploytarget.ServiceGroup) cluster.ApplyOpts {
	scopeFor := clusterScopeForGroups(p.Groups, p.Entities)
	// Platform deps (helm-as-a-RENDERER) are env-level, not per-group. To
	// apply each selected chart EXACTLY ONCE across a multi-group dispatch,
	// attach the chart specs only to the FIRST k8s group processed —
	// identified by its context. The charts carry their own target
	// namespace (helm template -n), so a single apply against any one of the
	// env's group contexts is correct for the cloud single-cluster case;
	// the once-only guard avoids re-applying cert-manager per service group.
	//
	// A chart that re-targets a SECOND cluster (HelmChartSpec.Cluster) still
	// rides this one group, and still installs into its own cluster: the
	// context is resolved PER CHART inside applyRenderedCharts, not taken from
	// the group. Keeping the attachment here group-agnostic is deliberate — a
	// re-targeted chart must be applied once whether or not the env happens to
	// declare a service group on that cluster, and an operator cluster
	// frequently has no forge-deployed service at all.
	//
	// The primary context is the env's own (declaredEnvContext: the declared
	// cluster_target, else the first group). Group order alone would pick
	// whichever context sorts lowest, which put prod's cert-manager on its
	// daemon cluster.
	primaryHelmContext := ""
	if len(p.HelmCharts) > 0 {
		primaryHelmContext = declaredEnvContext(p.Entities, p.Groups)
		// The charts must ride SOME group or they are silently never
		// applied. If no group in this dispatch targets the declared context
		// (a --target that selects only a secondary cluster's app), fall back
		// to the first group with a context, as before.
		rides := false
		for _, g := range p.Groups {
			if resolveGroupContext(g) == primaryHelmContext && primaryHelmContext != "" {
				rides = true
				break
			}
		}
		if !rides {
			primaryHelmContext = ""
			for _, g := range p.Groups {
				if c := resolveGroupContext(g); c != "" {
					primaryHelmContext = c
					break
				}
			}
		}
	}
	return func(group deploytarget.ServiceGroup) cluster.ApplyOpts {
		ns := group.Namespace
		if ns == "" {
			ns = p.FallbackNamespace
		}
		// Emit the platform-dep specs only on the primary context's group so
		// each chart applies once. Other groups get no charts.
		var charts []cluster.HelmChartSpec
		if len(p.HelmCharts) > 0 && resolveGroupContext(group) == primaryHelmContext {
			charts = p.HelmCharts
		}
		return cluster.ApplyOpts{
			MainK:        p.MainK,
			ImageTag:     p.ImageTag,
			ImageDigests: p.ImageDigests,
			Namespace:    ns,
			Env:          p.Env,
			Context:      resolveGroupContext(group),
			EnvConfigKV:  p.EnvCfgKV,
			DryRun:       p.DryRun,
			DryRunFramed: true,
			Prune:        p.Prune,
			HostSkip:     p.HostSkip,
			Targets:      p.Targets,
			ClusterScope: scopeFor(group),
			HelmCharts:   charts,
			Rollout:      p.Rollout,
			OnStream:     p.OnStream,
			OnRollout:    p.OnRollout,
		}
	}
}

// clusterScopeForGroups returns a closure that maps one k8s-cluster group
// to the cluster.GroupScope that filters the env's rendered manifest
// stream to THAT group's cluster — the load-bearing half of declared-
// cluster-only multi-cluster routing.
//
// KCL renders the whole env as a single manifest stream (every service's
// workloads, namespace, gateways, CRDs, infra). Each k8s group applies it
// to its OWN declared `--context`, so without scoping a two-cluster env
// dumps the entire bundle onto BOTH clusters: the secondary receives the
// other cluster's whole stack (and hard-fails on CRDs it doesn't have).
// The GroupScope partitions the stream so each cluster gets only the
// manifests whose OWNING service targets it — every manifest carries its
// owner's `app.kubernetes.io/name` label (workloads AND per-service owned
// manifests, including the env-level resources an image-less infra service
// pins to a cluster). There is NO primary cluster: a manifest lands on the
// cluster its owner declares, nowhere else (see
// cluster.ScopeManifestsToGroup for the per-document ownership rule).
//
// OPERATORS + CRONJOBS attribute to the env's MAIN cluster. Unlike services,
// an operator (forge.Operator) and a non-host cronjob (forge.CronJob) carry no
// per-service `deploy` K8sCluster — they are NOT part of the service-to-cluster
// grouping, so they never land in any group's Services. But forge's renderer
// stamps `app.kubernetes.io/name = <name>` on their Deployment / Job / RBAC all
// the same. Without claiming those app labels for SOME cluster's OwnApps, the
// scoper sees them as app-labelled-but-ungrouped and KEEPS them defensively on
// every cluster — replicating a control-plane operator (e.g. workspace-controller,
// whose ServiceAccount / ClusterRBAC / mounted kubeconfig Secret all live in the
// main cluster) into the daemon cluster, where it's stuck ContainerCreating and
// fails the rollout wait. An operator/cronjob deploys to the env's main cluster
// (RenderEnv.cluster — the env-level deploy target, == the first cluster-shaped
// service's cluster: see mainClusterForEntities), so we ADD their app names to
// THAT cluster's OwnApps. They then land in every OTHER cluster's OtherApps and
// the scoper drops them there. SINGLE-CLUSTER envs never scope (below), so this
// is invariant for dev-k8s / staging / prod.
//
// SINGLE-CLUSTER no-op: when every k8s group declares the SAME cluster
// (the common dev-k8s / staging / prod case), there is no second cluster to
// isolate, so the closure returns nil and the apply path is byte-identical
// to the pre-scoping behaviour. Scoping engages ONLY when >1 distinct
// cluster is declared across the k8s groups.
func clusterScopeForGroups(groups []deploytarget.ServiceGroup, entities *KCLEntities) func(deploytarget.ServiceGroup) *cluster.GroupScope {
	// Distinct clusters across the k8s groups, and the apps each owns. The
	// app set per cluster includes image-less infra services (they ARE
	// cluster groups in buildDeployGroups), so an infra service's owned
	// manifests route to its declared cluster via its app label.
	clusters := map[string]struct{}{}
	appsByCluster := map[string][]string{}
	for _, g := range groups {
		if g.ProviderID != "k8s-cluster" || g.Cluster == "" {
			continue
		}
		clusters[g.Cluster] = struct{}{}
		for _, s := range g.Services {
			appsByCluster[g.Cluster] = append(appsByCluster[g.Cluster], s.Name)
		}
	}
	// Attribute operators + cronjobs to the env's main cluster (see doc above).
	// Operators/cronjobs carry no deploy cluster, so the main cluster is the
	// env-level deploy target (the first cluster-shaped service's cluster).
	if entities != nil {
		if main := mainClusterForEntities(entities, groups); main != "" {
			for _, o := range entities.Operators {
				appsByCluster[main] = append(appsByCluster[main], o.Name)
			}
			for _, c := range entities.CronJobs {
				appsByCluster[main] = append(appsByCluster[main], c.Name)
			}
		}
	}
	// Single-cluster (or no-cluster) env: nothing to isolate — no-op.
	if len(clusters) < 2 {
		return func(deploytarget.ServiceGroup) *cluster.GroupScope { return nil }
	}

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
			Cluster:   group.Cluster,
			OwnApps:   own,
			OtherApps: other,
		}
	}
}

// mainClusterForEntities resolves the env's MAIN cluster — the env-level deploy
// target an operator / cronjob (which carry no per-service deploy block) lands
// on. This is the Bundle's declared `cluster_target.cluster` — the same
// resolution firstK8sClusterField / expectedClusterForEnv use for the env-wide
// context. Only a contract with no cluster_target falls back to render order
// (the FIRST cluster-shaped service's `forge.K8sCluster.cluster`), which is
// not safe in general: services render as bundle.services + projected
// workloads, so an image-less infra service on a second cluster can come
// first. Falls back to the first k8s group's cluster when no service entity
// carries a cluster (the manifests-only render shape), and "" when the env
// declares no cluster at all (host-only / compose — nothing to attribute).
func mainClusterForEntities(entities *KCLEntities, groups []deploytarget.ServiceGroup) string {
	if entities != nil {
		// The declared env target wins. See k8sClusterFieldFromEntities
		// for why render order cannot stand in for it.
		if c := entities.ClusterTarget.field("cluster"); c != "" {
			return c
		}
		for _, s := range entities.Services {
			if s.Deploy.Type == "cluster" && s.Deploy.Cluster != nil && s.Deploy.Cluster.Cluster != "" {
				return s.Deploy.Cluster.Cluster
			}
			// A SimpleBackend declares its own cluster, so an env made
			// only of them still has a main cluster for operators and
			// cronjobs to attribute to.
			if s.Deploy.Type == "simple-backend" && s.Deploy.SimpleBackend != nil && s.Deploy.SimpleBackend.Cluster != "" {
				return s.Deploy.SimpleBackend.Cluster
			}
		}
	}
	for _, g := range groups {
		if g.ProviderID == "k8s-cluster" && g.Cluster != "" {
			return g.Cluster
		}
	}
	return ""
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
// the empty-groups direct cluster.Apply, the rollback provider, and the
// deploy preflight. It is the Bundle's declared `cluster_target.cluster`,
// falling back (for a contract that declares none) to the first declared
// K8sCluster cluster (group.Cluster, from KCL `forge.K8sCluster.cluster`) —
// there is no CLI override. Empty when no
// cluster is declared (host-only / compose); the apply chokepoint refuses
// an empty context on a write rather than using kubectl's current one.
//
// A multi-cluster env's per-group dispatch still routes each group to its
// own declared cluster via resolveGroupContext; this single value covers
// only the env-wide single-cluster paths, which already assume one
// namespace per env.
func declaredEnvContext(entities *KCLEntities, groups []deploytarget.ServiceGroup) string {
	// The Bundle's declared env target wins. Group order cannot stand in for
	// it: groups are sorted by `k8s-cluster|<context>|…`, so the "first"
	// group is whichever context sorts lowest, and a zonal GKE context
	// (`…_us-central1-a_…`) sorts ahead of a regional one (`…_us-central1_…`).
	// That once aimed prod's deploy preflight at its secondary daemon cluster.
	if entities != nil {
		if c := entities.ClusterTarget.field("cluster"); c != "" {
			return c
		}
	}
	for _, g := range groups {
		if g.ProviderID == "k8s-cluster" && g.Cluster != "" {
			return g.Cluster
		}
	}
	return ""
}

// hostedTierNames is the refusal's vocabulary: the three deploy tiers a
// control plane runs.
const hostedTierNames = "forge.SimpleBackend (backend), forge.ManagedDatabase (database, on Bundle.databases) or forge.StaticSite (static)"

// buildHostedGroups turns a hosted env's entities into ONE "hosted" group.
//
// Every service must be a tier. A service with no deploy block, or a
// build-only one, is allowed through untouched: it deploys nothing (it may
// only BUILD the image a backend names). Anything else — a K8sCluster, an
// External command, compose, a host process — has no meaning on a control
// plane, and is refused with the tiers named rather than silently dropped: a
// hosted deploy that quietly skipped half the env would report success for an
// environment that is not running.
//
// The group's Hosted target (endpoint, release, digests) is filled in by the
// caller, which owns the credential and the ledger read.
func buildHostedGroups(envName string, entities *KCLEntities) ([]deploytarget.ServiceGroup, error) {
	var (
		services []deploytarget.ResolvedService
		refused  []string
	)
	for _, svc := range entities.Services {
		switch svc.Deploy.Type {
		case "simple-backend":
			if svc.Deploy.SimpleBackend == nil {
				continue
			}
			spec := svc.Deploy.SimpleBackend.Spec
			services = append(services, deploytarget.ResolvedService{
				Name: svc.Name,
				Hosted: &deploytarget.HostedWorkload{
					Tier: deploytarget.HostedTierBackend, Backend: &spec, Artifact: hostedArtifactKey(svc),
				},
			})
		case "", "build-only":
			// Deploys nothing; see the doc comment.
		default:
			refused = append(refused, fmt.Sprintf("%s (deploy type %q)", svc.Name, svc.Deploy.Type))
		}
	}
	for _, o := range entities.Operators {
		refused = append(refused, fmt.Sprintf("%s (operator)", o.Name))
	}
	for _, c := range entities.CronJobs {
		refused = append(refused, fmt.Sprintf("%s (cronjob)", c.Name))
	}
	for _, f := range entities.Frontends {
		switch {
		case f.Deploy == nil || f.Deploy.Type == "":
		case f.Deploy.Type == "static-site" && f.Deploy.StaticSite != nil:
			services = append(services, deploytarget.ResolvedService{
				Name:   f.Name,
				Hosted: &deploytarget.HostedWorkload{Tier: deploytarget.HostedTierStatic, Static: hostedStaticSpec(f.Deploy.StaticSite), Artifact: f.Name},
			})
		default:
			refused = append(refused, fmt.Sprintf("%s (frontend deploy type %q)", f.Name, f.Deploy.Type))
		}
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("env %q is hosted (its Bundle declares control_plane), so every workload must be a deploy tier: %s.\n"+
			"  Not a tier: %s\n"+
			"  fix: redeclare each as a tier, or move it to an env without control_plane",
			envName, hostedTierNames, strings.Join(refused, ", "))
	}
	for _, db := range entities.Databases {
		spec := db.Spec
		services = append(services, deploytarget.ResolvedService{
			Name:   db.Name,
			Hosted: &deploytarget.HostedWorkload{Tier: deploytarget.HostedTierDatabase, Database: &spec},
		})
	}
	if len(services) == 0 {
		return nil, nil
	}
	return []deploytarget.ServiceGroup{{
		Env:        envName,
		ProviderID: deploytarget.HostedProviderID,
		Services:   services,
	}}, nil
}

// hostedStaticSpec projects a StaticSite frontend onto the DEPLOYED half of
// forge's StaticSiteSpec. Build inputs (public_dir, bundle) are consumed by
// `forge build` and are not part of the deployed spec; bucket and cdn are
// refused on a hosted env by the Bundle check, so none reaches here. The
// liveDigest is pinned later, from the bound release.
func hostedStaticSpec(ss *StaticSiteDeploy) *v1alpha1.StaticSiteSpec {
	keep := int32(ss.KeepReleases)
	return &v1alpha1.StaticSiteSpec{BasePath: ss.BasePath, KeepReleases: &keep}
}

// hostedArtifactKey is the ONE rule for which release artifact a hosted
// backend's digest is bound under, shared by `forge release cut` (which
// records it) and the hosted deploy (which pins by it):
//
//   - the service's own `image`, when declared — that is the key forge's
//     build state records a pushed image under, so a backend whose image
//     this project builds resolves to the digest the build captured;
//   - otherwise the spec image's last path segment
//     (deploytarget.HostedArtifactName), for an image built elsewhere.
func hostedArtifactKey(svc ServiceEntity) string {
	if svc.Image != "" {
		return svc.Image
	}
	if svc.Deploy.SimpleBackend == nil {
		return ""
	}
	return deploytarget.HostedArtifactName(svc.Deploy.SimpleBackend.Spec.Image)
}
