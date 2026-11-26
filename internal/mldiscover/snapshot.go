package mldiscover

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// SnapshotSchema is the apiVersion of a recorded Snapshot.
const SnapshotSchema = "forge.dev/ml-snapshot/v1"

// Snapshot is everything discovery read from one cluster, as plain Kubernetes
// API objects. Discover is pure over it, so the same code runs on a live read
// (`forge ml discover`), on a recorded file (`--from-snapshot`, the golden
// fixtures) and, later, on the control plane's collector inventory.
//
// It holds only what a read-only, get/list grant returns, and never a Secret.
// The collector strips managedFields and every annotation from each object
// before it is stored here, so a recorded snapshot carries no configuration
// beyond labels and specs.
type Snapshot struct {
	// Schema is SnapshotSchema.
	Schema string `json:"schema"`
	// Context is the kube context the snapshot was read through.
	Context string `json:"context,omitempty"`
	// ServerVersion is the API server's gitVersion (v1.31.2-gke.1000).
	ServerVersion string `json:"server_version,omitempty"`
	// CollectedAt is when the read finished.
	CollectedAt time.Time `json:"collected_at"`
	// APIGroups is every API group the server serves, by name
	// (resource.k8s.io, kueue.x-k8s.io).
	APIGroups []string `json:"api_groups,omitempty"`
	// CRDs is the name of every CustomResourceDefinition (plural.group,
	// clusterpolicies.nvidia.com).
	CRDs []string `json:"crds,omitempty"`
	// CustomResources counts the objects of each ML CRD discovery reads
	// (keyed by CRD name), so a CRD with no objects is told apart from one in
	// use.
	CustomResources map[string]int `json:"custom_resources,omitempty"`
	// CustomObjects holds the few custom objects whose spec a rule reads,
	// keyed by CRD name: clusterpolicies.nvidia.com (whether the GPU
	// Operator's DCGM exporter is enabled) and deviceconfigs.amd.com.
	CustomObjects map[string][]map[string]any `json:"custom_objects,omitempty"`

	Nodes        []corev1.Node                           `json:"nodes,omitempty"`
	Pods         []corev1.Pod                            `json:"pods,omitempty"`
	DaemonSets   []appsv1.DaemonSet                      `json:"daemonsets,omitempty"`
	Deployments  []appsv1.Deployment                     `json:"deployments,omitempty"`
	StatefulSets []appsv1.StatefulSet                    `json:"statefulsets,omitempty"`
	Jobs         []batchv1.Job                           `json:"jobs,omitempty"`
	HPAs         []autoscalingv2.HorizontalPodAutoscaler `json:"hpas,omitempty"`

	// ConfigMaps holds only the ConfigMaps ConfigRefs named (an exporter's
	// counters CSV, node-problem-detector's monitor configs, a collector's
	// config, kube-system/kubelet-config, kube-system/kubeadm-config), each
	// read by name with get, never listed. Annotations and managedFields are
	// stripped; data is kept, since it is the configuration being inventoried.
	ConfigMaps []corev1.ConfigMap `json:"configmaps,omitempty"`
	// Probes holds what an opt-in metrics probe (`forge ml discover
	// --probe`) read from the endpoints ProbeTargets named: metric family
	// names and label keys only, never sample values.
	Probes []MetricsProbe `json:"probes,omitempty"`

	// ReadErrors records each list the grant refused or that failed. A
	// missing read is reported, never fatal: discovery says what it could
	// not see instead of claiming a component is absent.
	ReadErrors []ReadError `json:"read_errors,omitempty"`
}

// ReadError is one resource discovery could not list.
type ReadError struct {
	// Resource is the resource as kubectl names it (pods, daemonsets.apps,
	// clusterpolicies.nvidia.com).
	Resource string `json:"resource"`
	// Error is the API server's message.
	Error string `json:"error"`
}
