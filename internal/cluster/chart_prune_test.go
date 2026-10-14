package cluster

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The chart prune, proven against a stateful fake cluster (fake_cluster_test.go).
//
// Each test applies a chart as release N, then as release N+1 with the render
// changed, and reads what is LIVE afterwards. That is the claim the prune
// makes, and an argv recorder cannot check it.

const chartOwnerFixture = "acme.prod"

// fakeHelmReleases installs a `helm` that prints whatever stream was last passed
// to the returned setter, so one test can render release N and then release N+1
// through the real RenderHelmChart.
func fakeHelmReleases(t *testing.T) (setRelease func(stream string)) {
	t.Helper()
	requirePOSIXFake(t, "helm")
	dir := t.TempDir()
	body := filepath.Join(dir, "chart.yaml")
	script := "#!/bin/sh\ncat " + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(stream string) {
		t.Helper()
		if err := os.WriteFile(body, []byte(stream), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fluxLikeSpec() HelmChartSpec {
	return HelmChartSpec{
		Name: "flux", OCI: "oci://example/flux", Version: "1", Namespace: "flux-system",
		Owner: chartOwnerFixture, Prune: true,
	}
}

// renderRelease renders spec through the real RenderHelmChart and returns it as
// the renderedChart the apply loop takes.
func renderRelease(t *testing.T, spec HelmChartSpec) renderedChart {
	t.Helper()
	out, err := RenderHelmChart(context.Background(), spec)
	if err != nil {
		t.Fatalf("RenderHelmChart: %v", err)
	}
	return renderedChart{spec: spec, manifests: out}
}

func applyRelease(t *testing.T, spec HelmChartSpec) {
	t.Helper()
	rc := renderRelease(t, spec)
	if err := applyRenderedCharts(context.Background(), "k3d-test", []renderedChart{rc}, true); err != nil {
		t.Fatalf("applyRenderedCharts: %v", err)
	}
}

const (
	docDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: source-controller
spec: {}`
	docWebhookPolicy = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-webhooks
spec:
  podSelector: {}`
	docImpersonator = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cluster-reconciler-impersonator
rules: []`
	docImpersonatorBinding = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cluster-reconciler-impersonator
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-reconciler-impersonator}`
)

func stream(docs ...string) string { return strings.Join(docs, "\n---\n") }

// An object present in release N and absent from N+1 is gone afterwards; what
// release N+1 still renders is untouched. The three removed objects are the
// ones the security cleanup had to delete by hand: a NetworkPolicy, a
// ClusterRole and its ClusterRoleBinding. The Deployment names no namespace,
// as flux2's 40 objects do, so it also proves a namespace-less object is read
// as living in the chart's namespace and is not mistaken for a departed one.
func TestChartPrune_DeletesObjectsThatLeftTheRender(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()

	setRelease(stream(docDeployment, docWebhookPolicy, docImpersonator, docImpersonatorBinding))
	applyRelease(t, spec)
	for _, o := range [][3]string{
		{"NetworkPolicy", "flux-system", "allow-webhooks"},
		{"ClusterRole", "", "cluster-reconciler-impersonator"},
		{"ClusterRoleBinding", "", "cluster-reconciler-impersonator"},
		{"Deployment", "flux-system", "source-controller"},
	} {
		if !cl.has(o[0], o[1], o[2]) {
			t.Fatalf("release N should have applied %v", o)
		}
	}
	if got := cl.deletes(); len(got) != 0 {
		t.Fatalf("release N deleted %v; the first apply has nothing to prune", got)
	}

	setRelease(stream(docDeployment))
	applyRelease(t, spec)

	for _, o := range [][3]string{
		{"NetworkPolicy", "flux-system", "allow-webhooks"},
		{"ClusterRole", "", "cluster-reconciler-impersonator"},
		{"ClusterRoleBinding", "", "cluster-reconciler-impersonator"},
	} {
		if cl.has(o[0], o[1], o[2]) {
			t.Errorf("%s %s/%s left the render and must be pruned, but is still live", o[0], o[1], o[2])
		}
	}
	if !cl.has("Deployment", "flux-system", "source-controller") {
		t.Error("the Deployment is still rendered and must survive the prune")
	}
	if !cl.has("Namespace", "", "flux-system") {
		t.Error("the chart's Namespace is still rendered and must survive the prune")
	}
}

// A second apply of the SAME render prunes nothing: the prune is a diff, not a
// sweep.
func TestChartPrune_IdempotentOnAnUnchangedRender(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()
	setRelease(stream(docDeployment, docWebhookPolicy))
	applyRelease(t, spec)
	applyRelease(t, spec)
	if got := cl.deletes(); len(got) != 0 {
		t.Errorf("re-applying an unchanged render deleted %v", got)
	}
}

// `kustomize.toolkit.fluxcd.io/prune: disabled` is the escape hatch, and it is
// Flux's own, so one annotation protects an object on both delivery paths. It
// is set on the live object, which is how an operator marks one that must
// outlive the chart. The render does not need to carry it.
func TestChartPrune_PruneDisabledObjectSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()

	setRelease(stream(docDeployment, docWebhookPolicy, docImpersonator))
	applyRelease(t, spec)
	// An operator pins the NetworkPolicy on the live object.
	state := cl.state()
	for _, o := range state.Objects {
		if objKey(o) == fakeKey("NetworkPolicy", "flux-system", "allow-webhooks") {
			md := strMap(o["metadata"])
			md["annotations"] = map[string]any{PruneAnnotation: PruneDisabled}
		}
	}
	saveFakeState(cl.path, state)

	setRelease(stream(docDeployment))
	applyRelease(t, spec)

	if !cl.has("NetworkPolicy", "flux-system", "allow-webhooks") {
		t.Error("a prune: disabled object was deleted")
	}
	if cl.has("ClusterRole", "", "cluster-reconciler-impersonator") {
		t.Error("the unannotated ClusterRole should still have been pruned")
	}
}

// An object a controller owns is the controller's to garbage-collect.
func TestChartPrune_OwnedObjectIsLeftToItsOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()

	setRelease(stream(docDeployment, docWebhookPolicy))
	applyRelease(t, spec)
	state := cl.state()
	for _, o := range state.Objects {
		if objKey(o) == fakeKey("NetworkPolicy", "flux-system", "allow-webhooks") {
			strMap(o["metadata"])["ownerReferences"] = []any{map[string]any{"kind": "Deployment", "name": "x"}}
		}
	}
	saveFakeState(cl.path, state)

	setRelease(stream(docDeployment))
	applyRelease(t, spec)
	if !cl.has("NetworkPolicy", "flux-system", "allow-webhooks") {
		t.Error("an object with ownerReferences was deleted by the prune")
	}
}

// Kinds that hold data, or own everything under them, are reported and kept:
// PVC, StatefulSet, Secret, and a CRD (which is never deleted implicitly).
// The Namespace is covered separately, because forge itself renders it.
func TestChartPrune_StatefulKindsAreKept(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()

	docs := map[string]string{
		"PersistentVolumeClaim": "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\nspec: {}",
		"StatefulSet":           "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: db\nspec: {}",
		"Secret":                "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\nstringData: {a: b}",
		"CustomResourceDefinition": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
			"metadata:\n  name: widgets.acme.dev\nspec: {group: acme.dev}",
	}
	setRelease(stream(docDeployment, docs["PersistentVolumeClaim"], docs["StatefulSet"], docs["Secret"], docs["CustomResourceDefinition"]))
	applyRelease(t, spec)

	// Release N+1 drops all four, and a plain object beside them.
	setRelease(stream(docDeployment))
	applyRelease(t, spec)

	for _, o := range [][3]string{
		{"PersistentVolumeClaim", "flux-system", "data"},
		{"StatefulSet", "flux-system", "db"},
		{"Secret", "flux-system", "creds"},
		{"CustomResourceDefinition", "", "widgets.acme.dev"},
	} {
		if !cl.has(o[0], o[1], o[2]) {
			t.Errorf("%s %s was deleted by the chart prune; stateful kinds are reported and kept", o[0], o[2])
		}
	}
	for _, d := range cl.deletes() {
		for _, kind := range []string{"persistentvolumeclaim", "statefulset", "secret", "customresourcedefinition", "namespace"} {
			if strings.HasPrefix(d, kind) {
				t.Errorf("the prune issued %q for a stateful kind", d)
			}
		}
	}
}

// The Namespace is never deleted. When a chart moves to another namespace the
// old Namespace is departed, and it owns everything inside it.
func TestChartPrune_NamespaceIsNeverDeleted(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()
	setRelease(stream(docDeployment))
	applyRelease(t, spec)

	moved := spec
	moved.Namespace = "flux-system-2"
	applyRelease(t, moved)

	if !cl.has("Namespace", "", "flux-system") {
		t.Error("the old Namespace was deleted when the chart moved; a Namespace owns everything in it")
	}
	if cl.has("Deployment", "flux-system", "source-controller") {
		t.Error("the Deployment left in the old namespace is departed and should have been pruned individually")
	}
	if !cl.has("Deployment", "flux-system-2", "source-controller") {
		t.Error("the Deployment in the new namespace must exist")
	}
	for _, d := range cl.deletes() {
		if strings.HasPrefix(d, "namespace") {
			t.Errorf("the prune issued %q", d)
		}
	}
}

// `prune = false` turns the step off for a chart whose live objects are
// hand-managed; an unset owner turns it off for a spec forge cannot attribute.
func TestChartPrune_DisabledAndOwnerlessChartsDeleteNothing(t *testing.T) {
	for name, mutate := range map[string]func(*HelmChartSpec){
		"prune = false": func(s *HelmChartSpec) { s.Prune = false },
		"no owner":      func(s *HelmChartSpec) { s.Owner = "" },
	} {
		t.Run(name, func(t *testing.T) {
			if testing.Short() {
				t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
			}
			cl := newFakeCluster(t)
			setRelease := fakeHelmReleases(t)
			spec := fluxLikeSpec()
			mutate(&spec)

			setRelease(stream(docDeployment, docWebhookPolicy))
			applyRelease(t, spec)
			setRelease(stream(docDeployment))
			applyRelease(t, spec)

			if !cl.has("NetworkPolicy", "flux-system", "allow-webhooks") {
				t.Errorf("%s: the departed object was deleted", name)
			}
			if got := cl.deletes(); len(got) != 0 {
				t.Errorf("%s: issued deletes %v", name, got)
			}
		})
	}
}

// Another env's, another chart's and a hand-made object are never candidates:
// the listing is label-selected on the owner AND the chart.
func TestChartPrune_OnlyTouchesThisChartForThisOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()

	mk := func(name string, labels map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": map[string]any{"name": name, "namespace": "flux-system", "labels": labels},
		}
	}
	cl.seed(mk("other-env", map[string]any{ChartOwnerLabel: "acme.staging", WorkloadLabel: "flux"}))
	cl.seed(mk("other-chart", map[string]any{ChartOwnerLabel: chartOwnerFixture, WorkloadLabel: "cert-manager"}))
	cl.seed(mk("hand-made", map[string]any{"app": "x"}))
	cl.seed(mk("pre-stamp", map[string]any{WorkloadLabel: "flux"})) // dropped before forge stamped an owner

	setRelease(stream(docDeployment))
	applyRelease(t, spec)

	for _, name := range []string{"other-env", "other-chart", "hand-made", "pre-stamp"} {
		if !cl.has("NetworkPolicy", "flux-system", name) {
			t.Errorf("NetworkPolicy %s is not this chart's for this owner and must be left alone", name)
		}
	}
}

// A failed apply proves nothing about what the render declares, so it prunes
// nothing, however much has left the render.
func TestChartPrune_FailedApplyPrunesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	spec := fluxLikeSpec()

	setRelease(stream(docDeployment, docWebhookPolicy, docImpersonator))
	applyRelease(t, spec)

	setRelease(stream(docDeployment))
	t.Setenv("FORGE_FAKE_FAIL_APPLY", "1")
	rc := renderRelease(t, spec)
	if err := applyRenderedCharts(context.Background(), "k3d-test", []renderedChart{rc}, true); err == nil {
		t.Fatal("the apply was made to fail and reported success")
	}
	if got := cl.deletes(); len(got) != 0 {
		t.Errorf("a failed apply pruned %v", got)
	}
	if !cl.has("NetworkPolicy", "flux-system", "allow-webhooks") {
		t.Error("a failed apply deleted a departed object")
	}
}

// A render that cannot be read in full is not a statement about what the chart
// declares, so nothing is pruned from it.
func TestDesiredChartObjects_UnreadableOrEmptyRenderIsNotUsable(t *testing.T) {
	for name, in := range map[string]string{
		"empty":      "",
		"no objects": "# just a comment\n---\n",
		"malformed":  "apiVersion: v1\nkind: ConfigMap\nmetadata: [oops\n",
	} {
		if _, ok := desiredChartObjects(in, "flux-system"); ok {
			t.Errorf("%s: desiredChartObjects reported a usable render", name)
		}
	}
	if set, ok := desiredChartObjects(docDeployment, "flux-system"); !ok || !set.contains(chartObjectRef{"Deployment", "flux-system", "source-controller"}) {
		t.Errorf("a namespace-less Deployment must be indexed under the chart's namespace; ok=%v set=%+v", ok, set)
	}
	if set, _ := desiredChartObjects(docImpersonator, "flux-system"); !set.contains(chartObjectRef{"ClusterRole", "", "cluster-reconciler-impersonator"}) {
		t.Error("a cluster-scoped object must match by kind and name alone")
	}
}

// planChartPrune's guards, one row each, without a cluster.
func TestPlanChartPrune_Guards(t *testing.T) {
	obj := func(kind, name string, mod func(*liveChartObject)) liveChartObject {
		var o liveChartObject
		o.APIVersion, o.Kind, o.Metadata.Name, o.Metadata.Namespace = "v1", kind, name, "ns"
		if mod != nil {
			mod(&o)
		}
		return o
	}
	desired := chartDesiredSet{
		namespaced:   map[chartObjectRef]bool{{"ConfigMap", "ns", "current"}: true},
		anyNamespace: map[chartObjectRef]bool{{"ConfigMap", "", "current"}: true},
	}
	live := []liveChartObject{
		obj("ConfigMap", "current", nil),
		obj("ConfigMap", "gone", nil),
		obj("PersistentVolumeClaim", "data", nil),
		obj("Secret", "s", nil),
		obj("StatefulSet", "db", nil),
		obj("PersistentVolume", "pv", nil),
		obj("Namespace", "n", nil),
		obj("CustomResourceDefinition", "c", nil),
		obj("ConfigMap", "pinned", func(o *liveChartObject) {
			o.Metadata.Annotations = map[string]string{PruneAnnotation: PruneDisabled}
		}),
		obj("ConfigMap", "owned", func(o *liveChartObject) { o.Metadata.OwnerReferences = []json.RawMessage{[]byte(`{}`)} }),
		obj("ConfigMap", "dying", func(o *liveChartObject) { o.Metadata.DeletionTimestamp = "2026-01-01T00:00:00Z" }),
		obj("ConfigMap", "other-annotation", func(o *liveChartObject) {
			o.Metadata.Annotations = map[string]string{PruneAnnotation: "enabled"}
		}),
	}
	del, kept := planChartPrune(desired, live)
	var delNames, keptNames []string
	for _, o := range del {
		delNames = append(delNames, o.Metadata.Name)
	}
	for _, k := range kept {
		keptNames = append(keptNames, k.Object.Name)
	}
	if want := []string{"gone", "other-annotation"}; !reflect.DeepEqual(delNames, want) {
		t.Errorf("delete = %v, want %v", delNames, want)
	}
	for _, name := range []string{"data", "s", "db", "pv", "n", "c", "pinned", "owned"} {
		found := false
		for _, k := range keptNames {
			found = found || k == name
		}
		if !found {
			t.Errorf("%s should be reported as kept; kept = %v", name, keptNames)
		}
	}
}

// Namespace labels declared on a HelmChart land on the Namespace forge creates
// and CONVERGE: a changed value is updated, a key that is no longer declared is
// removed, and a key another manager set is left alone. The apiserver's own
// kubernetes.io/metadata.name is never disturbed.
func TestChartNamespaceMetadata_ConvergesWithoutFightingOtherManagers(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a fake kubectl (this test binary) through many subprocesses; runs in task test")
	}
	cl := newFakeCluster(t)
	setRelease := fakeHelmReleases(t)
	setRelease(stream(docDeployment))
	spec := fluxLikeSpec()
	spec.NamespaceLabels = map[string]string{
		"pod-security.kubernetes.io/enforce": "baseline",
		"team":                               "platform",
	}
	spec.NamespaceAnnotations = map[string]string{"owner": "sre"}

	applyRelease(t, spec)
	got := cl.labels("Namespace", "", "flux-system")
	if got["pod-security.kubernetes.io/enforce"] != "baseline" || got["team"] != "platform" {
		t.Fatalf("declared labels missing after the first apply: %v", got)
	}
	if got["kubernetes.io/metadata.name"] != "flux-system" {
		t.Errorf("the apiserver's own label was disturbed: %v", got)
	}

	// Another actor labels the Namespace; forge does not own that key.
	cl.setForeignLabel("Namespace", "", "flux-system", "istio-injection", "enabled")

	// Re-apply with a changed value and one declared key dropped.
	spec.NamespaceLabels = map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}
	applyRelease(t, spec)

	got = cl.labels("Namespace", "", "flux-system")
	if got["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("a changed declared label did not converge: %v", got)
	}
	if _, stale := got["team"]; stale {
		t.Errorf("a label forge stopped declaring was left behind: %v", got)
	}
	if got["istio-injection"] != "enabled" {
		t.Errorf("a label another manager set was stripped: %v", got)
	}
	if got["kubernetes.io/metadata.name"] != "flux-system" {
		t.Errorf("the apiserver's own label was stripped: %v", got)
	}
	if got[ChartOwnerLabel] != chartOwnerFixture {
		t.Errorf("the Namespace is not stamped with the chart owner: %v", got)
	}
}

// With nothing declared the Namespace is exactly what it always was: no labels
// or annotations beyond forge's own stamps.
func TestNamespaceManifest_NoMetadataByDefault(t *testing.T) {
	m, ok := parseDoc(namespaceManifest("flux-system", nil, nil))
	if !ok || m.Kind != "Namespace" || m.Metadata.Name != "flux-system" {
		t.Fatalf("namespaceManifest = %+v ok=%v", m, ok)
	}
	if len(m.Metadata.Labels) != 0 || len(m.Metadata.Annotations) != 0 {
		t.Errorf("an undeclared Namespace carries metadata: %+v", m.Metadata)
	}
	with, _ := parseDoc(namespaceManifest("psa", map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}, map[string]string{"a": "b"}))
	if with.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" || with.Metadata.Annotations["a"] != "b" {
		t.Errorf("declared metadata not rendered: %+v", with.Metadata)
	}
}

// A chart may not declare a label forge stamps on the Namespace it creates.
// RenderHelmChart refuses the spec, naming the key, before it runs helm.
// Without the refusal the declared value would decide the outcome silently: it
// would replace forge's app.kubernetes.io/name and managed-by, which
// stampDocAppLabel only defaults, and it would be overwritten on the forced
// forge.dev/* keys.
func TestRenderHelmChart_RefusesForgeOwnedNamespaceMetadata(t *testing.T) {
	requirePOSIXFake(t, "helm")
	// A helm that leaves a mark, so the test proves the refusal comes BEFORE the
	// render, which is a network operation.
	dir := t.TempDir()
	mark := filepath.Join(dir, "helm-ran")
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte("#!/bin/sh\ntouch "+mark+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tc := range []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        string
	}{
		{"app name", map[string]string{AppNameLabel: "flux2"}, nil, "label app.kubernetes.io/name"},
		{"managed-by", map[string]string{AppManagedByLabel: "flux"}, nil, "label app.kubernetes.io/managed-by"},
		{"workload", map[string]string{WorkloadLabel: "other"}, nil, "label forge.dev/workload"},
		{"chart owner", map[string]string{ChartOwnerLabel: "acme.staging"}, nil, "label forge.dev/chart-owner"},
		{"any forge.dev key", map[string]string{"forge.dev/anything": "x"}, nil, "label forge.dev/anything"},
		{"apiserver's name", map[string]string{"kubernetes.io/metadata.name": "x"}, nil, "label kubernetes.io/metadata.name"},
		{"forge.dev annotation", nil, map[string]string{"forge.dev/note": "x"}, "annotation forge.dev/note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := fluxLikeSpec()
			spec.NamespaceLabels = tc.labels
			spec.NamespaceAnnotations = tc.annotations
			// A PSA label beside it is fine on its own and must not mask the refusal.
			if spec.NamespaceLabels == nil {
				spec.NamespaceLabels = map[string]string{}
			}
			spec.NamespaceLabels["pod-security.kubernetes.io/enforce"] = "restricted"
			_, err := RenderHelmChart(context.Background(), spec)
			if err == nil {
				t.Fatalf("RenderHelmChart accepted namespace metadata %v / %v, which sets a key forge owns", tc.labels, tc.annotations)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"flux"`) {
				t.Errorf("the refusal must name the chart and the key (%q): %v", tc.want, err)
			}
			if _, statErr := os.Stat(mark); statErr == nil {
				t.Error("helm ran for a spec that must be refused before rendering")
			}
		})
	}
}

// The precedence the refusal protects: a declared PSA label lands beside
// forge's own stamps on the Namespace, and forge's stamps are forge's values.
func TestRenderHelmChart_NamespaceCarriesDeclaredAndForgeLabels(t *testing.T) {
	setRelease := fakeHelmReleases(t)
	setRelease(docDeployment)
	spec := fluxLikeSpec()
	spec.NamespaceLabels = map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}
	out, err := RenderHelmChart(context.Background(), spec)
	if err != nil {
		t.Fatalf("RenderHelmChart: %v", err)
	}
	var ns *parsedDoc
	for _, doc := range splitDocs(out) {
		if m, ok := parseDoc(doc); ok && m.Kind == "Namespace" {
			ns = &m
		}
	}
	if ns == nil {
		t.Fatalf("no Namespace in the render:\n%s", out)
	}
	for k, want := range map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted",
		AppNameLabel:                         "flux",
		AppManagedByLabel:                    "forge",
		WorkloadLabel:                        "flux",
		ChartOwnerLabel:                      chartOwnerFixture,
	} {
		if got := ns.Metadata.Labels[k]; got != want {
			t.Errorf("Namespace label %s = %q, want %q (labels %v)", k, got, want, ns.Metadata.Labels)
		}
	}
}

// ChartOwner shares CRDOwner's spelling and, like it, is empty when either half
// is unknown so the stamp and the prune switch off together.
func TestChartOwner(t *testing.T) {
	if got := ChartOwner("acme", "prod"); got != "acme.prod" {
		t.Errorf("ChartOwner = %q", got)
	}
	if ChartOwner("", "prod") != "" || ChartOwner("acme", " ") != "" {
		t.Error("an unnameable owner must be empty")
	}
}
