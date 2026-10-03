package cli

// Binding a RENDERED cluster to a CONNECTED one, and refusing the env whose
// declaration cannot be shipped.
//
// A forge bundle may render for several clusters at once (one subtree per
// cluster, keyed by release.BundleClusterPath), so "which cluster does this
// environment deploy to" has more than one answer and the control plane takes
// a repeated field. A binding is one pair: the rendered cluster key — the
// kubectl context, as it appears in the bundle's ClusterPaths — and the
// connected cluster's name.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// resolveClusterBindings turns the env's declared connected-cluster NAMES
// into the ids the wire carries, with one ListClusters call.
//
// ONE CALL, NOT ONE PER BINDING: a multi-cluster env would otherwise make a
// request per target to learn ids it could have read together, and the list is
// already org-scoped.
//
// A name the org does not have is an ERROR, not a dropped binding. Silently
// omitting it would record the environment with no target and report success
// — which is exactly the "accepted and never lands" outcome the render
// refusal exists to prevent.
func resolveClusterBindings(ctx context.Context, c cloudCaller, e *KCLEntities) ([]deploytarget.ClusterBinding, error) {
	declared := clusterBindingsOf(e)
	if len(declared) == 0 {
		return nil, nil
	}
	var resp struct {
		Clusters []wireConnectedCluster `json:"clusters"`
	}
	if err := c.Call(ctx, procListClusters, map[string]any{}, &resp); err != nil {
		return nil, fmt.Errorf("list the organization's connected clusters: %w", err)
	}
	idByName := make(map[string]string, len(resp.Clusters))
	known := make([]string, 0, len(resp.Clusters))
	for _, cl := range resp.Clusters {
		idByName[cl.Name] = cl.ID
		known = append(known, cl.Name)
	}
	out := make([]deploytarget.ClusterBinding, 0, len(declared))
	for _, b := range declared {
		id, ok := idByName[b.ConnectedCluster]
		if !ok {
			sort.Strings(known)
			return nil, fmt.Errorf("cluster %q targets connected cluster %q, which this organization has not "+
				"connected (it has: %s)\n"+
				"fix: forge cluster connect %s --context %s --env <env>",
				b.Cluster, b.ConnectedCluster, strings.Join(known, ", "), b.ConnectedCluster, b.Cluster)
		}
		out = append(out, deploytarget.ClusterBinding{Cluster: b.Cluster, ClusterID: id})
	}
	return out, nil
}

// clusterBindingsOf derives the env's rendered-cluster → connected-cluster
// bindings from its KCL.
//
// Keyed on the CONTEXT rather than the connected name, because the context is
// the bundle's own key and the thing the control plane matches against. Two
// workloads on the same target produce one binding, and the result is sorted
// so a request's bytes do not depend on map iteration order.
func clusterBindingsOf(e *KCLEntities) []clusterBinding {
	if e == nil {
		return nil
	}
	byContext := map[string]string{}
	note := func(context, connected string) {
		if context != "" && connected != "" {
			byContext[context] = connected
		}
	}
	note(e.ClusterTarget.field("cluster"), connectedClusterOf(e.ClusterTarget))
	for _, w := range e.Workloads {
		if w.Runtime.Type == RuntimeCluster && w.Runtime.Cluster != nil {
			note(w.Runtime.Cluster.Cluster, w.Runtime.Cluster.ConnectedCluster)
		}
	}
	out := make([]clusterBinding, 0, len(byContext))
	for context, connected := range byContext {
		out = append(out, clusterBinding{Cluster: context, ConnectedCluster: connected})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out
}

// clusterBinding is one rendered cluster and the connected cluster it maps
// to, before the connected NAME has been resolved to the id the wire carries.
type clusterBinding struct {
	// Cluster is the rendered cluster / kube-context name — the bundle's own
	// key (release.BundleClusterPath).
	Cluster string
	// ConnectedCluster is the name `forge cluster connect` registered.
	ConnectedCluster string
}

func connectedClusterOf(t *ClusterTargetEntity) string {
	if t == nil {
		return ""
	}
	return t.ConnectedCluster
}

// refuseUnboundClusterTargets refuses, AT RENDER, an env that declares a
// control plane and targets a real cluster with no connected cluster named.
//
// WHY THIS IS A RENDER-TIME REFUSAL rather than a deploy-time one. The env's
// declaration is what the control plane records, and a declaration naming a
// cluster the platform cannot reach is a deploy that is accepted and never
// lands — the failure arrives later, as a Flux reconcile error about a
// credential, two layers from the missing field. Refusing while the author is
// still looking at the KCL costs nothing and names the exact fix.
//
// A k3d CONTEXT IS EXEMPT, and that exemption is the common case: forge
// applies to a local cluster directly and the control plane never dials it, so
// requiring a registration for one would mean connecting a cluster that only
// exists on one laptop.
func refuseUnboundClusterTargets(envName string, e *KCLEntities) error {
	if e == nil || e.ControlPlane == nil {
		return nil
	}
	unbound := map[string]bool{}
	consider := func(context, connected string) {
		if context == "" || connected != "" || isForgeManagedContext(context) {
			return
		}
		unbound[context] = true
	}
	consider(e.ClusterTarget.field("cluster"), connectedClusterOf(e.ClusterTarget))
	for _, w := range e.Workloads {
		if w.Runtime.Type == RuntimeCluster && w.Runtime.Cluster != nil {
			consider(w.Runtime.Cluster.Cluster, w.Runtime.Cluster.ConnectedCluster)
		}
	}
	if len(unbound) == 0 {
		return nil
	}
	contexts := make([]string, 0, len(unbound))
	for c := range unbound {
		contexts = append(contexts, c)
	}
	sort.Strings(contexts)

	return fmt.Errorf("env %q declares a control plane and targets cluster %s, which names no "+
		"connected cluster.\n"+
		"The control plane would record this deploy with nowhere to send it: it holds no address, "+
		"CA or credential for a cluster it has not been told about.\n"+
		"fix: register the cluster once, then name it in the target:\n\n"+
		"    forge cluster connect <name> --context %s --env %s\n\n"+
		"    forge.ClusterTarget {\n        cluster = %q\n        namespace = \"...\"\n        connected_cluster = \"<name>\"\n    }\n\n"+
		"(a k3d cluster needs none of this — forge applies to it directly)",
		envName, strings.Join(quoteEach(contexts), ", "), contexts[0], envName, contexts[0])
}

// isForgeManagedContext reports whether a context is a local cluster forge
// creates and can delete. Mirrors kcl/render.k's _is_forge_managed_context
// prefix rule, which is the same signal gateway.k's _is_local_cluster uses.
func isForgeManagedContext(context string) bool {
	return strings.HasPrefix(context, "k3d-") || strings.HasPrefix(context, "kind-")
}

func quoteEach(items []string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return out
}
