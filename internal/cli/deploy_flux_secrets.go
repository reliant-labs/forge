package cli

// SECRETS ON THE FLUX PATH: synced by forge, never carried by the bundle.
//
// A bundle redacts every Secret value (F-13) and that stays. But redaction made
// a Secret in the manifest layer actively harmful: the in-cluster reconciler
// applied the redaction marker over the cluster's real value, and — with prune
// on — owned the object's lifetime. So a Secret is not in the layer at all
// (bundle.splitSecrets); the bundle's document NAMES the ones the env requires.
//
// This file is the other half. `forge env deploy` on the Flux path syncs those
// Secrets to each cluster itself, BEFORE it writes or re-points the Flux source,
// so a workload Flux rolls out never starts against a missing Secret. Values
// come from the same places `forge env secrets sync` reads (the render's own
// Secret documents, and the env's secret store / provider) and never touch the
// registry.
//
// IDEMPOTENT: server-side apply under the field manager `forge-secrets`, so a
// re-deploy is a no-op and the manager name says who wrote the fields.
//
// NEVER DELETES. A Secret dropped from the KCL stops being named by the next
// bundle, so nothing syncs it again — but forge does not remove it and Flux
// does not own it (it is not in the layer, so nothing can prune it). Deleting
// one is the user's call: `kubectl delete secret`.
//
// A hub-reconciled (ControlPlane) env is not this path: forge holds no context
// for the clusters the hub's Flux applies to, so it cannot write there. See
// [refuseUnsyncableHostedSecrets].

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// fluxSecretFieldManager is the SSA field manager every synced Secret is
// written under.
const fluxSecretFieldManager = "forge-secrets"

// fluxSecretApply is the cluster-touching half, a seam so a test can state what
// reached each cluster without one. Production is cluster.ApplySecrets.
var fluxSecretApply = cluster.ApplySecrets

// fluxSecretNamespaceEnsure creates the Secret's namespace if the reconciler
// has not yet (it applies the Namespace object later, from the bundle).
var fluxSecretNamespaceEnsure = cluster.EnsureNamespace

// fluxSecretSet is the Secrets to sync, keyed by destination.
type fluxSecretSet map[fluxSecretDest]map[string]map[string]any

type fluxSecretDest struct{ cluster, namespace string }

func (s fluxSecretSet) add(cluster, namespace string, manifest map[string]any) {
	dest := fluxSecretDest{cluster, namespace}
	if s[dest] == nil {
		s[dest] = map[string]map[string]any{}
	}
	meta, _ := manifest["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	s[dest][name] = manifest
}

// envSyncedSecretRefs is the NAMES of the Secrets an env needs forge to sync:
// the render's own `kind: Secret` documents plus the ones forge projects from
// the env's secret store (declared rendered Secrets, a value-resolving
// provider's secret_refs). No values.
func envSyncedSecretRefs(entities *KCLEntities, groups []deploytarget.ServiceGroup, namespace, manifests string) ([]release.BundleSecretRef, error) {
	var refs []release.BundleSecretRef
	docs, err := bundle.SecretDocuments(manifests)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		clusters := d.Clusters
		if len(clusters) == 0 {
			clusters = []string{""}
		}
		for _, c := range clusters {
			refs = append(refs, release.BundleSecretRef{Cluster: c, Namespace: d.Namespace, Name: d.Name})
		}
	}
	for _, p := range placeDeclaredSecrets(entities, groups, namespace) {
		for _, s := range p.secrets {
			refs = append(refs, release.BundleSecretRef{Cluster: p.cluster, Namespace: p.namespace, Name: s.Name})
		}
	}
	if providerResolvesValues(entities) {
		for _, p := range placeProviderSecretRefs(entities, groups, namespace, "") {
			seen := map[string]bool{}
			for _, r := range p.refs {
				if r.SecretName != "" && !seen[r.SecretName] {
					seen[r.SecretName] = true
					refs = append(refs, release.BundleSecretRef{Cluster: p.cluster, Namespace: p.namespace, Name: r.SecretName})
				}
			}
		}
	}
	return refs, nil
}

func providerResolvesValues(entities *KCLEntities) bool {
	if entities == nil || entities.SecretProvider == nil || entities.SecretProvider.Type == "rendered" {
		return false
	}
	prov, err := secretProviderFromEntities(entities, projectDirForKCL())
	return err == nil && prov.Kind() == "file"
}

// collectFluxSecrets gathers every Secret a Flux-path deploy syncs, VALUES
// INCLUDED, from the current render and the env's secret store.
//
// Only Secrets the bundle NAMES are returned: the bundle is the contract for
// what the env requires, and a Secret in the render that the recorded bundle
// does not name belongs to a newer render than the one being deployed. A named
// Secret nothing can supply is an error, said before anything is written.
func collectFluxSecrets(ctx context.Context, env string, entities *KCLEntities, named []release.BundleSecretRef, targetClusters []string) (fluxSecretSet, error) {
	namespace := k8sClusterNamespaceForEnv(ctx, env)
	if namespace == "" {
		namespace = env
		if store, err := loadProjectStore(); err == nil {
			namespace = store.Meta().Name + "-" + env
		}
	}
	groups, err := buildDeployGroups(env, entities, namespace)
	if err != nil {
		return nil, fmt.Errorf("group services: %w", err)
	}

	have := fluxSecretSet{}
	// Provider + declared Secrets, through the same code `forge env secrets
	// sync` runs, collecting instead of applying.
	collect := secretTarget{allowRemote: true, sink: func(_ context.Context, p secretPlacement) error {
		for _, m := range p.mans {
			for _, c := range clustersOrAll(clusterList(p.cluster), targetClusters) {
				have.add(c, p.namespace, m)
			}
		}
		return nil
	}}
	if err := applyK8sSecretsFromProviderTo(ctx, entities, groups, namespace, "", env, false, collect); err != nil {
		return nil, err
	}
	// The render's own Secret documents, with their real values.
	doc, err := projectEnvShapeFn(ctx, io.Discard, env)
	if err != nil {
		return nil, fmt.Errorf("render env %s's Secrets: %w", env, err)
	}
	renderDocs, err := bundle.SecretDocuments(doc.manifests)
	if err != nil {
		return nil, err
	}
	for _, d := range renderDocs {
		for _, c := range clustersOrAll(d.Clusters, targetClusters) {
			ns := d.Namespace
			if ns == "" {
				ns = namespace
			}
			body := cloneTopLevel(d.Body)
			setSecretNamespace(body, ns)
			have.add(c, ns, body)
		}
	}

	out := fluxSecretSet{}
	var missing []string
	for _, ref := range named {
		for _, c := range clustersOrAll(clusterList(ref.Cluster), targetClusters) {
			ns := ref.Namespace
			if ns == "" {
				ns = namespace
			}
			manifest, ok := have[fluxSecretDest{c, ns}][ref.Name]
			if !ok {
				missing = append(missing, fmt.Sprintf("%s/%s in %s", ns, ref.Name, c))
				continue
			}
			out.add(c, ns, manifest)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"env %s's bundle names Secret(s) this machine cannot supply a value for: %s\n"+
				"  Secrets are synced by forge from the env's secret store, never from the bundle.\n"+
				"  Set the missing value(s) in the store (forge secret ensure --env %s), then re-run the deploy",
			env, strings.Join(missing, ", "), env)
	}
	return out, nil
}

func clusterList(c string) []string {
	if c == "" {
		return nil
	}
	return []string{c}
}

// clustersOrAll is the clusters a document is routed to, or every target
// cluster when it was attributed to none.
func clustersOrAll(routed, all []string) []string {
	if len(routed) > 0 {
		return routed
	}
	return all
}

func cloneTopLevel(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	if meta, ok := in["metadata"].(map[string]any); ok {
		m := make(map[string]any, len(meta))
		for k, v := range meta {
			m[k] = v
		}
		out["metadata"] = m
	}
	return out
}

func setSecretNamespace(body map[string]any, namespace string) {
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		body["metadata"] = meta
	}
	meta["namespace"] = namespace
}

// syncFluxSecrets writes every collected Secret to its cluster and prints one
// line per cluster: a count, never a value.
func syncFluxSecrets(ctx context.Context, env string, set fluxSecretSet, out io.Writer) error {
	byCluster := map[string][]fluxSecretDest{}
	for dest := range set {
		byCluster[dest.cluster] = append(byCluster[dest.cluster], dest)
	}
	clusters := make([]string, 0, len(byCluster))
	for c := range byCluster {
		clusters = append(clusters, c)
	}
	sort.Strings(clusters)

	for _, c := range clusters {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("env %s has Secret(s) to sync with no cluster to put them in: declare the consuming workload's cluster (forge.ClusterTarget) in the env's KCL", env)
		}
		dests := byCluster[c]
		sort.Slice(dests, func(i, j int) bool { return dests[i].namespace < dests[j].namespace })
		count := 0
		for _, dest := range dests {
			if err := fluxSecretNamespaceEnsure(ctx, dest.cluster, dest.namespace); err != nil {
				return fmt.Errorf("ensure namespace %q in %q before syncing Secrets: %w", dest.namespace, dest.cluster, err)
			}
			names := make([]string, 0, len(set[dest]))
			for name := range set[dest] {
				names = append(names, name)
			}
			sort.Strings(names)
			mans := make([]map[string]any, 0, len(names))
			for _, name := range names {
				mans = append(mans, set[dest][name])
			}
			stream, err := marshalManifestStream(mans)
			if err != nil {
				return fmt.Errorf("render Secrets for %s/%s: %w", dest.cluster, dest.namespace, err)
			}
			if err := fluxSecretApply(ctx, dest.cluster, dest.namespace, fluxSecretFieldManager, stream); err != nil {
				return fmt.Errorf("sync Secrets to %s: %w", dest.cluster, err)
			}
			count += len(names)
		}
		fmt.Fprintf(out, "  %s: synced %d secret(s) (%s)\n", c, count, fluxSecretFieldManager)
	}
	return nil
}

// printFluxSecretPlan lists, by name only, what a deploy would sync.
func printFluxSecretPlan(out io.Writer, env string, named []release.BundleSecretRef, targetClusters []string) {
	if len(named) == 0 {
		return
	}
	lines := map[string]bool{}
	for _, ref := range named {
		for _, c := range clustersOrAll(clusterList(ref.Cluster), targetClusters) {
			lines[fmt.Sprintf("  %s: %s", c, secretRefLabel(ref))] = true
		}
	}
	sorted := make([]string, 0, len(lines))
	for l := range lines {
		sorted = append(sorted, l)
	}
	sort.Strings(sorted)
	fmt.Fprintf(out, "\n%s would sync %d Secret object(s) to its clusters before pointing its reconciler at the bundle (names only):\n", env, len(sorted))
	for _, l := range sorted {
		fmt.Fprintln(out, l)
	}
}

func secretRefLabel(ref release.BundleSecretRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "/" + ref.Name
}

// refuseUnsyncableHostedSecrets stops a hub-reconciled deploy that would ship
// without Secrets its workloads need.
//
// The hub's Flux applies to a cluster forge holds no kubectl context for, so
// forge cannot sync a value there, and a Secret is not in the bundle to carry
// one. Provider-backed secrets (ExternalSecrets / OpenBao references) are
// non-Secret objects and ride the bundle as references, needing nothing. A
// plain store value has no transport on this path, so say so rather than
// deploy a workload that starts without it.
func refuseUnsyncableHostedSecrets(env string, refs []release.BundleSecretRef) error {
	if len(refs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, r := range refs {
		label := secretRefLabel(r)
		if !seen[label] {
			seen[label] = true
			names = append(names, label)
		}
	}
	sort.Strings(names)
	return fmt.Errorf(
		"env %s is reconciled by its control plane, and it needs Secret value(s) forge cannot deliver: %s\n"+
			"  A bundle never carries a Secret, and forge holds no kubectl context for the clusters the control plane's\n"+
			"  reconciler applies to. Either:\n"+
			"    - back them with a provider reference (forge.ExternalSecret / OpenBao), which rides the bundle as a\n"+
			"      non-secret object; or\n"+
			"    - run `forge env secrets sync %s` against each cluster yourself before deploying",
		env, strings.Join(names, ", "), env)
}

// printFluxSecretExplain is `forge env deploy <env> --explain`'s answer to
// "which Secrets will this sync?": names only, and only for an env that
// reconciles through Flux. Silent for every other env, and on any render
// failure — explain is a read-only question, not a gate.
func printFluxSecretExplain(ctx context.Context, env string) {
	ledger, err := ledgerFor(ctx, projectDirForKCL(), env)
	if err != nil {
		return
	}
	reconciled, entities := fluxReconciledEnv(ctx, env, ledger)
	if !reconciled {
		return
	}
	doc, err := projectEnvShapeFn(ctx, io.Discard, env)
	if err != nil {
		return
	}
	refs, err := envSyncedSecretRefs(entities, nil, k8sClusterNamespaceForEnv(ctx, env), doc.manifests)
	if err != nil {
		return
	}
	printFluxSecretPlan(os.Stdout, env, refs, fluxTargetClustersFromEntities(entities))
}

// fluxTargetClustersFromEntities is the clusters an env declares, for naming
// where an unattributed Secret would be synced before a bundle exists.
func fluxTargetClustersFromEntities(entities *KCLEntities) []string {
	declared := declaredFluxClusters(entities)
	sort.Strings(declared)
	return declared
}

// fluxKubeconfigSecrets is the declared cross-cluster kubeconfig Secrets a
// Flux-path deploy mints: every declaration whose consumer cluster is one the
// env declares a kubectl context for. A declaration for a cluster forge holds
// no context for cannot be written from here and is left to the hub.
func fluxKubeconfigSecrets(entities *KCLEntities) []KubeconfigSecretEntity {
	if entities == nil {
		return nil
	}
	declared := map[string]bool{}
	for _, c := range declaredFluxClusters(entities) {
		declared[c] = true
	}
	var out []KubeconfigSecretEntity
	for _, k := range entities.KubeconfigSecrets {
		if declared[strings.TrimSpace(k.InCluster)] {
			out = append(out, k)
		}
	}
	return out
}

func printFluxKubeconfigPlan(out io.Writer, env string, secrets []KubeconfigSecretEntity) {
	if len(secrets) == 0 {
		return
	}
	fmt.Fprintf(out, "Would mint %d cross-cluster kubeconfig Secret(s) for %s (contents never printed):\n", len(secrets), env)
	for _, k := range secrets {
		fmt.Fprintf(out, "  %s in %s (target=%s)\n", k.Name, k.InCluster, k.TargetCluster)
	}
}

// fluxDeployNamespace is the namespace Secrets default to for an env, resolved
// the same way [collectFluxSecrets] does.
func fluxDeployNamespace(ctx context.Context, env string) string {
	if ns := k8sClusterNamespaceForEnv(ctx, env); ns != "" {
		return ns
	}
	if store, err := loadProjectStore(); err == nil {
		return store.Meta().Name + "-" + env
	}
	return env
}
