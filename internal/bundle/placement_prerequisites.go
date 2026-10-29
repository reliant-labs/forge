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
	keys := map[string][]release.PlacementKey{}
	// Only actual controller/Job and hosted record identities are items. Helpers
	// bind to their logical component, never invented pseudo-status objects.
	for _, o := range objects {
		workload := o.Workload
		if o.Cluster == release.BundleHostedCluster && workload == "" {
			workload = o.Name
		}
		d, ok := decl[workload]
		if !ok {
			continue
		}
		if !placementPrimary(o.Kind, o.Cluster == release.BundleHostedCluster) {
			continue
		}
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
		workload := o.Workload
		if o.Cluster == release.BundleHostedCluster && workload == "" {
			workload = o.Name
		}
		targets := keys[workload]
		if workload != "" && len(targets) == 0 {
			return nil, fmt.Errorf("sealed object %s/%s has undeclared placement workload %q", o.Kind, o.Name, workload)
		}
		if workload == "" && o.Cluster != release.BundleHostedCluster && (placementPrimary(o.Kind, false) || o.Kind == "ConfigMap") {
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
			switch o.Kind {
			case "Namespace", "Service", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "PersistentVolumeClaim", "NetworkPolicy", "Ingress", "PodDisruptionBudget", "HorizontalPodAutoscaler":
				continue // applied inventory, not an invented readiness status
			default:
				return nil, fmt.Errorf("unsupported mixed prerequisite resource kind %s (%s)", o.Kind, o.Name)
			}
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
