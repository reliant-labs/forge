package mlrecs

import (
	"sort"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// Chart is an upstream helm chart a fix installs, pinned. Propose writes it
// as the forge.HelmChart of a forge.Install.
type Chart struct {
	// Kind is the component kind it installs (mldiscover.Kind*).
	Kind string `json:"kind"`
	// Name is the HelmChart's name: its `--target` selector.
	Name string `json:"name"`
	// Repo is a chart repository URL (Chart names the chart in it), or OCI
	// the full OCI reference. Exactly one is set.
	Repo  string `json:"repo,omitempty"`
	Chart string `json:"chart,omitempty"`
	OCI   string `json:"oci,omitempty"`
	// Version is the pinned chart version.
	Version string `json:"version"`
	// Delivery is the forge.HelmChart delivery: "bundle" (reconciled with
	// the env) or "bootstrap" (a chart that ships CRDs, applied with
	// `forge env deploy <env> --target <name>`).
	Delivery string `json:"delivery"`
	// Docs is the upstream documentation.
	Docs string `json:"docs"`
}

// charts is every chart a fix may install. Never KubeRay: Ray is adopt-only
// by policy.
//
// Verified 2026-11-25 against upstream, each the newest release then:
//
//	helm search repo deliveryhero/node-problem-detector   (https://charts.deliveryhero.io/)  2.3.14, app v0.8.19
//	helm search repo dcgm/dcgm-exporter                    (https://nvidia.github.io/dcgm-exporter/helm-charts)  4.9.0
//	helm show chart oci://registry.k8s.io/kueue/charts/kueue       0.20.7 (sha256:f39b8bed…)
//	helm show chart oci://registry.k8s.io/jobset/charts/jobset     0.13.1 (sha256:811c65a9…)
//	helm show chart oci://ghcr.io/spegel-org/helm-charts/spegel    0.8.2  (sha256:2449e91c…)
//	helm show chart oci://ghcr.io/kubeflow/charts/kubeflow-trainer 2.4.0  (sha256:2234be43…)
//
// Moving a pin is a deliberate edit here; TestChartsArePinned fails a chart
// with no version.
var charts = []Chart{
	{Kind: mldiscover.KindNodeProblemDetector, Name: "node-problem-detector", Delivery: "bundle",
		Repo: "https://charts.deliveryhero.io/", Chart: "node-problem-detector", Version: "2.3.14",
		Docs: "https://github.com/kubernetes/node-problem-detector"},
	{Kind: mldiscover.KindDCGMExporter, Name: "dcgm-exporter", Delivery: "bundle",
		Repo: "https://nvidia.github.io/dcgm-exporter/helm-charts", Chart: "dcgm-exporter", Version: "4.9.0",
		Docs: "https://github.com/NVIDIA/dcgm-exporter"},
	{Kind: mldiscover.KindKueue, Name: "kueue", Delivery: "bootstrap",
		OCI: "oci://registry.k8s.io/kueue/charts/kueue", Version: "0.20.7",
		Docs: "https://kueue.sigs.k8s.io/docs/overview/"},
	{Kind: mldiscover.KindJobSet, Name: "jobset", Delivery: "bootstrap",
		OCI: "oci://registry.k8s.io/jobset/charts/jobset", Version: "0.13.1",
		Docs: "https://jobset.sigs.k8s.io/"},
	{Kind: mldiscover.KindSpegel, Name: "spegel", Delivery: "bundle",
		OCI: "oci://ghcr.io/spegel-org/helm-charts/spegel", Version: "0.8.2",
		Docs: "https://spegel.dev/docs/getting-started/"},
	{Kind: mldiscover.KindKubeflowTrainer, Name: "kubeflow-trainer", Delivery: "bootstrap",
		OCI: "oci://ghcr.io/kubeflow/charts/kubeflow-trainer", Version: "2.4.0",
		Docs: "https://www.kubeflow.org/docs/components/trainer/"},
}

// Charts returns the pinned chart table, sorted by kind.
func Charts() []Chart {
	out := append([]Chart(nil), charts...)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Delivery values.
const (
	DeliveryBundle    = "bundle"
	DeliveryBootstrap = "bootstrap"
)

// ChartFor returns the pinned chart that installs kind.
func ChartFor(kind string) (Chart, bool) {
	for _, c := range charts {
		if c.Kind == kind {
			return c, true
		}
	}
	return Chart{}, false
}

// gpuXidMonitor is the node-problem-detector kernel-log monitor the NPD fix
// adds: it sets the GPUProblem condition on an NVIDIA Xid that means the
// GPU needs a reset or RMA (48 DBE, 64 row-remap failure, 74 NVLink, 79
// fallen off the bus, 94/95 contained/uncontained ECC, 119/120 GSP, 140
// unrecovered ECC). Character classes stand in for escaped parentheses so
// the JSON survives a KCL string unescaped.
const gpuXidMonitor = `{
  "plugin": "kmsg",
  "logPath": "/dev/kmsg",
  "lookback": "5m",
  "bufferSize": 10,
  "source": "gpu-xid-monitor",
  "conditions": [
    {"type": "GPUProblem", "reason": "NoGPUXid", "message": "no fatal NVIDIA Xid in the kernel log"}
  ],
  "rules": [
    {"type": "permanent", "condition": "GPUProblem", "reason": "NVIDIAXid", "pattern": "NVRM: Xid [(][^)]*[)]: (48|64|74|79|94|95|119|120|140),.*"}
  ]
}
`

// chartValues are the helm values each install writes, as data in the
// user's ml.k (theirs to edit). Values that depend on the cluster are added
// in Propose.
func chartValues(kind string) map[string]any {
	switch kind {
	case mldiscover.KindNodeProblemDetector:
		return map[string]any{
			"settings": map[string]any{
				"custom_monitor_definitions": map[string]any{"gpu-xid-monitor.json": gpuXidMonitor},
				"log_monitors": []any{
					"/config/kernel-monitor.json",
					"/config/docker-monitor.json",
					"/custom-config/gpu-xid-monitor.json",
				},
			},
		}
	case mldiscover.KindDCGMExporter:
		return map[string]any{
			// forge scrapes the exporter itself; the chart's ServiceMonitor
			// needs the Prometheus Operator's CRDs.
			"serviceMonitor": map[string]any{"enabled": false},
			// GPU nodes are usually tainted (GKE taints nvidia.com/gpu).
			"tolerations": []any{map[string]any{"operator": "Exists"}},
		}
	}
	return nil
}
