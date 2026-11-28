package mlcollect

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/reliant-labs/forge/internal/mldiscover"
)

// Exercise the saved representation, not just the individual sanitizers.
func TestCollectSerializedRetainedFields(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	const sas = "https://account.blob.core.windows.net/disks/disk.vhd?sv=2024&sp=r&sig=opaque-sas-signature&%73ig=opaque-sas-signature"
	if err := f.kube.Tracker().Add(&corev1.Node{ObjectMeta: meta("", "retention"),
		Spec: corev1.NodeSpec{ProviderID: sas}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Message: "node-message-secret"}},
			Images: []corev1.ContainerImage{{Names: []string{sas}}},
		}}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: meta("default", "retention"), Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: "vllm:latest"}},
		Volumes: []corev1.Volume{{Name: "disk", VolumeSource: corev1.VolumeSource{
			AzureDisk: &corev1.AzureDiskVolumeSource{DiskName: "disk", DataDiskURI: sas},
		}}},
	}, Status: corev1.PodStatus{Message: "pod-diagnostic-secret", Conditions: []corev1.PodCondition{{Message: "pod-condition-secret"}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Message: "termination-secret"},
		}}},
	}}
	if _, err := f.kube.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	objects := []struct {
		resource schema.GroupVersionResource
		message  string
	}{
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, "daemonset-message-secret"},
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "deployment-message-secret"},
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "statefulset-message-secret"},
		{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "job-message-secret"},
		{schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}, "hpa-message-secret"},
	}
	for _, o := range objects {
		switch o.resource.Resource {
		case "daemonsets":
			if err := f.kube.Tracker().Add(&appsv1.DaemonSet{ObjectMeta: meta("default", "retention"), Status: appsv1.DaemonSetStatus{Conditions: []appsv1.DaemonSetCondition{{Message: o.message}}}}); err != nil {
				t.Fatal(err)
			}
		case "deployments":
			if err := f.kube.Tracker().Add(&appsv1.Deployment{ObjectMeta: meta("default", "retention"), Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Message: o.message}}}}); err != nil {
				t.Fatal(err)
			}
		case "statefulsets":
			if err := f.kube.Tracker().Add(&appsv1.StatefulSet{ObjectMeta: meta("default", "retention"), Status: appsv1.StatefulSetStatus{Conditions: []appsv1.StatefulSetCondition{{Message: o.message}}}}); err != nil {
				t.Fatal(err)
			}
		case "jobs":
			if err := f.kube.Tracker().Add(&batchv1.Job{ObjectMeta: meta("default", "retention"), Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Message: o.message}}}}); err != nil {
				t.Fatal(err)
			}
		case "horizontalpodautoscalers":
			if err := f.kube.Tracker().Add(&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: meta("default", "retention"), Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{Message: o.message}}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	gvr := schema.GroupVersionResource{Group: "opentelemetry.io", Version: "v1beta1", Resource: "opentelemetrycollectors"}
	u, err := f.dyn.Resource(gvr).Namespace("monitoring").Get(ctx, "otel", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u.SetLabels(map[string]string{"api-token": "opaque-custom-label", "app": "otel", "endpoint": "hf_abcdefghijklmnopqrstuvwxyz"})
	u.Object["status"] = map[string]any{"password": "custom-status-secret"}
	u.Object["spec"].(map[string]any)["endpoint"] = sas
	u.Object["spec"].(map[string]any)["inline"] = `{"password":"inline-document-secret"}`
	if _, err := f.dyn.Resource(gvr).Namespace("monitoring").Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: meta("default", "retention"), Data: map[string]string{
		"config.yaml": "endpoint: " + sas + "\nheaders:\n  x-test: config-header-secret\nnested:\n  password: config-password-secret\n",
		"config.json": `{"env":[{"name":"PRIVATE","value":"config-env-secret"}],"args":["--token","config-arg-secret"],"safe":"kept-config-value"}`,
		"script":      "echo opaque-script-secret",
		"api-token":   "config-key-secret",
	}, BinaryData: map[string][]byte{"binary": []byte("binary-secret")}}
	if _, err := f.kube.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err := Collect(ctx, f.clients(), Options{Now: fixedNow, ConfigRefs: func(*mldiscover.Snapshot) []mldiscover.ObjectRef {
		return []mldiscover.ObjectRef{{Namespace: "default", Name: "retention"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"opaque-sas-signature", "opaque-custom-label", "hf_abcdefghijklmnopqrstuvwxyz", "custom-status-secret", "config-header-secret", "config-password-secret", "config-env-secret", "config-arg-secret", "opaque-script-secret", "config-key-secret", "binary-secret", "pod-diagnostic-secret", "pod-condition-secret", "termination-secret", "node-message-secret", "inline-document-secret"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("serialized snapshot retained %q", secret)
		}
	}
	for _, o := range objects {
		if strings.Contains(string(b), o.message) {
			t.Errorf("serialized snapshot retained %s diagnostic", o.resource.Resource)
		}
	}
	if !strings.Contains(string(b), "kept-config-value") || !strings.Contains(string(b), "vllm:latest") {
		t.Error("snapshot lost discovery inputs")
	}
	if len(s.ReadErrors) != 0 || len(s.ConfigMaps) != 1 || len(s.CustomObjects["opentelemetrycollectors.opentelemetry.io"]) != 1 {
		t.Fatal("redaction regression did not collect its inputs")
	}
}
