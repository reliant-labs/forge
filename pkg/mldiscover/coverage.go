package mldiscover

// Signal ids: the stable names of the observability signals an ML cluster
// should have. A signal is only ever added; a rename breaks the console and
// every RecommendationDecision that refers to it by finding.
const (
	// Accelerator metrics (DCGM field groups for NVIDIA; the equivalent
	// families for other vendors).
	SignalAccelUtilisation    = "accel.utilisation"     // DCGM_FI_DEV_GPU_UTIL
	SignalAccelProfiling      = "accel.profiling"       // DCGM_FI_PROF_* (SM/tensor/DRAM activity)
	SignalAccelMemory         = "accel.memory"          // DCGM_FI_DEV_FB_USED/FREE
	SignalAccelXid            = "accel.xid"             // DCGM_FI_DEV_XID_ERRORS
	SignalAccelECC            = "accel.ecc"             // DCGM_FI_DEV_ECC_SBE/DBE_*
	SignalAccelRetiredPages   = "accel.retired-pages"   // DCGM_FI_DEV_RETIRED_SBE/DBE/PENDING (pre-Ampere)
	SignalAccelRowRemap       = "accel.row-remap"       // DCGM_FI_DEV_*_REMAPPED_ROWS, ROW_REMAP_FAILURE
	SignalAccelNVLink         = "accel.nvlink"          // DCGM_FI_DEV_NVLINK_* / DCGM_FI_PROF_NVLINK_*
	SignalAccelPCIe           = "accel.pcie"            // DCGM_FI_DEV_PCIE_REPLAY_COUNTER, DCGM_FI_PROF_PCIE_*
	SignalAccelThrottle       = "accel.throttle"        // DCGM_FI_DEV_CLOCK_THROTTLE_REASONS / CLOCKS_EVENT_REASONS
	SignalAccelPowerThermal   = "accel.power-thermal"   // POWER_USAGE, GPU_TEMP, POWER/THERMAL_VIOLATION
	SignalAccelPodAttribution = "accel.pod-attribution" // pod/namespace/container labels (DCGM_EXPORTER_KUBERNETES)

	// Node health.
	SignalNodeHealth         = "node.health"           // NPD, EKS agent or another source of node conditions
	SignalNodeHealthGPURules = "node.health.gpu-rules" // a monitor or plugin that turns Xid/NVRM kernel lines into a condition

	// Control-plane and kubelet tracing.
	SignalKubeletTracing   = "kubelet.tracing"
	SignalAPIServerTracing = "apiserver.tracing"

	// Cluster telemetry pipeline.
	SignalCollector     = "telemetry.collector" // an OTel (or compatible) collector runs
	SignalOTLPReceiver  = "telemetry.otlp"      // something accepts OTLP traces/metrics from workloads
	SignalK8sEvents     = "telemetry.k8s-events"
	SignalKubeletStats  = "telemetry.kubeletstats"
	SignalContainerLogs = "telemetry.container-logs"

	// Profiling and workload tracing.
	SignalContinuousProfiling = "profiling.continuous" // Parca, Pyroscope/Alloy, OTel eBPF profiler, Elastic, Datadog …
	SignalGPUProfiling        = "profiling.gpu"        // CUDA/Nsight/PyTorch profiler hooks (unknown from the API in general)
	SignalTrainingTracing     = "tracing.training"     // accelerator Jobs exporting OTLP
	SignalInferenceTracing    = "tracing.inference"    // vLLM --otlp-traces-endpoint, Triton tracing, OTLP env on servers
	SignalInferenceMetrics    = "metrics.inference"    // vLLM/Triton /metrics scraped
)

// Coverage status values.
const (
	CoverageCollected     = "collected"      // every expected part is present on every node it applies to
	CoveragePartial       = "partial"        // some parts or some nodes
	CoverageMissing       = "missing"        // nothing collects it
	CoverageUnknown       = "unknown"        // discovery cannot tell from what it may read (DeterminedBy says why)
	CoverageNotApplicable = "not-applicable" // e.g. accelerator signals on a cluster with no accelerators
)

// DeterminedBy values: how a coverage row (or a ComponentConfig) was
// established, strongest first.
const (
	DeterminedByProbe          = "probe"           // metric names read from the live endpoint (--probe)
	DeterminedByConfig         = "config"          // the component's own config (counters CSV, monitor JSON, collector config)
	DeterminedByArgs           = "args"            // container args/env names (--tracing-config-file, -f <csv>)
	DeterminedByVersionDefault = "version-default" // the default config shipped with the found version
	DeterminedByPresence       = "presence"        // only that the component runs
	DeterminedByUnreadable     = "unreadable"      // the config lives where discovery may not read (kubelet file, managed control plane)
)

// SignalCoverage is one signal's coverage row.
type SignalCoverage struct {
	// Signal is one of the Signal constants.
	Signal string `json:"signal"`
	// Category groups signals for display: "accelerator", "node-health",
	// "tracing", "telemetry", "profiling", "inference".
	Category string `json:"category"`
	// Vendor scopes accelerator signals ("nvidia").
	Vendor string `json:"vendor,omitempty"`
	// Status is one of the Coverage constants.
	Status string `json:"status"`
	// DeterminedBy is one of the DeterminedBy constants.
	DeterminedBy string `json:"determined_by"`
	// Sources are the components that provide it, as kind/namespace/workload.
	Sources []string `json:"sources,omitempty"`
	// Expected and Present are the parts of the signal (metric families,
	// receivers, condition types) discovery looked for and found; Missing is
	// Expected minus Present. Empty when the signal has no parts.
	Expected []string `json:"expected,omitempty"`
	Present  []string `json:"present,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	// NodesCovered / NodesTotal count the nodes a node-scoped signal applies
	// to (accelerator nodes for accelerator signals) and those it reaches.
	NodesCovered int `json:"nodes_covered,omitempty"`
	NodesTotal   int `json:"nodes_total,omitempty"`
	// Versions are the source versions found ("dcgm-exporter 3.3.9-3.6.1").
	Versions []string `json:"versions,omitempty"`
	// Note says, in one sentence, why the status is what it is when that is
	// not obvious (an unreadable kubelet config on a managed cluster).
	Note string `json:"note,omitempty"`
	// Findings lists the ids of the findings this gap raised (mlrecs fills
	// it; mldiscover leaves it empty).
	Findings []string `json:"findings,omitempty"`
}

// ComponentConfig is what discovery read of one component's configuration.
// Each list is sorted; a field a component does not have stays empty.
type ComponentConfig struct {
	// DeterminedBy is one of the DeterminedBy constants.
	DeterminedBy string `json:"determined_by"`
	// Source names where it was read ("ConfigMap/gpu-operator/metrics-config
	// key dcgm-metrics.csv", "args", "probe :9400/metrics").
	Source string `json:"source,omitempty"`
	// Fields are exported metric families (DCGM field names, or the probe's
	// metric names).
	Fields []string `json:"fields,omitempty"`
	// Monitors are NPD monitor configs (kernel-monitor, docker-monitor,
	// custom-plugin-monitor names) or equivalent.
	Monitors []string `json:"monitors,omitempty"`
	// Conditions are the node condition types the component sets; Reasons
	// the problem reasons its rules report (NPD's rules).
	Conditions []string `json:"conditions,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	// Receivers, Processors, Exporters and Pipelines describe a telemetry
	// collector's configuration (otlp, kubeletstats, k8s_events, …;
	// pipelines as "traces", "metrics", "logs", "profiles").
	Receivers  []string `json:"receivers,omitempty"`
	Processors []string `json:"processors,omitempty"`
	Exporters  []string `json:"exporters,omitempty"`
	Pipelines  []string `json:"pipelines,omitempty"`
	// Settings are other scalar facts worth showing, as key=value
	// ("kubernetes-attribution=true", "interval=30000ms",
	// "tracing.endpoint=set", "sampling-rate=10000").
	Settings []string `json:"settings,omitempty"`
}

// MetricsProbe is what one opt-in probe of a metrics endpoint returned.
type MetricsProbe struct {
	// Kind is the component kind the target belongs to.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Node      string `json:"node,omitempty"`
	Port      int32  `json:"port"`
	Path      string `json:"path"`
	// Families are the metric family names exposed; LabelKeys the label
	// names seen on any sample (no values).
	Families  []string `json:"families,omitempty"`
	LabelKeys []string `json:"label_keys,omitempty"`
	// Error is set when the probe failed (refused, timed out, not text).
	Error string `json:"error,omitempty"`
}

// ObjectRef names one object to read by name.
type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Why says which component needs it (for --help and the read log).
	Why string `json:"why"`
}

// ProbeTarget is one metrics endpoint an opt-in probe may read.
type ProbeTarget struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Node      string `json:"node,omitempty"`
	Port      int32  `json:"port"`
	Path      string `json:"path"`
}

// Signals lists every signal id in display order.
func Signals() []string {
	// TODO(contract stub): implemented by the pkg/mldiscover task.
	return nil
}

// ConfigRefs names the ConfigMaps whose content refines the inventory, given
// a first-phase snapshot (workloads, pods and custom objects read, no
// ConfigMaps yet). The collector gets exactly these, by name. Pure.
func ConfigRefs(s *Snapshot) []ObjectRef {
	// TODO(contract stub): implemented by the pkg/mldiscover task.
	return nil
}

// ProbeTargets names the metrics endpoints an opt-in probe may read: at most
// one running pod per (component, node pool), so a probe of a large cluster
// stays a handful of requests. Pure.
func ProbeTargets(s *Snapshot, inv *Inventory) []ProbeTarget {
	// TODO(contract stub): implemented by the pkg/mldiscover task.
	return nil
}
