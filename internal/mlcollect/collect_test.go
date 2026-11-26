package mlcollect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

var managed = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}

func meta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Labels:        map[string]string{"app": name},
		Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"secret":"x"}`},
		ManagedFields: managed,
	}
}

func podSpec(image string) corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{{
		Name: "c", Image: image,
		Env: []corev1.EnvVar{
			{Name: "HF_TOKEN", Value: "hf_supersecret"},
			{Name: "DCGM_EXPORTER_KUBERNETES", Value: "true"},
			{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}},
		},
	}}}
}

func template(name, image string) corev1.PodTemplateSpec {
	m := meta("", name)
	m.Namespace = ""
	return corev1.PodTemplateSpec{ObjectMeta: m, Spec: podSpec(image)}
}

func pmeta(apiVersion, kind, ns, name string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
}

type fixture struct {
	kube *kubefake.Clientset
	md   *metadatafake.FakeMetadataClient
	dyn  *dynamicfake.FakeDynamicClient
}

func (f fixture) clients() Clients { return Clients{Kube: f.kube, Metadata: f.md, Dynamic: f.dyn} }

// newFixture is a GPU cluster: one H100 node, a GPU Operator ClusterPolicy
// with its DCGM exporter, Kueue with two ClusterQueues, DRA serving one
// ResourceSlice, a CRD discovery does not count, and a Secret it must never
// read.
func newFixture(t *testing.T) fixture {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: meta("", "gpu-0"),
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
		},
	}
	node.Labels["nvidia.com/gpu.product"] = "NVIDIA-H100-80GB-HBM3"
	pod := &corev1.Pod{ObjectMeta: meta("gpu-operator", "nvidia-dcgm-exporter-abc"), Spec: podSpec("nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9")}
	kube := kubefake.NewSimpleClientset(
		node, pod,
		&appsv1.DaemonSet{ObjectMeta: meta("gpu-operator", "nvidia-dcgm-exporter"),
			Spec: appsv1.DaemonSetSpec{Template: template("nvidia-dcgm-exporter", "nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9")}},
		&appsv1.Deployment{ObjectMeta: meta("kueue-system", "kueue-controller-manager"),
			Spec: appsv1.DeploymentSpec{Template: template("kueue", "registry.k8s.io/kueue/kueue:v0.10.1")}},
		&appsv1.StatefulSet{ObjectMeta: meta("monitoring", "prometheus"),
			Spec: appsv1.StatefulSetSpec{Template: template("prometheus", "quay.io/prometheus/prometheus:v3"),
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: meta("", "data")}}}},
		&batchv1.Job{ObjectMeta: meta("research", "train"), Spec: batchv1.JobSpec{Template: template("train", "pytorch:2")}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: meta("serving", "vllm")},
		&corev1.Secret{ObjectMeta: meta("default", "do-not-read"), Data: map[string][]byte{"k": []byte("v")}},
		&corev1.ConfigMap{ObjectMeta: meta("gpu-operator", "metrics-config"), Data: map[string]string{
			"dcgm-metrics.csv": "# Format\n# DCGM FIELD, Prometheus metric type, help message\n\nDCGM_FI_DEV_GPU_UTIL, gauge, GPU utilization (in %).\nDCGM_FI_PROF_SM_ACTIVE, gauge, SM activity.\n",
			"notes.sh":         "#!/bin/sh\nexport TOKEN=abc\n",
		}},
		&corev1.ConfigMap{ObjectMeta: meta("default", "never-referenced")},
	)
	disco := kube.Discovery().(*fakediscovery.FakeDiscovery)
	disco.FakedServerVersion = &version.Info{GitVersion: "v1.31.2-gke.1000"}
	disco.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1"},
		{GroupVersion: "nvidia.com/v1"},
		{GroupVersion: "kueue.x-k8s.io/v1beta1"},
		{GroupVersion: "resource.k8s.io/v1"},
		{GroupVersion: "example.com/v1"},
		{GroupVersion: "opentelemetry.io/v1beta1", APIResources: []metav1.APIResource{{Name: "opentelemetrycollectors"}}},
		{GroupVersion: "opentelemetry.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "instrumentations"}, {Name: "opentelemetrycollectors"}}},
	}

	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crd := func(name string) runtime.Object {
		o := pmeta("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name)
		o.ManagedFields = managed
		return o
	}
	md := metadatafake.NewSimpleMetadataClient(scheme,
		crd("clusterpolicies.nvidia.com"), crd("clusterqueues.kueue.x-k8s.io"), crd("foos.example.com"),
		crd("opentelemetrycollectors.opentelemetry.io"), crd("instrumentations.opentelemetry.io"),
		pmeta("opentelemetry.io/v1beta1", "OpenTelemetryCollector", "monitoring", "otel"),
		pmeta("opentelemetry.io/v1alpha1", "Instrumentation", "serving", "python"),
		pmeta("kueue.x-k8s.io/v1beta1", "ClusterQueue", "", "default"),
		pmeta("kueue.x-k8s.io/v1beta1", "ClusterQueue", "", "research"),
		pmeta("resource.k8s.io/v1", "ResourceSlice", "", "gpu-0-gpu.nvidia.com"),
		pmeta("example.com/v1", "Foo", "default", "f"),
	)

	cp := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvidia.com/v1", "kind": "ClusterPolicy",
		"metadata": map[string]any{
			"name":          "cluster-policy",
			"annotations":   map[string]any{"a": "b"},
			"managedFields": []any{map[string]any{"manager": "helm"}},
		},
		"spec": map[string]any{"dcgmExporter": map[string]any{"enabled": true,
			"config": map[string]any{"name": "metrics-config"},
			"env": []any{
				map[string]any{"name": "DCGM_EXPORTER_COLLECTORS", "value": "/etc/dcgm-exporter/dcgm-metrics.csv"},
				map[string]any{"name": "NGC_API_KEY", "value": "nvapi-secret"},
			}}},
	}}
	otel := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "opentelemetry.io/v1beta1", "kind": "OpenTelemetryCollector",
		"metadata": map[string]any{"name": "otel", "namespace": "monitoring"},
		"spec": map[string]any{"config": map[string]any{
			"receivers": map[string]any{"otlp": map[string]any{"protocols": map[string]any{"grpc": map[string]any{}}}},
			"exporters": map[string]any{"otlphttp": map[string]any{"endpoint": "https://otel.example.com",
				"headers": map[string]any{"x-api-token": "abc123"}}},
			"service": map[string]any{"pipelines": map[string]any{"traces": map[string]any{"receivers": []any{"otlp"}, "exporters": []any{"otlphttp"}}}},
		}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}:                    "ClusterPolicyList",
			{Group: "amd.com", Version: "v1alpha1", Resource: "deviceconfigs"}:                   "DeviceConfigList",
			{Group: "opentelemetry.io", Version: "v1beta1", Resource: "opentelemetrycollectors"}: "OpenTelemetryCollectorList",
		}, cp, otel)
	return fixture{kube: kube, md: md, dyn: dyn}
}

var fixedNow = func() time.Time { return time.Date(2026, 11, 25, 9, 30, 0, 123, time.UTC) }

func TestCollectReadsTheCluster(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s, err := Collect(context.Background(), f.clients(), Options{Context: "gke_acme_gpu", Now: fixedNow, ConfigRefs: noRefs})
	if err != nil {
		t.Fatal(err)
	}
	if s.Schema != "forge.dev/ml-snapshot/v1" || s.Context != "gke_acme_gpu" || s.ServerVersion != "v1.31.2-gke.1000" {
		t.Errorf("header = %q %q %q", s.Schema, s.Context, s.ServerVersion)
	}
	if !s.CollectedAt.Equal(time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("collected_at = %v", s.CollectedAt)
	}
	if got := strings.Join(s.APIGroups, ","); got != "example.com,kueue.x-k8s.io,nvidia.com,opentelemetry.io,resource.k8s.io" {
		t.Errorf("api groups = %s", got)
	}
	if got := strings.Join(s.CRDs, ","); got != "clusterpolicies.nvidia.com,clusterqueues.kueue.x-k8s.io,foos.example.com,instrumentations.opentelemetry.io,opentelemetrycollectors.opentelemetry.io" {
		t.Errorf("crds = %s", got)
	}
	if len(s.ReadErrors) != 0 {
		t.Errorf("read errors = %+v", s.ReadErrors)
	}
	if len(s.Nodes) != 1 || len(s.Pods) != 1 || len(s.DaemonSets) != 1 || len(s.Deployments) != 1 ||
		len(s.StatefulSets) != 1 || len(s.Jobs) != 1 || len(s.HPAs) != 1 {
		t.Fatalf("objects: %d nodes %d pods %d ds %d deploy %d sts %d jobs %d hpas",
			len(s.Nodes), len(s.Pods), len(s.DaemonSets), len(s.Deployments), len(s.StatefulSets), len(s.Jobs), len(s.HPAs))
	}
	if q := s.Nodes[0].Status.Capacity["nvidia.com/gpu"]; q.Value() != 8 || s.Nodes[0].Labels["nvidia.com/gpu.product"] == "" {
		t.Errorf("node lost its capacity or labels: %+v", s.Nodes[0])
	}
	// Counts: the ML CRDs that exist and DRA's slices, never an unrelated CRD.
	want := map[string]int{"clusterqueues.kueue.x-k8s.io": 2, "clusterpolicies.nvidia.com": 0, "resourceslices.resource.k8s.io": 1,
		// instrumentations at v1alpha1, the version that serves it, not the group's preferred v1beta1
		"instrumentations.opentelemetry.io": 1, "opentelemetrycollectors.opentelemetry.io": 1}
	if fmt.Sprint(s.CustomResources) != fmt.Sprint(want) {
		t.Errorf("custom resources = %v, want %v", s.CustomResources, want)
	}
	cps := s.CustomObjects["clusterpolicies.nvidia.com"]
	if len(cps) != 1 {
		t.Fatalf("clusterpolicies = %v", s.CustomObjects)
	}
	md := cps[0]["metadata"].(map[string]any)
	if _, ok := md["annotations"]; ok {
		t.Errorf("a custom object kept its annotations: %v", md)
	}
	if _, ok := md["managedFields"]; ok {
		t.Errorf("a custom object kept its managedFields: %v", md)
	}
	if en, _, _ := unstructured.NestedBool(cps[0], "spec", "dcgmExporter", "enabled"); !en {
		t.Errorf("the ClusterPolicy lost its spec: %v", cps[0])
	}
	if name, _, _ := unstructured.NestedString(cps[0], "spec", "dcgmExporter", "config", "name"); name != "metrics-config" {
		t.Errorf("the ClusterPolicy lost its counters ConfigMap name: %v", cps[0])
	}
	env, _, _ := unstructured.NestedSlice(cps[0], "spec", "dcgmExporter", "env")
	if got := fmt.Sprint(env); !strings.Contains(got, "value:/etc/dcgm-exporter/dcgm-metrics.csv") || strings.Contains(got, "nvapi-secret") {
		t.Errorf("ClusterPolicy env: allowlisted value lost or secret kept: %v", got)
	}
	otels := s.CustomObjects["opentelemetrycollectors.opentelemetry.io"]
	if len(otels) != 1 {
		t.Fatalf("collectors = %v", s.CustomObjects)
	}
	hdr, _, _ := unstructured.NestedString(otels[0], "spec", "config", "exporters", "otlphttp", "headers", "x-api-token")
	ep, _, _ := unstructured.NestedString(otels[0], "spec", "config", "exporters", "otlphttp", "endpoint")
	if hdr != Redacted || ep != "https://otel.example.com" {
		t.Errorf("collector spec not redacted as expected: header %q endpoint %q", hdr, ep)
	}
}

// Every stored object loses managedFields, annotations and literal env
// values, its pod template too; names, labels and valueFrom stay.
func TestCollectStripsWhatASnapshotNeverKeeps(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
	if err != nil {
		t.Fatal(err)
	}
	metas := []metav1.ObjectMeta{s.Nodes[0].ObjectMeta, s.Pods[0].ObjectMeta, s.DaemonSets[0].ObjectMeta,
		s.DaemonSets[0].Spec.Template.ObjectMeta, s.Deployments[0].ObjectMeta, s.Deployments[0].Spec.Template.ObjectMeta,
		s.StatefulSets[0].ObjectMeta, s.StatefulSets[0].Spec.Template.ObjectMeta, s.StatefulSets[0].Spec.VolumeClaimTemplates[0].ObjectMeta,
		s.Jobs[0].ObjectMeta, s.Jobs[0].Spec.Template.ObjectMeta, s.HPAs[0].ObjectMeta}
	for i, m := range metas {
		if m.Annotations != nil || m.ManagedFields != nil {
			t.Errorf("object %d (%s) kept annotations %v / managedFields %v", i, m.Name, m.Annotations, m.ManagedFields)
		}
		if m.Labels["app"] == "" {
			t.Errorf("object %d (%s) lost its labels", i, m.Name)
		}
	}
	specs := []corev1.PodSpec{s.Pods[0].Spec, s.DaemonSets[0].Spec.Template.Spec, s.Deployments[0].Spec.Template.Spec,
		s.StatefulSets[0].Spec.Template.Spec, s.Jobs[0].Spec.Template.Spec}
	for i, p := range specs {
		env := p.Containers[0].Env
		if env[0].Name != "HF_TOKEN" || env[0].Value != "" {
			t.Errorf("spec %d: env value kept: %+v", i, env[0])
		}
		if env[1].Value != "true" {
			t.Errorf("spec %d: allowlisted env value blanked: %+v", i, env[1])
		}
		if env[2].ValueFrom == nil || env[2].ValueFrom.SecretKeyRef.Name != "s" {
			t.Errorf("spec %d: valueFrom lost: %+v", i, env[2])
		}
		if p.Containers[0].Image == "" {
			t.Errorf("spec %d lost its image", i)
		}
	}
}

// Every list pages at PageSize, following the continue token.
func TestCollectPaginates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var limits []int64
	f.kube.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		opts := a.(k8stesting.ListActionImpl).ListOptions
		limits = append(limits, opts.Limit)
		start := 0
		if opts.Continue != "" {
			start, _ = strconv.Atoi(opts.Continue)
		}
		const total = 5
		l := &corev1.PodList{}
		for i := start; i < total && i < start+int(opts.Limit); i++ {
			l.Items = append(l.Items, corev1.Pod{ObjectMeta: meta("ns", fmt.Sprintf("p%d", i))})
		}
		if next := start + int(opts.Limit); next < total {
			l.Continue = strconv.Itoa(next)
		}
		return true, l, nil
	})
	s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, PageSize: 2, ConfigRefs: noRefs})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pods) != 5 || s.Pods[4].Name != "p4" {
		t.Fatalf("pods = %d", len(s.Pods))
	}
	if fmt.Sprint(limits) != "[2 2 2]" {
		t.Errorf("list calls = %v, want three pages of 2", limits)
	}
	// The default page is PageSize.
	if l := listLimit(t, newFixture(t)); l != PageSize {
		t.Errorf("default limit = %d, want %d", l, PageSize)
	}
}

func listLimit(t *testing.T, f fixture) int64 {
	t.Helper()
	if _, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs}); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.kube.Actions() {
		if l, ok := a.(k8stesting.ListActionImpl); ok {
			return l.ListOptions.Limit
		}
	}
	t.Fatal("no list action")
	return 0
}

// A refused list is a ReadErrors entry; everything else is still read.
func TestCollectRecordsForbiddenReads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	forbid := func(group, res string) k8stesting.ReactionFunc {
		return func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: group, Resource: res}, "", errors.New("RBAC: no"))
		}
	}
	f.kube.PrependReactor("list", "pods", forbid("", "pods"))
	f.md.PrependReactor("list", "clusterqueues", forbid("kueue.x-k8s.io", "clusterqueues"))
	f.dyn.PrependReactor("list", "clusterpolicies", forbid("nvidia.com", "clusterpolicies"))
	s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.ReadErrors {
		got = append(got, e.Resource)
		if !strings.Contains(e.Error, "forbidden") {
			t.Errorf("%s: error %q does not carry the server's message", e.Resource, e.Error)
		}
	}
	if strings.Join(got, ",") != "clusterpolicies.nvidia.com,clusterqueues.kueue.x-k8s.io,pods" {
		t.Errorf("read errors = %v", got)
	}
	if len(s.Pods) != 0 || len(s.Nodes) != 1 || len(s.DaemonSets) != 1 {
		t.Errorf("a refused list stopped the others: %d pods %d nodes %d ds", len(s.Pods), len(s.Nodes), len(s.DaemonSets))
	}
	if _, ok := s.CustomResources["clusterqueues.kueue.x-k8s.io"]; ok {
		t.Error("a refused count was recorded as a count")
	}
}

func TestCollectFailsOnlyWhenUnreachable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.kube.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	})
	_, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
	if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
}

// The security promise, pinned: the collector only gets and lists exactly
// what ReadSet says with the verb it says (ConfigMaps get by name, never
// list), never touches a Secret or a subresource, and reads nothing else.
func TestCollectorReadsOnlyTheReadSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	allowed := map[string]bool{"get version": true, "get group": true, "get resource": true} // discovery: /version, /api(s)
	for _, r := range ReadSet() {
		if !r.Probe {
			allowed[r.Verb+" "+r.Resource] = true
		}
	}
	var reads []string
	guard := func(client string) k8stesting.ReactionFunc {
		return func(a k8stesting.Action) (bool, runtime.Object, error) {
			res := a.GetResource().Resource
			reads = append(reads, client+":"+a.GetVerb()+":"+res)
			switch {
			case a.GetVerb() != VerbGet && a.GetVerb() != VerbList:
				t.Errorf("%s: verb %q on %s: discovery only gets and lists", client, a.GetVerb(), res)
			case res == "secrets":
				t.Errorf("%s: discovery read Secrets", client)
			case a.GetSubresource() != "":
				t.Errorf("%s: subresource %s/%s read", client, res, a.GetSubresource())
			case !allowed[a.GetVerb()+" "+res]:
				t.Errorf("%s: %s %s is not in ReadSet", client, a.GetVerb(), res)
			}
			return false, nil, nil // fall through to the tracker
		}
	}
	f.kube.PrependReactor("*", "*", guard("kube"))
	f.md.PrependReactor("*", "*", guard("metadata"))
	f.dyn.PrependReactor("*", "*", guard("dynamic"))
	if _, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: fixtureRefs}); err != nil {
		t.Fatal(err)
	}
	if len(reads) < 10 || !slices.Contains(reads, "kube:get:configmaps") {
		t.Errorf("the guard saw %d reads, want the lists and a configmap get: %v", len(reads), reads)
	}
	if slices.Contains(reads, "kube:list:configmaps") {
		t.Error("the collector listed ConfigMaps")
	}
	check := func(name string, rules []rbacv1.PolicyRule, probe bool) {
		for _, r := range rules {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					switch {
					case res == "secrets" || res == "pods/log" || res == "nodes/proxy" || res == "pods/exec":
						t.Errorf("%s grants %s %s", name, v, res)
					case v != VerbGet && v != VerbList:
						t.Errorf("%s grants %q", name, v)
					case res == "configmaps" && v != VerbGet:
						t.Errorf("%s grants %s configmaps: get only", name, v)
					case res == "pods/proxy" && (!probe || v != VerbGet):
						t.Errorf("%s grants %s pods/proxy", name, v)
					case strings.Contains(res, "/") && res != "pods/proxy":
						t.Errorf("%s grants subresource %s", name, res)
					}
				}
			}
		}
	}
	check("ClusterRole", ClusterRole(), false)
	check("ProbeRules", ProbeRules(), true)
	if got := fmt.Sprint(ProbeRules()); !strings.Contains(got, "pods/proxy") || strings.Contains(got, "configmaps") {
		t.Errorf("ProbeRules = %s, want exactly get pods/proxy", got)
	}
}

func noRefs(*mldiscover.Snapshot) []mldiscover.ObjectRef { return nil }

func fixtureRefs(*mldiscover.Snapshot) []mldiscover.ObjectRef {
	return []mldiscover.ObjectRef{
		{Namespace: "gpu-operator", Name: "metrics-config", Why: "dcgm-exporter counters"},
		{Namespace: "gpu-operator", Name: "metrics-config", Why: "duplicate ref, read once"},
		{Namespace: "kube-system", Name: "kubelet-config", Why: "kubelet tracing"},
	}
}

// Phase two GETs each referenced ConfigMap once, redacts it, and records a
// missing one as a read error.
func TestCollectReadsReferencedConfigMaps(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var gets int
	f.kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	var phaseOne *mldiscover.Snapshot
	s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: func(s *mldiscover.Snapshot) []mldiscover.ObjectRef {
		phaseOne = s
		return fixtureRefs(s)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if phaseOne == nil || len(phaseOne.DaemonSets) == 0 {
		t.Fatal("ConfigRefs did not see the first phase's lists")
	}
	if gets != 2 {
		t.Errorf("configmap gets = %d, want 2 (a duplicate ref is read once)", gets)
	}
	if len(s.ConfigMaps) != 1 || s.ConfigMaps[0].Name != "metrics-config" {
		t.Fatalf("configmaps = %+v", s.ConfigMaps)
	}
	cm := s.ConfigMaps[0]
	if cm.Annotations != nil || cm.ManagedFields != nil {
		t.Errorf("configmap kept metadata: %+v", cm.ObjectMeta)
	}
	if cm.Data["dcgm-metrics.csv"] != "DCGM_FI_DEV_GPU_UTIL, gauge, GPU utilization (in %).\nDCGM_FI_PROF_SM_ACTIVE, gauge, SM activity.\n" {
		t.Errorf("the CSV was not kept (minus comments): %q", cm.Data["dcgm-metrics.csv"])
	}
	if cm.Data["notes.sh"] != Dropped {
		t.Errorf("unparsable data kept: %q", cm.Data["notes.sh"])
	}
	if len(s.ReadErrors) != 0 {
		t.Errorf("an absent ConfigMap is not a read error: %+v", s.ReadErrors)
	}
	// A refused get is.
	f = newFixture(t)
	f.kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "metrics-config", errors.New("RBAC: no"))
	})
	s, err = Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: fixtureRefs})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ReadErrors) != 2 || s.ReadErrors[0].Resource != "configmaps" || !strings.Contains(s.ReadErrors[0].Error, "gpu-operator/metrics-config (dcgm-exporter counters)") {
		t.Errorf("read errors = %+v", s.ReadErrors)
	}
}

func TestReadSetTableNamesEveryRead(t *testing.T) {
	t.Parallel()
	table := ReadSetTable()
	for _, r := range ReadSet() {
		if !strings.Contains(table, r.Name()) {
			t.Errorf("ReadSetTable omits %s", r.Name())
		}
		if r.Verb != VerbGet && r.Verb != VerbList {
			t.Errorf("%s: verb %q", r.Name(), r.Verb)
		}
	}
}

// A group whose discovery failed (metrics-server's aggregated API not ready
// on a fresh cluster) leaves the partial answer in use: a group the read set
// does not need is ignored, one it needs is a ReadError naming it, and the
// rest of the read goes on.
func TestCollectToleratesPartialAPIDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		failed map[schema.GroupVersion]error
		want   string
	}{
		{"unneeded group", map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("stale GroupVersion discovery: metrics.k8s.io/v1beta1"),
		}, ""},
		{"needed group", map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("stale"),
			{Group: "kueue.x-k8s.io", Version: "v1beta1"}: errors.New("stale GroupVersion discovery: kueue.x-k8s.io/v1beta1"),
		}, "/apis/kueue.x-k8s.io/v1beta1"},
	} {
		f := newFixture(t)
		f.kube.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, &discovery.ErrGroupDiscoveryFailed{Groups: tc.failed}
		})
		s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []string
		for _, e := range s.ReadErrors {
			got = append(got, e.Resource)
		}
		if strings.Join(got, ",") != tc.want {
			t.Errorf("%s: read errors = %+v, want %q", tc.name, s.ReadErrors, tc.want)
		}
		if !slices.Contains(s.APIGroups, "kueue.x-k8s.io") || s.CustomResources["clusterqueues.kueue.x-k8s.io"] != 2 || len(s.Nodes) != 1 {
			t.Errorf("%s: the partial result was not used: groups %v counts %v", tc.name, s.APIGroups, s.CustomResources)
		}
	}
	// Any other discovery error is still a /apis read error.
	f := newFixture(t)
	f.kube.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
	if err != nil || len(s.ReadErrors) != 1 || s.ReadErrors[0].Resource != "/apis" {
		t.Errorf("a whole discovery failure: %v %+v", err, s.ReadErrors)
	}
}
