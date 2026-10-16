package cluster

// Chart prune — removing the objects a helm-rendered platform dependency
// STOPPED rendering.
//
// `kubectl apply` adds and updates; it never deletes. A chart whose rendered
// set shrinks between two versions (the Flux chart's ClusterRoles and NetworkPolicies
// are the case that surfaced it) therefore left the departed objects live
// forever: a ClusterRole that grants impersonation, a NetworkPolicy that
// opens an ingress path, long after the declaration that justified them was
// deleted. The Deployment prune and the CRD prune never covered them — they
// look at one kind each.
//
// So a chart apply now ends with a prune, and it is deliberately narrow. An
// object is deleted only when ALL hold:
//
//  1. forge stamped it as THIS chart's, for THIS (project, env):
//     `forge.dev/chart-owner=<project>.<env>` plus
//     `forge.dev/workload=<chart>`. The listing is SERVER-SIDE
//     label-selected on both, so an object forge did not apply, another
//     env's, another chart's, or a hand-made one is never even a candidate.
//     The stamp is written by stampDocAppLabel on the render path and
//     nowhere else.
//  2. it is absent from the stream this apply just sent (the chart's own
//     templates plus the manifests that ride it).
//  3. it is not a kind that holds data or owns everything under it
//     ([release.StatefulKind]: PersistentVolume, PersistentVolumeClaim,
//     StatefulSet, Secret, Namespace, CustomResourceDefinition). Those are
//     kept (and reported, except a Secret, which is never even listed so its
//     contents are never read), exactly as the CRD prune keeps a CRD that
//     still has instances. Removing one is a human decision.
//  4. it carries no `kustomize.toolkit.fluxcd.io/prune: disabled`
//     annotation, the same escape hatch a Flux bundle honours, so one
//     spelling protects an object on both delivery paths.
//  5. no controller owns it (`ownerReferences`): the owner garbage-collects
//     it, and deleting it here would only start a fight.
//
// It runs only after the chart's apply fully succeeded (a partial apply
// proves nothing about what the render contains), only when the chart did not
// declare `prune = false`, and never on a render it cannot read in full.
//
// The same semantics Flux gives the bundle path, with the same escape hatch:
// the migration design's §4.3 relies on a reconciler pruning by inventory, and
// that later path must not surprise an env that already lives with this one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

const (
	// ChartOwnerLabel names the (project, env) that applied a chart's object.
	// It is the chart prune's ownership proof; see the file comment.
	ChartOwnerLabel = "forge.dev/chart-owner"

	// PruneAnnotation is Flux's prune-exemption annotation. Spelled here (and
	// in internal/bundle, which cannot import this package) because the string
	// is Flux's, not forge's: published and stable across Flux's v1 API. An
	// object carrying it with the value PruneDisabled is never pruned,
	// whichever delivery path would have pruned it.
	PruneAnnotation = "kustomize.toolkit.fluxcd.io/prune"

	// PruneDisabled is the PruneAnnotation value that exempts an object.
	PruneDisabled = "disabled"
)

// ChartOwner is the ChartOwnerLabel value for a (project, env): "<project>.<env>",
// sanitized to a legal label value. Empty when either half is unknown, which
// disables both the stamp and the prune — an owner that cannot be named cannot
// be proven. It is CRDOwner's value on purpose: one spelling of "this env".
func ChartOwner(project, env string) string { return CRDOwner(project, env) }

// chartPruneSkippedResources are API resources the prune never lists. They are
// derived or churned by controllers, so a forge label on one means a
// controller copied it, and none can be something a chart renders.
//
// Secrets are skipped for a different reason: they are never deleted (a Secret
// is a stateful kind), so listing them would read credentials into this
// process for no decision they could change.
var chartPruneSkippedResources = map[string]bool{
	"secrets":                         true,
	"events":                          true,
	"events.events.k8s.io":            true,
	"pods":                            true,
	"replicasets.apps":                true,
	"endpoints":                       true,
	"endpointslices.discovery.k8s.io": true,
	"controllerrevisions.apps":        true,
	"leases.coordination.k8s.io":      true,
	"nodes":                           true,
	"componentstatuses":               true,
}

// chartObjectRef is one object by the three things that identify it in both a
// rendered stream and a live listing.
//
// Kind and name, with the namespace, and NOT the API group: a chart written
// against `extensions/v1beta1` renders an Ingress the apiserver serves as
// `networking.k8s.io`, and treating the group as identity would read a live
// object the render still declares as one it dropped. The cost of ignoring
// the group is a collision between two same-kind same-name objects of
// different groups inside one chart's label scope, which does not occur and
// which errs toward keeping an object.
type chartObjectRef struct {
	Kind, Namespace, Name string
}

func (r chartObjectRef) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// chartDesiredSet is what a chart's apply sent, indexed for the live
// comparison.
type chartDesiredSet struct {
	// namespaced holds kind/effective-namespace/name. The effective namespace
	// of a document that names none is the chart's own: the apply passed it as
	// `-n`, which is where kubectl put every namespace-less object (flux2's
	// 40 objects carry no metadata.namespace at all).
	namespaced map[chartObjectRef]bool
	// anyNamespace holds kind/name alone, for matching a live CLUSTER-scoped
	// object. Scope is not in the manifest, so a document is indexed both
	// ways and the live object's own namespace picks the lookup.
	anyNamespace map[chartObjectRef]bool
}

// desiredChartObjects indexes a chart's stream. ok is false when the stream
// cannot be read in full or holds no object: a prune computed from a partial
// view would delete everything the unread part declared.
func desiredChartObjects(stream, chartNamespace string) (set chartDesiredSet, ok bool) {
	set = chartDesiredSet{namespaced: map[chartObjectRef]bool{}, anyNamespace: map[chartObjectRef]bool{}}
	n := 0
	for _, doc := range splitDocs(stream) {
		m, parsed := parseDoc(doc)
		if !parsed {
			return set, false
		}
		if m.Kind == "" || m.Metadata.Name == "" {
			continue
		}
		ns := m.Metadata.Namespace
		if ns == "" {
			ns = chartNamespace
		}
		set.namespaced[chartObjectRef{m.Kind, ns, m.Metadata.Name}] = true
		set.anyNamespace[chartObjectRef{m.Kind, "", m.Metadata.Name}] = true
		n++
	}
	return set, n > 0
}

func (s chartDesiredSet) contains(live chartObjectRef) bool {
	if live.Namespace == "" {
		return s.anyNamespace[live]
	}
	return s.namespaced[live]
}

// liveChartObject is the part of a listed object the prune decides on.
type liveChartObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Annotations       map[string]string `json:"annotations"`
		OwnerReferences   []json.RawMessage `json:"ownerReferences"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
	} `json:"metadata"`
	// raw is the whole listed document, for the deploy plan's comparison.
	raw map[string]any
}

// UnmarshalJSON decodes the fields the prune decides on and keeps the whole
// document beside them.
func (o *liveChartObject) UnmarshalJSON(b []byte) error {
	type plain liveChartObject
	if err := json.Unmarshal(b, (*plain)(o)); err != nil {
		return err
	}
	return json.Unmarshal(b, &o.raw)
}

func (o liveChartObject) ref() chartObjectRef {
	return chartObjectRef{o.Kind, o.Metadata.Namespace, o.Metadata.Name}
}

// group is the object's API group, "" for the core group.
func (o liveChartObject) group() string {
	if g, _, found := strings.Cut(o.APIVersion, "/"); found {
		return g
	}
	return ""
}

// resource is the argument `kubectl delete` takes: kind.group, or the bare
// kind in the core group. A kind alone is ambiguous across groups.
func (o liveChartObject) resource() string {
	if g := o.group(); g != "" {
		return strings.ToLower(o.Kind) + "." + g
	}
	return strings.ToLower(o.Kind)
}

// chartPruneKeep says why a departed object was left alone.
type chartPruneKeep struct {
	Object chartObjectRef
	Why    string
}

// planChartPrune splits the live objects carrying a chart's stamp into those
// to delete and those departed objects that a guard keeps. An object the
// render still declares is neither: it is simply current.
func planChartPrune(desired chartDesiredSet, live []liveChartObject) (del []liveChartObject, kept []chartPruneKeep) {
	for _, o := range live {
		ref := o.ref()
		if desired.contains(ref) {
			continue
		}
		switch {
		case release.StatefulKind(o.Kind):
			kept = append(kept, chartPruneKeep{ref, "a " + o.Kind + " holds data or owns everything under it; delete it by hand if it should go"})
		case o.Metadata.Annotations[PruneAnnotation] == PruneDisabled:
			kept = append(kept, chartPruneKeep{ref, "annotated " + PruneAnnotation + ": " + PruneDisabled})
		case len(o.Metadata.OwnerReferences) > 0:
			kept = append(kept, chartPruneKeep{ref, "owned by another object, which garbage-collects it"})
		case o.Metadata.DeletionTimestamp != "":
			// Already going. Deleting again only waits on its finalizers.
		default:
			del = append(del, o)
		}
	}
	sort.Slice(del, func(i, j int) bool { return del[i].ref().String() < del[j].ref().String() })
	sort.Slice(kept, func(i, j int) bool { return kept[i].Object.String() < kept[j].Object.String() })
	return del, kept
}

// PruneChartObjects deletes the objects rc's chart applied before and no
// longer renders, per the file comment. It returns the objects it deleted. A
// failure to LIST is an error and deletes nothing; a failure to delete one
// object is reported on stdout and does not stop the rest.
//
// It does nothing for a chart without an Owner, one that declared
// `prune = false`, or one whose stream it cannot read in full.
func PruneChartObjects(ctx context.Context, kctx string, rc renderedChart) (pruned []string, err error) {
	spec := rc.spec
	if !spec.Prune || spec.Owner == "" {
		return nil, nil
	}
	if strings.TrimSpace(kctx) == "" {
		return nil, errors.New("chart prune: no kubectl context — a delete is never aimed at the current context")
	}
	del, kept, readable, err := chartPruneCandidates(ctx, kctx, rc)
	if err != nil {
		return nil, err
	}
	if !readable {
		fmt.Printf("  chart %q: skipping prune (its render is empty or unreadable, so nothing can be proven absent)\n", spec.Name)
		return nil, nil
	}
	for _, k := range kept {
		fmt.Printf("  chart %q no longer renders %s; keeping it (%s)\n", spec.Name, k.Object, k.Why)
	}
	if len(del) == 0 {
		return nil, nil
	}
	fmt.Printf("Pruning %d object(s) chart %q no longer renders (owner %s):\n", len(del), spec.Name, spec.Owner)
	for _, o := range del {
		args := []string{"delete", o.resource(), o.Metadata.Name, "--ignore-not-found=true"}
		if o.Metadata.Namespace != "" {
			args = append(args, "-n", o.Metadata.Namespace)
		}
		fmt.Printf("  - %s\n", o.ref())
		cmd := kubectlCmd(ctx, kctx, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if derr := cmd.Run(); derr != nil {
			fmt.Printf("  Warning: delete %s: %v\n", o.ref(), derr)
			continue
		}
		pruned = append(pruned, o.ref().String())
	}
	return pruned, nil
}

// chartPruneCandidates is the prune's decision, without its effect: the live
// objects the chart no longer renders that a prune would delete, and those a
// guard keeps. readable is false when the render cannot prove anything absent.
// Shared by PruneChartObjects and the deploy plan, so what the plan says it
// will delete is what the apply deletes.
func chartPruneCandidates(ctx context.Context, kctx string, rc renderedChart) (del []liveChartObject, kept []chartPruneKeep, readable bool, err error) {
	spec := rc.spec
	desired, ok := desiredChartObjects(joinNonEmpty(rc.manifests, rc.extra), spec.Namespace)
	if !ok {
		return nil, nil, false, nil
	}
	resources, err := listChartPruneResources(ctx, kctx)
	if err != nil {
		return nil, nil, true, err
	}
	live, err := listLiveChartObjects(ctx, kctx, resources, spec)
	if err != nil {
		return nil, nil, true, err
	}
	del, kept = planChartPrune(desired, live)
	return del, kept, true, nil
}

// listChartPruneResources asks the cluster which resource types can be listed
// and deleted. Discovered rather than listed here because the prune's whole
// job is to find a kind the NEW render no longer mentions: when the last
// NetworkPolicy leaves a chart, no document of that kind remains to name it.
func listChartPruneResources(ctx context.Context, kctx string) ([]string, error) {
	out, err := kubectlCmd(ctx, kctx, "api-resources", "--verbs=list,delete", "-o", "name").Output()
	if err != nil {
		return nil, fmt.Errorf("chart prune: discover API resources: %w", err)
	}
	var resources []string
	for _, line := range strings.Split(string(out), "\n") {
		r := strings.TrimSpace(line)
		if r != "" && !chartPruneSkippedResources[r] {
			resources = append(resources, r)
		}
	}
	if len(resources) == 0 {
		return nil, errors.New("chart prune: the cluster reported no listable API resources")
	}
	return resources, nil
}

// listLiveChartObjects lists every object across the given resource types
// that carries this chart's stamp, in every namespace.
//
// kubectl exits non-zero when ANY listed type fails (an aggregated API that is
// down) while still printing the rest. Listing fewer objects can only keep
// more of them, so partial output is used; output that does not parse is an
// error.
func listLiveChartObjects(ctx context.Context, kctx string, resources []string, spec HelmChartSpec) ([]liveChartObject, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s", ChartOwnerLabel, spec.Owner, WorkloadLabel, spec.Name)
	cmd := kubectlCmd(ctx, kctx, "get", strings.Join(resources, ","), "-A", "-l", selector, "-o", "json", "--ignore-not-found")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var list struct {
		Items []liveChartObject `json:"items"`
	}
	if perr := json.Unmarshal(stdout.Bytes(), &list); perr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("chart prune: list objects owned by %s: %w: %s", spec.Owner, runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("chart prune: parse listing: %w", perr)
	}
	if runErr != nil {
		fmt.Printf("  Warning: chart %q prune: some resource types could not be listed (%v); pruning what was found\n", spec.Name, runErr)
	}
	return list.Items, nil
}
