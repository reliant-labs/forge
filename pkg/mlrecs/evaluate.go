package mlrecs

import (
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// rule is one catalogue entry and its evaluation.
type rule struct {
	Rule
	eval func(c *evalCtx) []Finding
}

// rules is the catalogue, in catalogue order (docs/proposals/ml-workloads.md
// §5 and Appendix A).
var rules = []rule{
	{Rule{ID: "accel-telemetry-missing", Severity: SeverityHigh,
		Detects: "Accelerator nodes no adopted or installed metrics source covers.",
		Fix:     "GPU Operator with dcgmExporter disabled: enable it in your ClusterPolicy; otherwise forge.Install NVIDIA's dcgm-exporter chart (other vendors: install their exporter)."}, ruleTelemetryMissing},
	{Rule{ID: "accel-exporter-duplicate", Severity: SeverityHigh, Vendor: mldiscover.VendorNVIDIA,
		Detects: "Two or more DCGM exporters or host engines from different installs on one node.",
		Fix:     "Remove the one discovery names; forge touches neither."}, ruleExporterDuplicate},
	{Rule{ID: "bad-accelerator", Severity: SeverityHigh,
		Detects: "A node's allocatable accelerators are fewer than its capacity: the device plugin marked a device unhealthy.",
		Fix:     "Cordon the node, then reset the device or RMA it."}, ruleBadAccelerator},
	{Rule{ID: "mig-config-failed", Severity: SeverityHigh, Vendor: mldiscover.VendorNVIDIA,
		Detects: "A node labelled nvidia.com/mig.config.state=failed.",
		Fix:     "Read the MIG manager's logs on the node and fix or relabel its MIG config."}, ruleMIGFailed},
	{Rule{ID: "driver-skew", Severity: SeverityMedium, Vendor: mldiscover.VendorNVIDIA,
		Detects: "NVIDIA driver versions (GFD nvidia.com/cuda.driver.*) differ across the nodes of one pool.",
		Fix:     "Align the pool's node image."}, ruleDriverSkew},
	{Rule{ID: "no-node-health", Severity: SeverityMedium,
		Detects: "Accelerator nodes and no node-problem-detector, no EKS node monitoring agent, and no non-core node conditions.",
		Fix:     "Turn on the cloud's node auto-repair; else forge.Install node-problem-detector with a GPU Xid log rule."}, ruleNoNodeHealth},
	{Rule{ID: "gpu-jobs-bypass-queue", Severity: SeverityMedium,
		Detects: "Unfinished Jobs request accelerators and neither Kueue nor Volcano runs.",
		Fix:     "forge.Install Kueue as the stack's queue (and JobSet for multi-pod training)."}, ruleJobsBypassQueue},
	{Rule{ID: "job-not-queued", Severity: SeverityMedium,
		Detects: "Kueue runs and an unfinished accelerator Job has no kueue.x-k8s.io/queue-name label.",
		Fix:     "Label the Job with its LocalQueue."}, ruleJobNotQueued},
	{Rule{ID: "spot-no-termination-handling", Severity: SeverityMedium,
		Detects: "Spot accelerator nodes on AWS with no aws-node-termination-handler, no Karpenter interruption queue and no EKS Auto Mode. (GKE and AKS deliver the notice through graceful node shutdown.)",
		Fix:     "Install aws-node-termination-handler, or set Karpenter's interruption queue."}, ruleSpotNoTermination},
	{Rule{ID: "shm-too-small", Severity: SeverityMedium,
		Detects: "An accelerator container mounts no memory-medium emptyDir at /dev/shm, so it has the 64 MiB default.",
		Fix:     "Mount emptyDir {medium: Memory} at /dev/shm."}, ruleShmTooSmall},
	{Rule{ID: "llm-scales-on-cpu", Severity: SeverityMedium,
		Detects: "A vLLM or Triton workload whose HPA scales only on CPU, memory or GPU utilisation, or that has no autoscaler.",
		Fix:     "Scale on queue depth: KEDA on vllm:num_requests_waiting or KV-cache usage."}, ruleLLMScalesOnCPU},
	{Rule{ID: "model-downloaded-per-pod", Severity: SeverityMedium,
		Detects: "An accelerator pod's init or storage-initializer container fetches s3://, gs:// or hf:// on every start.",
		Fix:     "Ship the weights by digest as an OCI artifact, or use KServe LocalModelCache."}, ruleModelDownloaded},
	{Rule{ID: "weights-in-image", Severity: SeverityMedium,
		Detects: "An accelerator pod's image is larger than 10 GB (node.status.images sizeBytes).",
		Fix:     "Split it into a weights artifact (ModelPack) and a slim runtime image."}, ruleWeightsInImage},
	{Rule{ID: "no-p2p-cache", Severity: SeverityMedium,
		Detects: "Four or more accelerator pods of one image across two or more nodes, and no Spegel or Dragonfly.",
		Fix:     "forge.Install Spegel as the stack's pull_cache."}, ruleNoP2PCache},
	{Rule{ID: "image-streaming-off", Severity: SeverityMedium,
		Detects: "GKE: an accelerator pod pulls from Artifact Registry onto pool nodes without the cloud.google.com/gke-image-streaming=true label.",
		Fix:     "Enable Image Streaming on the node pool."}, ruleImageStreamingOff},
	{Rule{ID: "accel-unallocated", Severity: SeverityLow,
		Detects: "Ready, schedulable nodes with allocatable accelerators and no pod requesting them, at snapshot time.",
		Fix:     "If it lasts, scale the pool down with its autoscaler."}, ruleUnallocated},
	{Rule{ID: "image-not-warm", Severity: SeverityLow,
		Detects: "An accelerator image present in node.status.images on some nodes of a pool and absent on others.",
		Fix:     "Pre-pull it (a pre-pull DaemonSet, kube-fledged) or bake it into the node image."}, ruleImageNotWarm},
	{Rule{ID: "hf-transfer-off", Severity: SeverityLow,
		Detects: "An accelerator pod downloads from Hugging Face (hf://, or a vLLM model id) with neither HF_XET_HIGH_PERFORMANCE nor HF_HUB_ENABLE_HF_TRANSFER set.",
		Fix:     "Set HF_XET_HIGH_PERFORMANCE=1, or stop downloading per pod."}, ruleHFTransferOff},
	{Rule{ID: "image-volumes-unavailable", Severity: SeverityInfo,
		Detects: "The API server is below Kubernetes 1.35 (ImageVolume on by default) or an accelerator node runs containerd below 2.1.",
		Fix:     "Upgrade; meanwhile mount weights with a modelcar sidecar or an init-container copy."}, ruleImageVolumes},
	{Rule{ID: "kubelet-tracing-off", Severity: SeverityInfo,
		Detects: "Self-managed accelerator nodes, where kubelet tracing may be off; discovery never reads the kubelet's config, so this is unverified.",
		Fix:     "Add `tracing` to the KubeletConfiguration and point it at your OTLP collector."}, ruleKubeletTracing},
}

// Rules lists every rule Evaluate runs, in catalogue order.
func Rules() []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = r.Rule
	}
	return out
}

// Evaluate runs every rule over a snapshot and its inventory. It is pure and
// deterministic: findings are sorted by severity, then ID, then evidence, and
// dismissed findings last.
func Evaluate(s *mldiscover.Snapshot, inv *mldiscover.Inventory, opts Options) []Finding {
	c := newEvalCtx(s, inv)
	out := []Finding{}
	for _, r := range rules {
		for _, f := range r.eval(c) {
			f.ID = r.ID
			if f.Severity == "" {
				f.Severity = r.Severity
			}
			if f.Vendor == "" {
				f.Vendor = r.Vendor
			}
			if f.Evidence == nil {
				f.Evidence = []string{}
			}
			out = append(out, f)
		}
	}
	now := opts.Now
	if now.IsZero() {
		now = s.CollectedAt
	}
	for i := range out {
		out[i].Dismissed = dismissal(out[i].ID, opts.Decisions, now)
	}
	for i := range out {
		if out[i].Fix.Kind == FixKCL && out[i].Dismissed == nil {
			out[i].Fix.KCLDiff = kclDiff(inv, out, opts.Propose, out[i].ID)
		}
	}
	sortFindings(out)
	return out
}

// dismissal returns the unexpired decision that dismisses id. A decision
// applies through its Expires day (UTC) and stops the day after; a
// malformed date never applies.
func dismissal(id string, decisions []Decision, now time.Time) *Dismissal {
	today := now.UTC().Format(dateLayout)
	for _, d := range decisions {
		if d.ID != id || (d.Decision != "" && d.Decision != "dismiss") {
			continue
		}
		if _, err := time.Parse(dateLayout, d.Expires); err != nil || today > d.Expires {
			continue
		}
		return &Dismissal{Reason: d.Reason, Expires: d.Expires}
	}
	return nil
}

const dateLayout = "2006-01-02"

var severityRank = map[string]int{SeverityHigh: 0, SeverityMedium: 1, SeverityLow: 2, SeverityInfo: 3}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if (a.Dismissed != nil) != (b.Dismissed != nil) {
			return b.Dismissed != nil
		}
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.Vendor != b.Vendor {
			return a.Vendor < b.Vendor
		}
		return strings.Join(a.Evidence, "\n") < strings.Join(b.Evidence, "\n")
	})
}

// evalCtx is the snapshot and inventory, indexed once for the rules.
type evalCtx struct {
	s         *mldiscover.Snapshot
	inv       *mldiscover.Inventory
	nodes     map[string]*corev1.Node
	accelPods []*corev1.Pod
}

func newEvalCtx(s *mldiscover.Snapshot, inv *mldiscover.Inventory) *evalCtx {
	c := &evalCtx{s: s, inv: inv, nodes: map[string]*corev1.Node{}, accelPods: mldiscover.AcceleratorPods(s)}
	for i := range s.Nodes {
		c.nodes[s.Nodes[i].Name] = &s.Nodes[i]
	}
	return c
}

// present returns the present components of kind.
func (c *evalCtx) present(kind string) []mldiscover.Component {
	var out []mldiscover.Component
	for _, comp := range c.inv.Components {
		if comp.Kind == kind && comp.Status == mldiscover.StatusPresent {
			out = append(out, comp)
		}
	}
	return out
}

// podsByOwner groups pods by their owning workload, in owner order.
func (c *evalCtx) podsByOwner(pods []*corev1.Pod) ([]mldiscover.WorkloadRef, map[mldiscover.WorkloadRef][]*corev1.Pod) {
	by := map[mldiscover.WorkloadRef][]*corev1.Pod{}
	var order []mldiscover.WorkloadRef
	for _, p := range pods {
		o := mldiscover.PodOwner(c.s, p)
		if _, ok := by[o]; !ok {
			order = append(order, o)
		}
		by[o] = append(by[o], p)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].String() < order[j].String() })
	return order, by
}

// accelJobs returns the unfinished Jobs whose template requests
// accelerators, sorted by namespace/name.
func (c *evalCtx) accelJobs() []int {
	var idx []int
	for i := range c.s.Jobs {
		j := &c.s.Jobs[i]
		if jobFinished(j.Status.Conditions) || len(mldiscover.PodSpecAccelerators(&j.Spec.Template.Spec)) == 0 {
			continue
		}
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool {
		ja, jb := &c.s.Jobs[idx[a]], &c.s.Jobs[idx[b]]
		return ja.Namespace+"/"+ja.Name < jb.Namespace+"/"+jb.Name
	})
	return idx
}
