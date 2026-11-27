package mldiscover

import "time"

// Inventory is what Discover makes of a Snapshot: the cluster's accelerators,
// its machines, and the ML components it already runs. It is vendor-neutral:
// a device is a device whichever exporter reports it.
//
// Every field is part of the published `forge ml discover --json` document
// (forge.dev/ml-discover/v1). Fields are only ever added.
type Inventory struct {
	// Context is the kube context the snapshot was read through.
	Context string `json:"context,omitempty"`
	// ServerVersion is the API server's gitVersion.
	ServerVersion string `json:"server_version,omitempty"`
	// CollectedAt is when the snapshot was read.
	CollectedAt time.Time `json:"collected_at"`
	// Cloud is where the nodes run, from their labels and provider IDs:
	// one of CloudGKE, CloudEKS, CloudAKS, CloudOther.
	Cloud string `json:"cloud"`
	// Provider is the infrastructure the nodes' providerIDs name, managed
	// service or not: one of ProviderGCP, ProviderAWS, ProviderAzure, or
	// empty. A self-managed cluster on EC2 is Cloud "other", Provider "aws".
	Provider string `json:"provider,omitempty"`
	// Accelerators groups the accelerator nodes by (vendor, resource,
	// product, pool).
	Accelerators []AcceleratorGroup `json:"accelerators"`
	// Machines is one row per node that has accelerator capacity.
	Machines []Machine `json:"machines"`
	// Components is every ML-relevant component found, in a stable order
	// (category, then kind, then namespace).
	Components []Component `json:"components"`
	// Coverage is one row per observability signal an ML cluster should
	// have (see the Signal constants): whether it is collected, by what, how
	// that was determined, and what is missing. It is what the console's
	// coverage-gap view reads. Every signal in Signals() has a row, so an
	// absent signal is a row with StatusMissing, never an absent row.
	Coverage []SignalCoverage `json:"coverage"`
	// Warnings says what discovery could not see (a refused list, a CRD
	// whose controller is missing), so an absence is never overstated.
	Warnings []string `json:"warnings,omitempty"`
}

// Cloud values.
const (
	CloudGKE   = "gke"
	CloudEKS   = "eks"
	CloudAKS   = "aks"
	CloudOther = "other"
)

// Vendor values. They match forge.Accelerators.vendor in KCL.
const (
	VendorNVIDIA  = "nvidia"
	VendorAMD     = "amd"
	VendorTPU     = "tpu"
	VendorNeuron  = "neuron"
	VendorGaudi   = "gaudi"
	VendorGeneric = "generic"
)

// AcceleratorGroup is a set of nodes carrying the same accelerator.
type AcceleratorGroup struct {
	// Vendor is one of the Vendor constants.
	Vendor string `json:"vendor"`
	// Resource is the extended resource the devices are requested by
	// (nvidia.com/gpu, amd.com/gpu, google.com/tpu,
	// aws.amazon.com/neuroncore, habana.ai/gaudi).
	Resource string `json:"resource"`
	// Product is the device model when a label names it (NVIDIA-H100-80GB-HBM3,
	// from GFD's nvidia.com/gpu.product or cloud.google.com/gke-accelerator).
	Product string `json:"product,omitempty"`
	// Pool is the node pool when a label names it (GKE, EKS, AKS, Karpenter).
	Pool string `json:"pool,omitempty"`
	// Nodes is how many nodes are in the group.
	Nodes int `json:"nodes"`
	// Capacity and Allocatable sum the nodes' status for Resource;
	// Allocatable below Capacity means the device plugin marked devices
	// unhealthy.
	Capacity    int64 `json:"capacity"`
	Allocatable int64 `json:"allocatable"`
	// Allocated sums the Resource limits of the running and pending pods
	// bound to these nodes.
	Allocated int64 `json:"allocated"`
	// Sharing is how devices are shared when a label says so: "mig",
	// "time-slicing", "mps", or empty.
	Sharing string `json:"sharing,omitempty"`
	// DriverVersions lists the distinct driver versions the nodes' labels
	// report (GFD nvidia.com/cuda.driver.*), sorted.
	DriverVersions []string `json:"driver_versions,omitempty"`
	// DetectedBy says how the vendor was recognised: "extended-resource" or
	// "label" (a label with no capacity yet: the device plugin is missing or
	// not ready).
	DetectedBy string `json:"detected_by"`
}

// Machine is one node with accelerator capacity, or with accelerator labels
// and no capacity yet (its group's DetectedBy is "label").
type Machine struct {
	Name        string `json:"name"`
	Vendor      string `json:"vendor"`
	Resource    string `json:"resource"`
	Product     string `json:"product,omitempty"`
	Pool        string `json:"pool,omitempty"`
	Capacity    int64  `json:"capacity"`
	Allocatable int64  `json:"allocatable"`
	Allocated   int64  `json:"allocated"`
	// Ready is the node's Ready condition.
	Ready bool `json:"ready"`
	// Unschedulable is spec.unschedulable (cordoned).
	Unschedulable bool `json:"unschedulable,omitempty"`
	// Spot is true when a cloud spot/preemptible label is set.
	Spot bool `json:"spot,omitempty"`
	// Conditions lists every condition that is not the healthy value for its
	// type (Ready=False, MemoryPressure=True, an NPD condition that is True).
	Conditions []string `json:"conditions,omitempty"`
	// ContainerRuntime is nodeInfo.containerRuntimeVersion
	// (containerd://2.1.4).
	ContainerRuntime string `json:"container_runtime,omitempty"`
	// KubeletVersion is nodeInfo.kubeletVersion.
	KubeletVersion string `json:"kubelet_version,omitempty"`
	// Sharing is the node's device sharing (see AcceleratorGroup.Sharing).
	Sharing string `json:"sharing,omitempty"`
	// DriverVersion is the driver version the node's labels report.
	DriverVersion string `json:"driver_version,omitempty"`
	// DetectedBy is DetectedByResource or DetectedByLabel.
	DetectedBy string `json:"detected_by"`
	// InstanceType is the node.kubernetes.io/instance-type label.
	InstanceType string `json:"instance_type,omitempty"`
}

// Component categories.
const (
	CategoryOperator     = "operator"       // GPU Operator, AMD GPU Operator
	CategoryDevicePlugin = "device-plugin"  // NVIDIA/Neuron device plugin, DRA driver
	CategoryAccelMetrics = "accel-metrics"  // DCGM exporter, AMD device-metrics-exporter, neuron-monitor
	CategoryNodeHealth   = "node-health"    // node-problem-detector, EKS node monitoring agent
	CategoryMonitoring   = "monitoring"     // Prometheus Operator, GMP, kube-state-metrics, OTel collector
	CategoryQueue        = "queue"          // Kueue, Volcano
	CategoryTraining     = "training"       // JobSet, Kubeflow Trainer / Training Operator, LWS, KubeRay
	CategoryServing      = "serving"        // KServe, Inference Extension, vLLM, Triton
	CategoryAutoscaling  = "autoscaling"    // KEDA, Karpenter
	CategoryPullCache    = "pull-cache"     // Spegel, Dragonfly
	CategoryLifecycle    = "node-lifecycle" // aws-node-termination-handler
	CategoryProfiling    = "profiling"      // Parca, Pyroscope, OTel eBPF profiler, Elastic, Coroot, Pixie
	CategoryTracing      = "tracing"        // Grafana Beyla (eBPF tracing)
)

// Owner values.
const (
	// OwnerTheirs: installed by someone else. forge adopts it and never
	// upgrades, reconfigures or removes it.
	OwnerTheirs = "theirs"
	// OwnerForge: carries forge.dev/component-owner, so a forge.Install put
	// it there.
	OwnerForge = "forge"
)

// Status values.
const (
	// StatusPresent: a running controller or workload matched.
	StatusPresent = "present"
	// StatusCRDOnly: the CRDs are installed but no controller runs (a
	// leftover from an uninstall, or a controller in another cluster).
	StatusCRDOnly = "crd-only"
)

// Component is one ML-relevant component discovery found.
type Component struct {
	// Kind is the stable component id, the same vocabulary as
	// forge.Adopt.kind / forge.Install.kind (see Kinds).
	Kind string `json:"kind"`
	// Category is one of the Category constants.
	Category string `json:"category"`
	// Vendor is set for vendor-specific components.
	Vendor string `json:"vendor,omitempty"`
	// Status is StatusPresent or StatusCRDOnly.
	Status string `json:"status"`
	// Owner is OwnerTheirs or OwnerForge.
	Owner string `json:"owner"`
	// Version is the image tag (or chart label) of the matched workload.
	Version string `json:"version,omitempty"`
	// Namespace and Workload locate the matched object (DaemonSet/nvidia-dcgm-exporter).
	Namespace string `json:"namespace,omitempty"`
	Workload  string `json:"workload,omitempty"`
	// Via names what manages it when that is not the component itself:
	// "gpu-operator" (a DCGM exporter the ClusterPolicy deploys) or
	// "gke-managed" (gke-managed-system).
	Via string `json:"via,omitempty"`
	// Selector is the matched workload's pod selector, what forge.Adopt
	// scrapes by.
	Selector map[string]string `json:"selector,omitempty"`
	// MetricsPort is the container port the exporter serves metrics on.
	MetricsPort int32 `json:"metrics_port,omitempty"`
	// Pods counts the component's running pods (a DaemonSet's ready pods).
	Pods int `json:"pods,omitempty"`
	// Resources counts the custom objects of its CRDs (ClusterQueues,
	// InferenceServices).
	Resources int `json:"resources,omitempty"`
	// MatchedBy lists the signatures that matched, as "crd:<name>",
	// "image:<pattern>", "label:<key>", "capacity:<resource>",
	// "clusterpolicy:<field>", "group:<api group>".
	MatchedBy []string `json:"matched_by"`
	// ChartVersion is the version in the workload's helm.sh/chart label,
	// when a helm chart installed it.
	ChartVersion string `json:"chart_version,omitempty"`
	// Config is what discovery learned of the component's configuration
	// (the DCGM fields it exports, NPD's monitors and conditions, a
	// collector's receivers), nil when it learned nothing.
	Config *ComponentConfig `json:"config,omitempty"`
}

// Provider values.
const (
	ProviderGCP   = "gcp"
	ProviderAWS   = "aws"
	ProviderAzure = "azure"
)

// Has reports whether a component of kind is present (StatusPresent).
func (inv *Inventory) Has(kind string) bool {
	for _, c := range inv.Components {
		if c.Kind == kind && c.Status == StatusPresent {
			return true
		}
	}
	return false
}

// Find returns every component of kind, present or CRD-only.
func (inv *Inventory) Find(kind string) []Component {
	var out []Component
	for _, c := range inv.Components {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}
