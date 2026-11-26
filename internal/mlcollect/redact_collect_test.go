package mlcollect

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// credentialPod carries a synthetic credential in every pod field the
// collector must sanitise: a flag value, a flag's next argument, a shell
// script's variable, header and URL password, an exec probe, and the
// headers of HTTP probes and lifecycle hooks.
func credentialPod() corev1.PodSpec {
	hdr := []corev1.HTTPHeader{{Name: "Authorization", Value: "Bearer probe-hdr-secret"}, {Name: "X-Tenant", Value: "tenant-hdr-secret"}}
	return corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "fetch", Image: "amazon/aws-cli",
			Command: []string{"sh", "-c"},
			Args:    []string{`export AWS_SECRET_ACCESS_KEY=script-var-secret; curl -H "Authorization: Bearer script-hdr-secret" https://u:script-url-secret@m.example/w; aws s3 cp s3://models/llama /models`}}},
		Containers: []corev1.Container{{
			Name: "vllm", Image: "vllm/vllm-openai:v0.6",
			Args: []string{"--model=meta-llama/Llama-3-8B", "--hf-token=arg-eq-secret", "--api-key", "arg-next-secret"},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/health?token=probe-path-secret", HTTPHeaders: hdr}}},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"check", "--password=exec-probe-secret"}}}},
			Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/drain", HTTPHeaders: []corev1.HTTPHeader{{Name: "Authorization", Value: "Bearer hook-hdr-secret"}}}}},
		}},
	}
}

var credentialSecrets = []string{
	"script-var-secret", "script-hdr-secret", "script-url-secret", "arg-eq-secret", "arg-next-secret",
	"probe-hdr-secret", "tenant-hdr-secret", "probe-path-secret", "exec-probe-secret", "hook-hdr-secret",
}

// TestCollectRedactsArgsAndProbeHeaders: a live read keeps no credential a
// pod's command, args, probes or lifecycle hooks carry.
func TestCollectRedactsArgsAndProbeHeaders(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "serving", Name: "vllm-0"}, Spec: credentialPod()}
	ds := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "serving", Name: "vllm"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: credentialPod()}}}
	kube := kubefake.NewSimpleClientset(pod, ds)
	s, err := Collect(context.Background(), minimalClients(t, kube), Options{Now: fixedNow, ConfigRefs: noRefs})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range credentialSecrets {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the snapshot carries %q", secret)
		}
	}
	// What discovery reads survives.
	for _, keep := range []string{"s3://models/llama", "--model=meta-llama/Llama-3-8B", `"Authorization"`, "/drain"} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("the snapshot lost %s", keep)
		}
	}
}

const alloyRiver = `discovery.kubernetes "pods" {
  role = "pod"
}

pyroscope.ebpf "default" {
  targets    = discovery.kubernetes.pods.targets
  forward_to = [pyroscope.write.backend.receiver]
}

pyroscope.write "backend" {
  endpoint {
    url = "https://profiles.lab.example"
    basic_auth {
      username = "lab"
      password = "alloy-basic-auth-secret"
    }
  }
}

loki.source.kubernetes "pods" {
  targets    = discovery.kubernetes.pods.targets
  forward_to = [loki.write.default.receiver]
}

loki.write "default" {
  endpoint {
    url     = "https://tenant:alloy-url-secret@logs.lab.example/loki/api/v1/push"
    headers = {"X-Scope-OrgID" = "alloy-header-secret"}
  }
}
`

// TestCollectAlloyConfigThroughDiscovery: a live read of an Alloy
// DaemonSet's River config keeps none of its credentials and still tells
// discovery which components it runs, so profiling and container-log
// coverage are established from the config.
func TestCollectAlloyConfigThroughDiscovery(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-0"}, Status: corev1.NodeStatus{
		Capacity:    corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
		Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
	}}
	sel := map[string]string{"app.kubernetes.io/name": "alloy"}
	alloy := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "alloy", Labels: sel},
		Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: sel}, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: sel},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "alloy", Image: "grafana/alloy:v1.5.1",
					Args:         []string{"run", "/etc/alloy/config.alloy"},
					VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/alloy"}}}},
				Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "alloy-config"}}}}},
			}}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "alloy-config"},
		Data: map[string]string{"config.alloy": alloyRiver}}
	kube := kubefake.NewSimpleClientset(node, alloy, cm)

	s, err := Collect(context.Background(), minimalClients(t, kube), Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"alloy-basic-auth-secret", "alloy-url-secret", "alloy-header-secret", "profiles.lab.example"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the snapshot carries %q", secret)
		}
	}
	if len(s.ConfigMaps) != 1 || !strings.Contains(s.ConfigMaps[0].Data["config.alloy"], `pyroscope.ebpf "default"`) {
		t.Fatalf("the Alloy config was not kept as its component blocks: %+v", s.ConfigMaps)
	}

	inv := mldiscover.Discover(s)
	var cfg *mldiscover.ComponentConfig
	for _, c := range inv.Components {
		if c.Kind == mldiscover.KindAlloy {
			cfg = c.Config
		}
	}
	if cfg == nil || cfg.DeterminedBy != mldiscover.DeterminedByConfig ||
		!reflect.DeepEqual(cfg.Receivers, []string{"loki.source.kubernetes", "pyroscope.ebpf"}) ||
		!reflect.DeepEqual(cfg.Exporters, []string{"loki.write", "pyroscope.write"}) {
		t.Fatalf("Alloy config = %+v", cfg)
	}
	for _, signal := range []string{mldiscover.SignalContinuousProfiling, mldiscover.SignalContainerLogs} {
		found := false
		for _, r := range inv.Coverage {
			if r.Signal != signal {
				continue
			}
			found = true
			if r.Status != mldiscover.CoverageCollected || r.DeterminedBy != mldiscover.DeterminedByConfig {
				t.Errorf("%s: %s by %s (%s), want collected by config", signal, r.Status, r.DeterminedBy, r.Note)
			}
		}
		if !found {
			t.Errorf("no %s coverage row", signal)
		}
	}
}

// minimalClients is kube with empty metadata and dynamic clients and a
// v1-only discovery.
func minimalClients(t *testing.T, kube *kubefake.Clientset) Clients {
	t.Helper()
	disco := kube.Discovery().(*fakediscovery.FakeDiscovery)
	disco.FakedServerVersion = &version.Info{GitVersion: "v1.33.1"}
	disco.Resources = []*metav1.APIResourceList{{GroupVersion: "v1"}}
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return Clients{Kube: kube, Metadata: metadatafake.NewSimpleMetadataClient(scheme),
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
}
