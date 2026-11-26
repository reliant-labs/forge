package mldiscover

import (
	"strings"
)

// Via values.
const (
	ViaGPUOperator = "gpu-operator"
	ViaGKEManaged  = "gke-managed"
)

// Well-known namespaces.
const (
	NamespaceGKEManagedSystem = "gke-managed-system"
)

// Default metrics ports by component kind, used when the matched
// container names no `metrics` port. DCGM's 9400 is upstream's default;
// the others are the exporters' documented defaults, unverified against a
// live cluster.
var defaultMetricsPorts = map[string]int32{
	KindDCGMExporter:       9400,
	KindAMDMetricsExporter: 5000,
	KindNeuronMonitor:      8000,
	KindGaudiExporter:      41611,
}

// DefaultMetricsPort is the port an exporter kind serves metrics on by
// default, or 0.
func DefaultMetricsPort(kind string) int32 { return defaultMetricsPorts[kind] }

// signature is one row of Appendix B.
type signature struct {
	kind, category, vendor string
	// crds and groups: any CRD named, or any CRD in a group.
	crds   []string
	groups []string
	// images are globs (`*` matches anything) over the lowercase image
	// repository, without tag or digest; exclude vetoes a match.
	images  []string
	exclude []string
	// containers match container names exactly.
	containers []string
	// labels match a workload's pod-template (or a pod's) labels, as
	// "key=value".
	labels []string
	// names match workload-name prefixes (GKE's nvidia-gpu-device-plugin-*).
	names []string
	// args match substrings of a container's command and args joined by
	// spaces (vllm serve).
	args []string
	// workloads restricts the workload kinds that may match (nil: all).
	workloads []string
	// pods: also match pods whose owner the snapshot does not hold, or that
	// have none (model servers, exporters).
	pods bool
	// objects: the CRD plus at least one object of it is present, with no
	// controller to find (InferencePool is read by the gateway).
	objects bool
	// namespaceImages: any image in this namespace containing the
	// substring matches (GKE managed DCGM in gke-managed-system).
	namespace, namespaceImage string
}

// signatures is Appendix B of docs/proposals/ml-workloads.md, in a stable
// order. Kinds matched by node facts (capacity, conditions, labels) or by
// the ClusterPolicy are finished in discoverSpecial.
var signatures = []signature{
	{kind: KindGPUOperator, category: CategoryOperator, vendor: VendorNVIDIA,
		crds: []string{"clusterpolicies.nvidia.com"}, objects: true,
		images: []string{"*/gpu-operator"}, exclude: []string{"*rocm*", "*amd*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindAMDGPUOperator, category: CategoryOperator, vendor: VendorAMD,
		crds: []string{"deviceconfigs.amd.com"}, objects: true,
		images: []string{"*rocm/gpu-operator*", "*amd-gpu-operator*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindNVIDIADevicePlugin, category: CategoryDevicePlugin, vendor: VendorNVIDIA,
		images: []string{"*k8s-device-plugin", "*nvidia-gpu-device-plugin*"}, exclude: []string{"*rocm*", "*amd*"},
		names: []string{"nvidia-gpu-device-plugin"}, workloads: []string{WorkloadDaemonSet}},
	{kind: KindNeuronDevicePlugin, category: CategoryDevicePlugin, vendor: VendorNeuron,
		images: []string{"*neuron-device-plugin*"}, workloads: []string{WorkloadDaemonSet}},
	{kind: KindDRA, category: CategoryDevicePlugin},
	{kind: KindDCGMExporter, category: CategoryAccelMetrics, vendor: VendorNVIDIA,
		images: []string{"*dcgm-exporter*"}, pods: true,
		// fake-gpu-operator's status-exporter takes the operator's
		// container name and app label on a different image.
		containers: []string{"nvidia-dcgm-exporter", "dcgm-exporter"},
		labels:     []string{"app=nvidia-dcgm-exporter", "app.kubernetes.io/name=dcgm-exporter"},
		namespace:  NamespaceGKEManagedSystem, namespaceImage: "dcgm"},
	{kind: KindDCGMHostEngine, category: CategoryAccelMetrics, vendor: VendorNVIDIA,
		images: []string{"*/dcgm"}, containers: []string{"nvidia-dcgm", "nvidia-dcgm-ctr"}, pods: true},
	{kind: KindAMDMetricsExporter, category: CategoryAccelMetrics, vendor: VendorAMD,
		images: []string{"*device-metrics-exporter*"}, pods: true},
	{kind: KindNeuronMonitor, category: CategoryAccelMetrics, vendor: VendorNeuron,
		images: []string{"*neuron-monitor*"}, pods: true},
	{kind: KindGaudiExporter, category: CategoryAccelMetrics, vendor: VendorGaudi,
		images: []string{"*habana*metric*", "*gaudi*metric*", "*habana*exporter*"}, pods: true},
	{kind: KindNodeProblemDetector, category: CategoryNodeHealth,
		images: []string{"*node-problem-detector*"}},
	{kind: KindEKSNodeMonitoring, category: CategoryNodeHealth,
		images: []string{"*eks-node-monitoring-agent*"}},
	{kind: KindPrometheusOperator, category: CategoryMonitoring,
		crds:   []string{"servicemonitors.monitoring.coreos.com", "podmonitors.monitoring.coreos.com"},
		images: []string{"*/prometheus-operator"}, workloads: []string{WorkloadDeployment}},
	{kind: KindGMP, category: CategoryMonitoring,
		crds:   []string{"podmonitorings.monitoring.googleapis.com"},
		images: []string{"*prometheus-engine/operator*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindKubeStateMetrics, category: CategoryMonitoring,
		images: []string{"*kube-state-metrics*"}},
	{kind: KindOTelCollector, category: CategoryMonitoring,
		crds:   []string{"opentelemetrycollectors.opentelemetry.io"},
		images: []string{"*opentelemetry-collector*", "*otelcol*"}},
	{kind: KindKueue, category: CategoryQueue,
		groups: []string{"kueue.x-k8s.io"}, images: []string{"*kueue*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindVolcano, category: CategoryQueue,
		groups: []string{"batch.volcano.sh", "scheduling.volcano.sh"},
		images: []string{"*vc-scheduler*", "*vc-controller-manager*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindJobSet, category: CategoryTraining,
		crds: []string{"jobsets.jobset.x-k8s.io"}, images: []string{"*jobset*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindKubeflowTrainer, category: CategoryTraining,
		crds:   []string{"trainjobs.trainer.kubeflow.org"},
		images: []string{"*kubeflow/trainer*", "*trainer-controller-manager*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindKubeflowTraining, category: CategoryTraining,
		crds:   []string{"pytorchjobs.kubeflow.org", "tfjobs.kubeflow.org", "mpijobs.kubeflow.org"},
		images: []string{"*training-operator*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindLWS, category: CategoryTraining,
		crds: []string{"leaderworkersets.leaderworkerset.x-k8s.io"}, images: []string{"*/lws", "*lws-controller*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindKubeRay, category: CategoryTraining,
		groups: []string{"ray.io"}, images: []string{"*kuberay*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindKServe, category: CategoryServing,
		groups: []string{"serving.kserve.io"}, images: []string{"*kserve-controller*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindInferenceExtension, category: CategoryServing, objects: true,
		crds: []string{"inferencepools.inference.networking.k8s.io", "inferencepools.inference.networking.x-k8s.io"}},
	{kind: KindVLLM, category: CategoryServing,
		images: []string{"*vllm*"}, args: []string{"vllm serve", "vllm.entrypoints"}, pods: true,
		workloads: []string{WorkloadDeployment, WorkloadStatefulSet}},
	{kind: KindTriton, category: CategoryServing,
		images: []string{"*tritonserver*"}, pods: true, workloads: []string{WorkloadDeployment, WorkloadStatefulSet}},
	{kind: KindKEDA, category: CategoryAutoscaling,
		crds: []string{"scaledobjects.keda.sh"}, images: []string{"*kedacore/keda", "*/keda"}, workloads: []string{WorkloadDeployment}},
	{kind: KindKarpenter, category: CategoryAutoscaling,
		crds: []string{"nodepools.karpenter.sh"}, images: []string{"*karpenter*"}, workloads: []string{WorkloadDeployment}},
	{kind: KindSpegel, category: CategoryPullCache,
		images: []string{"*spegel*"}, workloads: []string{WorkloadDaemonSet}},
	{kind: KindDragonfly, category: CategoryPullCache,
		images: []string{"*dragonfly*dfdaemon*", "*dfdaemon*"}, workloads: []string{WorkloadDaemonSet}},
	{kind: KindNodeTerminationHandler, category: CategoryLifecycle,
		images: []string{"*aws-node-termination-handler*"}},
}

// matchLabels reports the label signature pod labels match, or "".
func (sig *signature) matchLabels(l map[string]string) string {
	for _, kv := range sig.labels {
		k, v, _ := strings.Cut(kv, "=")
		if l[k] == v {
			return "label:" + kv
		}
	}
	return ""
}

func (sig *signature) allowsWorkload(kind string) bool {
	return len(sig.workloads) == 0 || contains(sig.workloads, kind)
}

// matchContainer reports every signature a container matches.
func (sig *signature) matchContainer(namespace string, name, image string, cmd []string) []string {
	repo := imageRepo(image)
	for _, ex := range sig.exclude {
		if glob(ex, repo) {
			return nil
		}
	}
	var out []string
	for _, p := range sig.images {
		if glob(p, repo) {
			out = append(out, "image:"+p)
			break
		}
	}
	for _, c := range sig.containers {
		if name == c {
			out = append(out, "container:"+c)
		}
	}
	if sig.namespace != "" && namespace == sig.namespace && strings.Contains(repo, sig.namespaceImage) {
		out = append(out, "namespace:"+sig.namespace)
	}
	if len(sig.args) > 0 {
		joined := strings.Join(cmd, " ")
		for _, a := range sig.args {
			if strings.Contains(joined, a) {
				out = append(out, "args:"+a)
				break
			}
		}
	}
	return out
}

// imageRepo is the lowercase repository of an image reference: no tag, no
// digest.
func imageRepo(image string) string {
	image = strings.ToLower(image)
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		image = image[:i]
	}
	return image
}

// ImageTag is the tag of an image reference, or "" when it has none (a
// bare repository, or a digest only).
func ImageTag(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		return image[i+1:]
	}
	return ""
}

// glob matches s against a pattern where `*` matches any run of
// characters, `/` included.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, last)
}

// coreConditions are the node condition types the kubelet itself sets.
var coreConditions = map[string]bool{
	"Ready": true, "MemoryPressure": true, "DiskPressure": true, "PIDPressure": true, "NetworkUnavailable": true,
}

// eksAgentConditions are the conditions the EKS node monitoring agent sets.
var eksAgentConditions = map[string]bool{
	"AcceleratedHardwareReady": true, "ContainerRuntimeReady": true, "KernelReady": true,
	"NetworkingReady": true, "StorageReady": true,
}

// IsCoreCondition reports whether a node condition type is one the kubelet
// sets (Ready and the four pressure/network types).
func IsCoreCondition(t string) bool { return coreConditions[t] }

// IsEKSAgentCondition reports whether a node condition type is one the EKS
// node monitoring agent sets.
func IsEKSAgentCondition(t string) bool { return eksAgentConditions[t] }
