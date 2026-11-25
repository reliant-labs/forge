package mldiscover

// Component kinds: the stable ids of forge.Adopt.kind / forge.Install.kind and
// Component.Kind. A kind is only ever added; renaming one breaks every ml.k
// and RecommendationDecision that names it.
const (
	KindGPUOperator            = "gpu-operator"
	KindNVIDIADevicePlugin     = "nvidia-device-plugin"
	KindDCGMExporter           = "dcgm-exporter"
	KindDCGMHostEngine         = "dcgm-hostengine"
	KindAMDGPUOperator         = "amd-gpu-operator"
	KindAMDMetricsExporter     = "amd-device-metrics-exporter"
	KindNeuronDevicePlugin     = "neuron-device-plugin"
	KindNeuronMonitor          = "neuron-monitor"
	KindGaudiExporter          = "gaudi-metric-exporter"
	KindDRA                    = "dra"
	KindNodeProblemDetector    = "node-problem-detector"
	KindEKSNodeMonitoring      = "eks-node-monitoring-agent"
	KindPrometheusOperator     = "prometheus-operator"
	KindGMP                    = "gmp"
	KindKubeStateMetrics       = "kube-state-metrics"
	KindOTelCollector          = "otel-collector"
	KindKueue                  = "kueue"
	KindVolcano                = "volcano"
	KindJobSet                 = "jobset"
	KindKubeflowTrainer        = "kubeflow-trainer"
	KindKubeflowTraining       = "kubeflow-training-operator"
	KindLWS                    = "lws"
	KindKubeRay                = "kuberay"
	KindKServe                 = "kserve"
	KindInferenceExtension     = "inference-extension"
	KindVLLM                   = "vllm"
	KindTriton                 = "triton"
	KindKEDA                   = "keda"
	KindKarpenter              = "karpenter"
	KindSpegel                 = "spegel"
	KindDragonfly              = "dragonfly"
	KindNodeTerminationHandler = "aws-node-termination-handler"
)

// Discover turns a snapshot into an inventory. It is pure: no I/O, no clock
// (CollectedAt comes from the snapshot), deterministic order.
func Discover(s *Snapshot) *Inventory {
	// TODO(contract stub): implemented by the pkg/mldiscover task.
	return &Inventory{Context: s.Context, ServerVersion: s.ServerVersion, CollectedAt: s.CollectedAt, Cloud: CloudOther,
		Accelerators: []AcceleratorGroup{}, Machines: []Machine{}, Components: []Component{}}
}
