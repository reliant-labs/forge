package bundle

import (
	"fmt"
	"slices"
	"sort"

	"github.com/reliant-labs/forge/pkg/release"
)

// PlacementDeclaration is logical release dependency input, never a readiness claim.
// The actual keys and binding hashes are projected from the sealed parse by Build.
type PlacementDeclaration struct {
	Workload     string
	Runtime      string
	Actuator     string
	Dependencies []string
	Unit         string
}

func placementPrerequisites(in BuildInput, docs []parsedDoc, objects []release.ShapeObject, trees []release.BundleTree) (*release.PlacementPrerequisites, error) {
	cluster, hosted := false, false
	for _, o := range objects {
		if o.Cluster == release.BundleHostedCluster {
			hosted = true
		} else if o.Cluster != "" {
			cluster = true
		}
	}
	for _, d := range in.Placements {
		cluster = cluster || d.Runtime == "cluster"
		hosted = hosted || d.Runtime == "hosted"
	}
	if !cluster || !hosted {
		return nil, nil
	}
	if !in.Reader.Supports(release.BundleFeaturePlacementPrerequisites) {
		return nil, fmt.Errorf("mixed placements require reader feature %s before composition or publication; upgrade the recording control plane", release.BundleFeaturePlacementPrerequisites)
	}
	if in.Placements == nil {
		return nil, fmt.Errorf("mixed placements require complete placement dependency declarations")
	}
	p := &release.PlacementPrerequisites{Version: 1, Items: []release.PlacementItem{}, Edges: []release.PlacementEdge{}, Bindings: []release.PlacementBinding{}}
	if in.Snapshot != nil {
		var err error
		in.Placements, err = snapshotPlacements(*in.Snapshot, in.Placements)
		if err != nil {
			return nil, err
		}
	}
	decl := map[string]PlacementDeclaration{}
	for _, d := range in.Placements {
		if _, ok := decl[d.Workload]; ok {
			return nil, fmt.Errorf("duplicate placement workload %q", d.Workload)
		}
		if !((d.Runtime == "cluster" && d.Actuator == "forge") || (d.Runtime == "hosted" && d.Actuator == "platform")) {
			return nil, fmt.Errorf("unsupported mixed placement %s: %s/%s", d.Workload, d.Runtime, d.Actuator)
		}
		decl[d.Workload] = d
	}
	ownersOf, err := placementOwners(docs, decl)
	if err != nil {
		return nil, err
	}
	owners := func(o release.ShapeObject) []string {
		if o.Cluster == release.BundleHostedCluster {
			return []string{o.Name} // a hosted record is its own component
		}
		return ownersOf[o.Key()]
	}
	keys := map[string][]release.PlacementKey{}
	// Only actual controller/Job and hosted record identities are items. Helpers
	// bind to their logical component, never invented pseudo-status objects.
	for _, o := range objects {
		if !placementPrimary(o.Kind, o.Cluster == release.BundleHostedCluster) {
			continue
		}
		declared := []string{}
		for _, owner := range owners(o) {
			if _, ok := decl[owner]; ok {
				declared = append(declared, owner)
			}
		}
		if len(declared) == 0 {
			continue
		}
		if len(declared) > 1 {
			return nil, fmt.Errorf("unsupported mixed placement: controller %s/%s has several release owners %v", o.Kind, o.Name, declared)
		}
		workload := declared[0]
		d := decl[workload]
		runtime, actuator := "cluster", "forge"
		if o.Cluster == release.BundleHostedCluster {
			runtime, actuator = "hosted", "platform"
		}
		if runtime != d.Runtime || actuator != d.Actuator {
			return nil, fmt.Errorf("placement declaration and sealed object disagree for %s", workload)
		}
		k := release.PlacementKey{Runtime: runtime, Actuator: actuator, Cluster: o.Cluster, Namespace: o.Namespace, Kind: o.Kind, Name: o.Name}
		keys[workload] = append(keys[workload], k)
		p.Items = append(p.Items, release.PlacementItem{Key: k, Workload: workload})
	}
	for _, d := range in.Placements {
		if len(keys[d.Workload]) == 0 {
			return nil, fmt.Errorf("placement workload %q has no sealed controller/record", d.Workload)
		}
	}
	edgeSet := map[release.PlacementEdge]bool{}
	add := func(a, b release.PlacementKey) {
		e := release.PlacementEdge{Predecessor: a, Dependent: b}
		if !edgeSet[e] {
			edgeSet[e] = true
			p.Edges = append(p.Edges, e)
		}
	}
	for _, d := range in.Placements {
		for _, peer := range d.Dependencies {
			if len(keys[peer]) == 0 {
				return nil, fmt.Errorf("placement %s has missing predecessor %q", d.Workload, peer)
			}
			for _, a := range keys[peer] {
				for _, b := range keys[d.Workload] {
					add(a, b)
				}
			}
		}
	}
	// Unit order is the sealed trees' own (release.PlanTrees over the complete
	// inventory, typos already refused there): the implicit base dependency
	// exists only when base renders, and a unit holding only support objects
	// passes ordering through without becoming an invented readiness item.
	unitItems := map[string][]release.PlacementKey{}
	objectUnits := map[release.ObjectKey]string{}
	for _, doc := range docs {
		for _, c := range doc.clusters {
			objectUnits[release.ObjectKey{Cluster: c, Kind: doc.meta.kind, Namespace: doc.meta.namespace, Name: doc.meta.name}] = docUnit(doc)
		}
	}
	for _, item := range p.Items {
		k := item.Key
		if k.Runtime == "hosted" {
			continue // hosted records are flat: no delivery unit
		}
		u := objectUnits[release.ObjectKey{Cluster: k.Cluster, Kind: k.Kind, Namespace: k.Namespace, Name: k.Name}]
		unitItems[u] = append(unitItems[u], k)
	}
	treeAt := map[string]release.BundleTree{}
	for _, t := range trees {
		treeAt[t.Path] = t
	}
	unitAfter := map[string]map[string]bool{}
	for _, t := range trees {
		for _, path := range t.DependsOn {
			if dep, ok := treeAt[path]; ok && dep.Unit != t.Unit {
				if unitAfter[t.Unit] == nil {
					unitAfter[t.Unit] = map[string]bool{}
				}
				unitAfter[t.Unit][dep.Unit] = true
			}
		}
	}
	// predecessors are the nearest units with items, walking through
	// support-only units; their own predecessors order them transitively.
	var predecessors func(unit string, seen map[string]bool) []release.PlacementKey
	predecessors = func(unit string, seen map[string]bool) []release.PlacementKey {
		var out []release.PlacementKey
		for dep := range unitAfter[unit] {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if len(unitItems[dep]) > 0 {
				out = append(out, unitItems[dep]...)
			} else {
				out = append(out, predecessors(dep, seen)...)
			}
		}
		return out
	}
	for unit, items := range unitItems {
		for _, a := range predecessors(unit, map[string]bool{}) {
			for _, b := range items {
				add(a, b)
			}
		}
	}
	// Bind the full component inventory and broadcast pre-rollout Jobs/configs.
	// Secret documents have already been removed and are never inspected here.
	bindingSet := map[release.PlacementBinding]bool{}
	for _, o := range objects {
		var targets []release.PlacementKey
		for _, owner := range owners(o) {
			if len(keys[owner]) == 0 {
				return nil, fmt.Errorf("sealed object %s/%s has undeclared placement owner %q", o.Kind, o.Name, owner)
			}
			targets = append(targets, keys[owner]...)
		}
		if len(owners(o)) == 0 && o.Cluster != release.BundleHostedCluster && (placementPrimary(o.Kind, false) || o.Kind == "ConfigMap") {
			for _, item := range p.Items {
				if item.Key.Cluster == o.Cluster && item.Key.Namespace == o.Namespace && item.Key.Runtime == "cluster" {
					targets = append(targets, item.Key)
				}
			}
			if len(targets) == 0 {
				return nil, fmt.Errorf("unbound broadcast prerequisite %s/%s", o.Kind, o.Name)
			}
		}
		if o.Cluster == release.BundleHostedCluster && len(targets) == 0 {
			return nil, fmt.Errorf("unsupported/unbound hosted sealed record %s/%s", o.Kind, o.Name)
		}
		if o.Cluster != release.BundleHostedCluster && !placementPrimary(o.Kind, false) && o.Kind != "ConfigMap" {
			if !placementPassiveKinds[o.Kind] {
				return nil, fmt.Errorf("unsupported mixed prerequisite resource kind %s (%s)", o.Kind, o.Name)
			}
			continue // applied inventory, not an invented readiness status
		}
		for _, k := range targets {
			if k.Cluster != o.Cluster || k.Namespace != o.Namespace {
				continue
			}
			b := release.PlacementBinding{Item: k, Object: release.PlacementObject{Cluster: o.Cluster, APIVersion: o.APIVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}, ManifestHash: o.Hash}
			if o.Kind == "Job" {
				b.JobContract = release.KubernetesJobCompleteV1
			}
			if !bindingSet[b] {
				bindingSet[b] = true
				p.Bindings = append(p.Bindings, b)
			}
		}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	// The v1 actuator can hold one hosted group after a forge prerequisite
	// closure. Hosted->forge and hosted interleaving require a future scheduler.
	for _, e := range p.Edges {
		if e.Predecessor.Runtime == "hosted" {
			return nil, fmt.Errorf("unsupported mixed placement edge %s -> %s: hosted predecessor/interleaving", e.Predecessor.Name, e.Dependent.Name)
		}
	}
	sort.Slice(p.Items, func(i, j int) bool { return placementKeyOrder(p.Items[i].Key) < placementKeyOrder(p.Items[j].Key) })
	sort.Slice(p.Edges, func(i, j int) bool {
		a, b := p.Edges[i], p.Edges[j]
		return placementKeyOrder(a.Predecessor)+"|"+placementKeyOrder(a.Dependent) < placementKeyOrder(b.Predecessor)+"|"+placementKeyOrder(b.Dependent)
	})
	sort.Slice(p.Bindings, func(i, j int) bool {
		a, b := p.Bindings[i], p.Bindings[j]
		return placementKeyOrder(a.Item)+fmt.Sprint(a.Object) < placementKeyOrder(b.Item)+fmt.Sprint(b.Object)
	})
	return p, nil
}

// Frozen consumer declarations, not the current checkout, govern carried peers.
func snapshotPlacements(s release.Snapshot, declarations []PlacementDeclaration) ([]PlacementDeclaration, error) {
	byName := map[string]PlacementDeclaration{}
	for _, d := range declarations {
		byName[d.Workload] = d
	}
	var servers []string
	for _, c := range s.Consumers {
		for _, slot := range c.Slots {
			if slot != "frontend" && slot != "build-only" {
				servers = append(servers, c.Key.Workload)
				break
			}
		}
	}
	var out []PlacementDeclaration
	for _, c := range s.Consumers {
		// A build-only artifact has no runtime placement: it stays in the
		// snapshot's artifacts and is no prerequisite item.
		if slices.Equal(c.Slots, []string{"build-only"}) {
			continue
		}
		d, ok := byName[c.Key.Workload]
		if !ok {
			return nil, fmt.Errorf("snapshot consumer %s lacks placement declaration", c.Key.Workload)
		}
		d.Dependencies = nil
		frontend := false
		for _, slot := range c.Slots {
			frontend = frontend || slot == "frontend"
		}
		if frontend && !c.DependenciesDeclared {
			d.Dependencies = append(d.Dependencies, servers...)
		} else {
			for _, peer := range c.DependsOn {
				d.Dependencies = append(d.Dependencies, peer.Consumer.Workload)
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// placementPassiveKinds is the closed set of applied-inventory kinds a mixed
// bundle may carry without a readiness item of their own: their effect on
// ordinary rollout readiness is observed through the owning component's
// healthy Pods (an ExternalSecret or Certificate a Pod mounts, a route to
// it). Any other kind is refused rather than silently treated as ready.
var placementPassiveKinds = map[string]bool{
	"Namespace": true, "Service": true, "ServiceAccount": true, "Role": true, "RoleBinding": true,
	"ClusterRole": true, "ClusterRoleBinding": true, "PersistentVolumeClaim": true, "NetworkPolicy": true,
	"Ingress": true, "PodDisruptionBudget": true, "HorizontalPodAutoscaler": true,
	// Gateway API and Envoy Gateway policy attachments.
	"Gateway": true, "HTTPRoute": true, "ReferenceGrant": true, "BackendTLSPolicy": true,
	"BackendTrafficPolicy": true, "ClientTrafficPolicy": true,
	// External Secrets Operator and cert-manager: names and references only;
	// no Secret value is read or recorded.
	"ExternalSecret": true, "SecretStore": true, "Issuer": true, "Certificate": true,
}

// placementOwners is each sealed object's owners by releaseOwnershipOf, the
// rule SnapshotInventory uses. A forge-managed app name counts only when it
// names a declared component; an object with no owner is env-level.
func placementOwners(docs []parsedDoc, declared map[string]PlacementDeclaration) (map[release.ObjectKey][]string, error) {
	out := map[release.ObjectKey][]string{}
	for _, doc := range docs {
		body, _ := doc.body.(map[string]any)
		own, err := releaseOwnershipOf(mapAt(body, "metadata"))
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", doc.meta.kind, doc.meta.name, err)
		}
		names := own.Owners
		if own.Owners == nil && own.Label != "" {
			if _, ok := declared[own.Label]; own.Explicit || ok {
				names = []string{own.Label}
			}
		}
		for _, c := range doc.clusters {
			out[release.ObjectKey{Cluster: c, Kind: doc.meta.kind, Namespace: doc.meta.namespace, Name: doc.meta.name}] = names
		}
	}
	return out, nil
}

func placementPrimary(kind string, hosted bool) bool {
	if hosted {
		return kind == "StaticSite" || kind == "Workload" || kind == "ManagedDatabase"
	}
	switch kind {
	case "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Pod", "Job":
		return true
	}
	return false
}

func placementKeyOrder(k release.PlacementKey) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s", k.Runtime, k.Actuator, k.Cluster, k.Namespace, k.Kind, k.Name)
}
