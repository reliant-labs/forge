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
// THE INVENTORY. Every apply of a chart records what it sent in that chart's
// INVENTORY: a ConfigMap in the chart's namespace, one per chart and
// (project, env) (chartInventoryName). The prune's candidates are the entries
// the inventory held BEFORE this apply that the new render no longer
// contains. Nothing else is ever a candidate, so the prune deletes only what
// an earlier forge apply of this chart recorded sending:
//
//   - an object a human or a controller made is never a candidate, even one
//     carrying forge's labels (a manifest copied with `kubectl get -o yaml`);
//   - an object a CONCURRENT apply of the same chart is introducing is not in
//     the inventory this apply read before it started, so this apply cannot
//     delete it (CONCURRENT APPLIES below);
//   - an apply that finds no inventory (the first deploy under this version,
//     or after the chart's namespace was deleted) prunes NOTHING and only
//     records one.
//
// A candidate is then deleted only when ALL of these hold on the live object,
// read just before the delete:
//
//  1. it still carries this chart's stamp, `forge.dev/chart-owner=<project>.<env>`
//     plus `forge.dev/workload=<chart>` (stampDocAppLabel writes both on
//     the render path and nowhere else). An object whose stamp was removed or
//     rewritten (by a human, or by another env applying the same object) is
//     someone else's now: forge leaves it and stops tracking it.
//  2. it is not a kind that holds data or owns everything under it
//     ([release.StatefulKind]: PersistentVolume, PersistentVolumeClaim,
//     StatefulSet, Secret, Namespace, CustomResourceDefinition). Those are
//     kept and reported, exactly as the CRD prune keeps a CRD that still has
//     instances. Only their EXISTENCE is read (`-o name`), so a Secret's
//     contents never enter this process. Removing one is a human decision.
//  3. it carries no `kustomize.toolkit.fluxcd.io/prune: disabled`
//     annotation, the same escape hatch a Flux bundle honours, so one
//     spelling protects an object on both delivery paths.
//  4. no controller owns it (`ownerReferences`): the owner garbage-collects
//     it, and deleting it here would only start a fight.
//
// The prune runs only after the chart's apply fully succeeded (a partial apply
// proves nothing about what the render contains), only when the chart did not
// declare `prune = false`, and never on a render it cannot read in full. A
// FAILED apply deletes nothing, but it still adds what it sent to the
// inventory. An object it created before failing is therefore tracked, and is
// pruned once a later render drops it.
//
// CONCURRENT APPLIES. Two forge processes may apply the same chart at once
// (two checkouts deploying one env). Each reads the inventory before it
// applies and prunes only from that snapshot, so neither deletes an object the
// other is introducing. The inventory is written by compare-and-swap
// (`kubectl replace` against the resourceVersion read, `kubectl create` when
// there is none) and retried on a conflict. A retry merges, never
// overwrites: each applier adds what it sent and removes only the entries it
// saw gone that nobody re-recorded after it read them. Every entry carries the
// id of the apply that last recorded it, which is how a re-record is detected.
// Concurrency cannot decide which of two different renders wins; as with any
// two applies, the next deploy converges.
//
// The same semantics Flux gives the bundle path, with the same escape hatch:
// the migration design's §4.3 relies on a reconciler pruning by inventory, and
// that later path must not surprise an env that already lives with this one.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

const (
	// ChartOwnerLabel names the (project, env) that applied a chart's object.
	// The prune deletes an inventoried object only while it still carries it;
	// see the file comment.
	ChartOwnerLabel = "forge.dev/chart-owner"

	// PruneAnnotation is Flux's prune-exemption annotation. Spelled here (and
	// in internal/bundle, which cannot import this package) because the string
	// is Flux's, not forge's: published and stable across Flux's v1 API. An
	// object carrying it with the value PruneDisabled is never pruned,
	// whichever delivery path would have pruned it.
	PruneAnnotation = "kustomize.toolkit.fluxcd.io/prune"

	// PruneDisabled is the PruneAnnotation value that exempts an object.
	PruneDisabled = "disabled"

	// ChartInventoryLabel and ChartInventoryOwnerLabel select a chart's
	// inventory ConfigMap: the chart's name, and the (project, env) that
	// records it. They are deliberately not the stamp a chart's own objects
	// carry, so the inventory can never be read as an object of the chart.
	ChartInventoryLabel      = "forge.dev/chart-inventory"
	ChartInventoryOwnerLabel = "forge.dev/chart-inventory-owner"

	// chartInventoryDataKey is the inventory ConfigMap's data key.
	chartInventoryDataKey = "inventory"

	// chartInventoryWriteAttempts bounds the compare-and-swap retry. Each
	// conflict means another apply of the same chart wrote in between, so a
	// handful is generous.
	chartInventoryWriteAttempts = 5
)

// ChartOwner is the ChartOwnerLabel value for a (project, env): "<project>.<env>",
// sanitized to a legal label value. Empty when either half is unknown, which
// disables both the stamp and the prune — an owner that cannot be named cannot
// be proven. It is CRDOwner's value on purpose: one spelling of "this env".
func ChartOwner(project, env string) string { return CRDOwner(project, env) }

// chartObjectRef is one object by the three things that identify it in both a
// rendered stream and a live object.
//
// Kind and name, with the namespace, and NOT the API group: a chart written
// against `extensions/v1beta1` renders an Ingress the apiserver serves as
// `networking.k8s.io`, and treating the group as identity would read a live
// object the render still declares as one it dropped. The cost of ignoring
// the group is a collision between two same-kind same-name objects of
// different groups inside one chart, which does not occur and which errs
// toward keeping an object.
type chartObjectRef struct {
	Kind, Namespace, Name string
}

func (r chartObjectRef) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// apiGroup is the group of an apiVersion, "" for the core group.
func apiGroup(apiVersion string) string {
	if g, _, found := strings.Cut(apiVersion, "/"); found {
		return g
	}
	return ""
}

// kubectlResource is the resource argument kubectl takes for a kind: kind.group,
// or the bare kind in the core group. A kind alone is ambiguous across groups.
func kubectlResource(kind, group string) string {
	if group != "" {
		return strings.ToLower(kind) + "." + group
	}
	return strings.ToLower(kind)
}

// inventoryEntry is one object a chart's apply sent, as its inventory records
// it: the API group (so a later delete names the right resource), kind,
// EFFECTIVE namespace and name. A document that names no namespace is recorded
// under the chart's, which is where the apply's `-n` put it — and, for a
// cluster-scoped kind, not where it lives. Scope is not in the manifest, so the
// prune settles that against the live object (decideChartPrune).
type inventoryEntry struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

func (e inventoryEntry) ref() chartObjectRef { return chartObjectRef{e.Kind, e.Namespace, e.Name} }
func (e inventoryEntry) resource() string    { return kubectlResource(e.Kind, e.Group) }
func (e inventoryEntry) String() string      { return e.ref().String() }

// chartDesiredSet is what a chart's apply sent, indexed for the comparison
// with the inventory and the live objects.
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
	// entries is the stream as inventory entries, in stream order: what the
	// inventory records for this apply.
	entries []inventoryEntry
}

// desiredChartObjects indexes a chart's stream. ok is false when the stream
// cannot be read in full or holds no object: a prune computed from a partial
// view would delete everything the unread part declared, and an inventory
// recorded from one would forget it.
func desiredChartObjects(stream, chartNamespace string) (set chartDesiredSet, ok bool) {
	set = chartDesiredSet{namespaced: map[chartObjectRef]bool{}, anyNamespace: map[chartObjectRef]bool{}}
	seen := map[inventoryEntry]bool{}
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
		e := inventoryEntry{Group: apiGroup(m.APIVersion), Kind: m.Kind, Namespace: ns, Name: m.Metadata.Name}
		if !seen[e] {
			seen[e] = true
			set.entries = append(set.entries, e)
		}
	}
	return set, len(set.entries) > 0
}

func (s chartDesiredSet) contains(live chartObjectRef) bool {
	if live.Namespace == "" {
		return s.anyNamespace[live]
	}
	return s.namespaced[live]
}

// chartInventory is a chart's recorded inventory: each entry, with the id of
// the apply that last recorded it.
type chartInventory map[inventoryEntry]string

// inventoryRecord is one entry as the ConfigMap stores it.
type inventoryRecord struct {
	inventoryEntry
	// Apply is the id of the apply that last recorded the entry
	// (newChartApplyID). The compare-and-swap merge uses it to tell an entry
	// another apply re-recorded from one nobody touched.
	Apply string `json:"apply"`
}

// encode renders the inventory as the ConfigMap's data: a JSON list sorted by
// object, so a diff of two inventories reads as a diff of objects.
func (inv chartInventory) encode() (string, error) {
	records := make([]inventoryRecord, 0, len(inv))
	for e, id := range inv {
		records = append(records, inventoryRecord{e, id})
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i].inventoryEntry, records[j].inventoryEntry
		if a.String() != b.String() {
			return a.String() < b.String()
		}
		return a.Group < b.Group
	})
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeChartInventory(data string) (chartInventory, error) {
	var records []inventoryRecord
	if err := json.Unmarshal([]byte(data), &records); err != nil {
		return nil, err
	}
	inv := chartInventory{}
	for _, r := range records {
		if r.Kind == "" || r.Name == "" {
			return nil, fmt.Errorf("an entry names no kind or name: %+v", r.inventoryEntry)
		}
		inv[r.inventoryEntry] = r.Apply
	}
	return inv, nil
}

var dnsLabelUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// chartInventoryName names a chart's inventory ConfigMap. The (project, env)
// is hashed in rather than spelled out, for two reasons. An owner is a label
// value, which may hold characters a ConfigMap name cannot. And two envs that
// install the same chart into one namespace (two envs sharing a dev cluster)
// each need an inventory of their own.
func chartInventoryName(chart, owner string) string {
	sum := sha256.Sum256([]byte(owner + "/" + chart))
	base := strings.Trim(dnsLabelUnsafe.ReplaceAllString(strings.ToLower(chart), "-"), "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		base = "chart"
	}
	return "forge-chart-inventory-" + base + "-" + hex.EncodeToString(sum[:])[:10]
}

// newChartApplyID names one apply run: when it ran, plus randomness, so two
// concurrent applies never share an id.
func newChartApplyID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:])
}

// storedInventory is one inventory ConfigMap as read from the cluster.
type storedInventory struct {
	Namespace, Name, ResourceVersion string
	Entries                          chartInventory
}

// configMapJSON is the part of a ConfigMap the inventory reads.
type configMapJSON struct {
	Metadata struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

func (cm configMapJSON) inventory() (storedInventory, error) {
	entries, err := decodeChartInventory(cm.Data[chartInventoryDataKey])
	if err != nil {
		return storedInventory{}, fmt.Errorf("inventory ConfigMap %s/%s is unreadable (%v); "+
			"delete it to start a fresh inventory (the next apply then prunes nothing and records one)",
			cm.Metadata.Namespace, cm.Metadata.Name, err)
	}
	return storedInventory{cm.Metadata.Namespace, cm.Metadata.Name, cm.Metadata.ResourceVersion, entries}, nil
}

// readChartInventories returns every inventory ConfigMap recorded for this
// (owner, chart), in ANY namespace. There is usually one. There are two when the
// chart moved to another namespace: the old one still holds what the chart
// applied there, and the prune must see it.
func readChartInventories(ctx context.Context, kctx string, spec HelmChartSpec) ([]storedInventory, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s", ChartInventoryLabel, spec.Name, ChartInventoryOwnerLabel, spec.Owner)
	cmd := kubectlCmd(ctx, kctx, "get", "configmaps", "-A", "-l", selector, "-o", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read the chart's inventory: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var list struct {
		Items []configMapJSON `json:"items"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		return nil, fmt.Errorf("read the chart's inventory: parse listing: %w", err)
	}
	out := make([]storedInventory, 0, len(list.Items))
	for _, cm := range list.Items {
		inv, err := cm.inventory()
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

// getChartInventory reads one inventory ConfigMap, or nil when there is none.
func getChartInventory(ctx context.Context, kctx, namespace, name string) (*storedInventory, error) {
	cmd := kubectlCmd(ctx, kctx, "get", "configmap", name, "-n", namespace, "-o", "json", "--ignore-not-found")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read inventory %s/%s: %w: %s", namespace, name, err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return nil, nil
	}
	var cm configMapJSON
	if err := json.Unmarshal(stdout.Bytes(), &cm); err != nil {
		return nil, fmt.Errorf("read inventory %s/%s: %w", namespace, name, err)
	}
	inv, err := cm.inventory()
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// errInventoryConflict is a compare-and-swap that lost: another apply wrote
// the inventory after this one read it.
var errInventoryConflict = errors.New("the inventory changed since it was read")

// writeChartInventory writes inv as the chart's inventory, compare-and-swap:
// `kubectl create` when cur is nil (AlreadyExists is a conflict),
// `kubectl replace` at cur's resourceVersion otherwise (Conflict, or NotFound
// because it was deleted meanwhile, is a conflict).
func writeChartInventory(ctx context.Context, kctx string, spec HelmChartSpec, name string, inv chartInventory, cur *storedInventory) error {
	data, err := inv.encode()
	if err != nil {
		return err
	}
	metadata := map[string]any{
		"name":      name,
		"namespace": spec.Namespace,
		"labels": map[string]any{
			AppManagedByLabel:        "forge",
			ChartInventoryLabel:      spec.Name,
			ChartInventoryOwnerLabel: spec.Owner,
		},
	}
	verb := "create"
	if cur != nil {
		verb = "replace"
		metadata["resourceVersion"] = cur.ResourceVersion
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata,
		"data": map[string]any{chartInventoryDataKey: data},
	})
	if err != nil {
		return err
	}
	cmd := kubectlCmd(ctx, kctx, verb, "-f", "-")
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		lost := strings.Contains(msg, "(AlreadyExists)")
		if verb == "replace" {
			lost = strings.Contains(msg, "(Conflict)") || strings.Contains(msg, "(NotFound)")
		}
		if lost {
			return errInventoryConflict
		}
		return fmt.Errorf("write inventory %s/%s: %w: %s", spec.Namespace, name, err, strings.TrimSpace(msg))
	}
	return nil
}

// liveChartObject is the part of a live object the prune decides on.
type liveChartObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Labels            map[string]string `json:"labels"`
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

// resource is the argument `kubectl delete` takes for the live object.
func (o liveChartObject) resource() string { return kubectlResource(o.Kind, apiGroup(o.APIVersion)) }

// chartPruneVerdict is what the prune does with one departed inventory entry.
type chartPruneVerdict int

const (
	// pruneDelete deletes the object and drops its entry.
	pruneDelete chartPruneVerdict = iota
	// pruneKeep leaves the object and keeps tracking it, with a reason to
	// report: a later render, or a removed guard, may still settle it.
	pruneKeep
	// pruneForget drops the entry and leaves the object, if any: it is gone,
	// is still rendered under its live identity, or is no longer this chart's.
	pruneForget
)

// decideChartPrune decides one departed inventory entry against its live
// object. spec is the chart; desired is the new render.
func decideChartPrune(live liveChartObject, desired chartDesiredSet, spec HelmChartSpec) (chartPruneVerdict, string) {
	labels := live.Metadata.Labels
	switch {
	case desired.contains(live.ref()):
		// Still rendered: a cluster-scoped object recorded under the
		// namespace the chart had then. The render's entry tracks it now.
		return pruneForget, ""
	case labels[ChartOwnerLabel] != spec.Owner || labels[WorkloadLabel] != spec.Name:
		return pruneForget, "it no longer carries this chart's " + ChartOwnerLabel + "=" + spec.Owner +
			" / " + WorkloadLabel + "=" + spec.Name + " stamp, so something else owns it now; no longer tracking it"
	case release.StatefulKind(live.Kind):
		return pruneKeep, "a " + live.Kind + " holds data or owns everything under it; delete it by hand if it should go"
	case live.Metadata.Annotations[PruneAnnotation] == PruneDisabled:
		return pruneKeep, "annotated " + PruneAnnotation + ": " + PruneDisabled
	case len(live.Metadata.OwnerReferences) > 0:
		return pruneKeep, "owned by another object, which garbage-collects it"
	case live.Metadata.DeletionTimestamp != "":
		// Already going. Deleting again only waits on its finalizers.
		return pruneForget, ""
	default:
		return pruneDelete, ""
	}
}

// chartPrune is one chart's prune across its apply: the inventory read BEFORE
// the apply, and what is done with it after. A nil *chartPrune is a chart
// whose prune is off (it opted out, has no owner, or its inventory could not
// be read); every method is then a no-op.
type chartPrune struct {
	kctx    string
	rc      renderedChart
	applyID string
	// stored is every inventory ConfigMap of this (owner, chart), read
	// before the apply.
	stored []storedInventory
	// prior is their entries merged: what earlier applies recorded sending.
	// The prune's candidates come from here and nowhere else.
	prior chartInventory
}

// beginChartPrune reads a chart's inventory before its apply. It returns nil,
// and prints why, when this apply must neither prune nor record: an inventory
// that cannot be read cannot be merged into, and overwriting it would forget
// what it held.
func beginChartPrune(ctx context.Context, kctx string, rc renderedChart, applyID string) *chartPrune {
	spec := rc.spec
	if !spec.Prune || spec.Owner == "" {
		return nil
	}
	if strings.TrimSpace(kctx) == "" {
		fmt.Printf("  Warning: chart %q: skipping prune (no kubectl context; a delete is never aimed at the current context)\n", spec.Name)
		return nil
	}
	if strings.TrimSpace(spec.Namespace) == "" {
		// The inventory lives in the chart's namespace; with none it would land
		// in whatever namespace the context defaults to. The KCL schema
		// requires a namespace, so only a hand-built spec gets here.
		fmt.Printf("  Warning: chart %q: skipping prune (the chart declares no namespace to keep its inventory in)\n", spec.Name)
		return nil
	}
	stored, err := readChartInventories(ctx, kctx, spec)
	if err != nil {
		fmt.Printf("  Warning: chart %q: skipping prune: %v\n", spec.Name, err)
		return nil
	}
	prior := chartInventory{}
	for _, s := range stored {
		for e, id := range s.Entries {
			prior[e] = id
		}
	}
	return &chartPrune{kctx: kctx, rc: rc, applyID: applyID, stored: stored, prior: prior}
}

// desired indexes what the chart's apply sends: its templates plus the
// manifests that ride it.
func (p *chartPrune) desired() (chartDesiredSet, bool) {
	return desiredChartObjects(joinNonEmpty(p.rc.manifests, p.rc.extra), p.rc.spec.Namespace)
}

// afterFailedApply records what a failed apply sent and deletes nothing. A
// partial apply may have created some of it, and an object missing from the
// inventory could never be pruned.
func (p *chartPrune) afterFailedApply(ctx context.Context) {
	if p == nil {
		return
	}
	desired, ok := p.desired()
	if !ok {
		return
	}
	if err := p.record(ctx, desired.entries, nil); err != nil {
		fmt.Printf("  Warning: chart %q: %v\n", p.rc.spec.Name, err)
	}
}

// afterApply runs once the chart's apply has fully succeeded: it prunes what
// the prior inventory held and the render dropped, then records the render. It
// returns the objects it deleted. A failure to delete one object is reported
// on stdout and does not stop the rest; it stays in the inventory, so the
// next apply tries again.
func (p *chartPrune) afterApply(ctx context.Context) (pruned []string, err error) {
	if p == nil {
		return nil, nil
	}
	spec := p.rc.spec
	desired, ok := p.desired()
	if !ok {
		fmt.Printf("  chart %q: skipping prune (its render is empty or unreadable, so nothing can be proven absent)\n", spec.Name)
		return nil, nil
	}
	if len(p.stored) == 0 {
		fmt.Printf("  chart %q: no inventory yet; recording one (nothing is pruned on this apply)\n", spec.Name)
	}
	pruned, gone := p.prune(ctx, desired)
	if err := p.record(ctx, desired.entries, gone); err != nil {
		return pruned, err
	}
	return pruned, nil
}

// prune deletes the prior inventory's departed objects per the file comment.
// It returns the objects it deleted, and the entries to drop from the
// inventory (deleted, already absent, or forgotten).
func (p *chartPrune) prune(ctx context.Context, desired chartDesiredSet) (pruned []string, gone []inventoryEntry) {
	spec := p.rc.spec
	var departed []inventoryEntry
	for e := range p.prior {
		if !desired.namespaced[e.ref()] {
			departed = append(departed, e)
		}
	}
	sort.Slice(departed, func(i, j int) bool { return departed[i].String() < departed[j].String() })

	announced := false
	for _, e := range departed {
		// A stateful kind is kept whatever its labels say, so only its
		// existence is read: a Secret's data never reaches this process.
		if release.StatefulKind(e.Kind) {
			exists, err := chartObjectExists(ctx, p.kctx, e)
			switch {
			case err != nil:
				fmt.Printf("  Warning: chart %q prune: %v\n", spec.Name, err)
			case !exists:
				gone = append(gone, e)
			default:
				fmt.Printf("  chart %q no longer renders %s; keeping it (a %s holds data or owns everything under it; delete it by hand if it should go)\n",
					spec.Name, e, e.Kind)
			}
			continue
		}
		live, err := getChartObject(ctx, p.kctx, e)
		if err != nil {
			fmt.Printf("  Warning: chart %q prune: %v\n", spec.Name, err)
			continue
		}
		if live == nil {
			gone = append(gone, e)
			continue
		}
		verdict, why := decideChartPrune(*live, desired, spec)
		switch verdict {
		case pruneForget:
			if why != "" {
				fmt.Printf("  chart %q no longer renders %s; leaving it (%s)\n", spec.Name, live.ref(), why)
			}
			gone = append(gone, e)
		case pruneKeep:
			fmt.Printf("  chart %q no longer renders %s; keeping it (%s)\n", spec.Name, live.ref(), why)
		case pruneDelete:
			if !announced {
				fmt.Printf("Pruning object(s) chart %q no longer renders (owner %s):\n", spec.Name, spec.Owner)
				announced = true
			}
			fmt.Printf("  - %s\n", live.ref())
			args := []string{"delete", live.resource(), live.Metadata.Name, "--ignore-not-found=true"}
			if live.Metadata.Namespace != "" {
				args = append(args, "-n", live.Metadata.Namespace)
			}
			cmd := kubectlCmd(ctx, p.kctx, args...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if derr := cmd.Run(); derr != nil {
				fmt.Printf("  Warning: delete %s: %v\n", live.ref(), derr)
				continue
			}
			pruned = append(pruned, live.ref().String())
			gone = append(gone, e)
		}
	}
	return pruned, gone
}

// getChartObject reads one inventoried object, or nil when it is absent.
func getChartObject(ctx context.Context, kctx string, e inventoryEntry) (*liveChartObject, error) {
	args := []string{"get", e.resource(), e.Name, "-o", "json", "--ignore-not-found"}
	if e.Namespace != "" {
		// Ignored by kubectl for a cluster-scoped kind, which is why an
		// entry recorded under the chart's namespace still finds one.
		args = append(args, "-n", e.Namespace)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if kindNotServed(stderr.String()) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w: %s", e, err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return nil, nil
	}
	var live liveChartObject
	if err := json.Unmarshal(stdout.Bytes(), &live); err != nil {
		return nil, fmt.Errorf("read %s: %w", e, err)
	}
	return &live, nil
}

// chartObjectExists reports whether an inventoried object exists, reading only
// its name.
func chartObjectExists(ctx context.Context, kctx string, e inventoryEntry) (bool, error) {
	args := []string{"get", e.resource(), e.Name, "-o", "name", "--ignore-not-found"}
	if e.Namespace != "" {
		args = append(args, "-n", e.Namespace)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if kindNotServed(stderr.String()) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w: %s", e, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()) != "", nil
}

// kindNotServed reports a kubectl read that failed because the cluster no
// longer serves the kind at all (its CRD was removed). No object of that kind
// can exist, so the entry is absent. Answering "absent" only forgets the
// entry; it never deletes anything.
func kindNotServed(stderr string) bool {
	return strings.Contains(stderr, "the server doesn't have a resource type")
}

// record writes the chart's inventory in its namespace: what the inventory
// holds now, plus every entry this apply sent (with this apply's id), minus
// the entries this apply saw gone, unless another apply re-recorded one after
// this apply read it. It is a compare-and-swap, retried on a conflict, so a
// concurrent apply's entries are merged rather than overwritten.
//
// An inventory left in another namespace (the chart moved) is folded in, and
// deleted once its entries are safely in this one.
func (p *chartPrune) record(ctx context.Context, sent, gone []inventoryEntry) error {
	spec := p.rc.spec
	name := chartInventoryName(spec.Name, spec.Owner)
	written := false
	for attempt := 0; attempt < chartInventoryWriteAttempts && !written; attempt++ {
		cur, err := getChartInventory(ctx, p.kctx, spec.Namespace, name)
		if err != nil {
			return err
		}
		merged := chartInventory{}
		if cur != nil {
			for e, id := range cur.Entries {
				merged[e] = id
			}
		}
		for _, s := range p.stored {
			if s.Namespace == spec.Namespace && s.Name == name {
				continue
			}
			for e, id := range s.Entries {
				if _, ok := merged[e]; !ok {
					merged[e] = id
				}
			}
		}
		for _, e := range sent {
			merged[e] = p.applyID
		}
		for _, e := range gone {
			if id, ok := merged[e]; ok && id == p.prior[e] {
				delete(merged, e)
			}
		}
		switch err := writeChartInventory(ctx, p.kctx, spec, name, merged, cur); {
		case err == nil:
			written = true
		case !errors.Is(err, errInventoryConflict):
			return err
		}
	}
	if !written {
		return fmt.Errorf("write inventory %s/%s: still conflicting after %d attempts; another apply of this chart is writing it",
			spec.Namespace, name, chartInventoryWriteAttempts)
	}
	for _, s := range p.stored {
		if s.Namespace == spec.Namespace && s.Name == name {
			continue
		}
		cmd := kubectlCmd(ctx, p.kctx, "delete", "configmap", s.Name, "-n", s.Namespace, "--ignore-not-found=true")
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.Discard, &stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("  Warning: chart %q: delete its former inventory %s/%s: %v: %s\n",
				spec.Name, s.Namespace, s.Name, err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
