package mlrecs

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// Thresholds the rules use.
const (
	// WeightsInImageBytes is the image size above which weights-in-image
	// fires (10 GB, decimal, as registries and the kubelet report it).
	WeightsInImageBytes = 10_000_000_000
	// P2PFanOut is how many accelerator pods of one image, across two or
	// more nodes, make no-p2p-cache fire.
	P2PFanOut = 4
	// ImageVolumeDefaultOnMinor is the Kubernetes minor from which the
	// ImageVolume feature gate is on by default (beta on in 1.35, GA in
	// 1.36; kubernetes/website feature-gates/ImageVolume.md, checked
	// 2026-11-25).
	ImageVolumeDefaultOnMinor = 35
	// maxEvidence caps a finding's evidence lines.
	maxEvidence = 20
)

// Well-known labels and env vars.
const (
	labelKueueQueue      = "kueue.x-k8s.io/queue-name"
	labelKnativeRevision = "serving.knative.dev/revision"
	envXetHighPerf       = "HF_XET_HIGH_PERFORMANCE"
	envHFTransfer        = "HF_HUB_ENABLE_HF_TRANSFER"
	envHFOffline         = "HF_HUB_OFFLINE"
)

func capEvidence(ev []string) []string {
	if len(ev) <= maxEvidence {
		return ev
	}
	return append(ev[:maxEvidence:maxEvidence], fmt.Sprintf("… and %d more", len(ev)-maxEvidence))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// pv is a counted subject and its verb: "1 node runs", "2 nodes run".
func pv(n int, one, many, verbOne, verbMany string) string {
	if n == 1 {
		return plural(n, one, many) + " " + verbOne
	}
	return plural(n, one, many) + " " + verbMany
}

func jobFinished(conds []batchv1.JobCondition) bool {
	for _, c := range conds {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func describeAccel(req map[string]int64) string {
	keys := make([]string, 0, len(req))
	for k := range req {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d × %s", req[k], k)
	}
	return strings.Join(parts, ", ")
}

func componentRef(comp mldiscover.Component) string {
	switch {
	case comp.Workload != "":
		kind, name, _ := strings.Cut(comp.Workload, "/")
		return kind + "/" + comp.Namespace + "/" + name
	case comp.Via != "":
		return comp.Kind + " (via " + comp.Via + ")"
	}
	return comp.Kind
}

// --- accel-telemetry-missing -------------------------------------------

// metricsSources are the components that serve a vendor's utilisation (a
// DCGM host engine alone is not scraped).
func (c *evalCtx) metricsSources(vendor string) []mldiscover.Component {
	var out []mldiscover.Component
	for _, comp := range c.inv.Components {
		if comp.Category == mldiscover.CategoryAccelMetrics && comp.Status == mldiscover.StatusPresent &&
			comp.Vendor == vendor && comp.Kind != mldiscover.KindDCGMHostEngine {
			out = append(out, comp)
		}
	}
	return out
}

func ruleTelemetryMissing(c *evalCtx) []Finding {
	byVendor := map[string][]mldiscover.Machine{}
	var vendors []string
	for _, m := range c.inv.Machines {
		// TPU utilisation is in Cloud Monitoring, not an in-cluster
		// exporter: nothing in the cluster can be missing.
		if m.Vendor == mldiscover.VendorTPU {
			continue
		}
		if _, ok := byVendor[m.Vendor]; !ok {
			vendors = append(vendors, m.Vendor)
		}
		byVendor[m.Vendor] = append(byVendor[m.Vendor], m)
	}
	sort.Slice(vendors, func(i, j int) bool { return mldiscover.VendorRank(vendors[i]) < mldiscover.VendorRank(vendors[j]) })
	var out []Finding
	for _, v := range vendors {
		sources := c.metricsSources(v)
		covered := map[string]bool{}
		for _, src := range sources {
			for _, n := range mldiscover.NodeCoverage(c.s, src) {
				covered[n] = true
			}
		}
		var ev []string
		resources := map[string]bool{}
		for _, m := range byVendor[v] {
			if covered[m.Name] {
				continue
			}
			resources[m.Resource] = true
			product := ""
			if m.Product != "" {
				product = " (" + m.Product + ")"
			}
			ev = append(ev, fmt.Sprintf("Node/%s: %d × %s%s, no metrics source runs on it", m.Name, m.Capacity, m.Resource, product))
		}
		if len(ev) == 0 {
			continue
		}
		res := strings.Join(sortedSet(resources), ", ")
		f := Finding{
			Vendor:     v,
			Confidence: ConfidenceCertain,
			Title:      fmt.Sprintf("%s with %s %s no utilisation source", plural(len(ev), "node", "nodes"), res, hasHave(len(ev))),
			Evidence:   capEvidence(ev),
		}
		switch {
		case v == mldiscover.VendorNVIDIA && operatorExporterDisabled(c):
			f.Evidence = append(f.Evidence, "ClusterPolicy: spec.dcgmExporter.enabled=false")
			f.Fix = Fix{Kind: FixManual,
				Summary: "Your GPU Operator runs with its DCGM exporter disabled: set spec.dcgmExporter.enabled=true in your ClusterPolicy (helm value dcgmExporter.enabled=true), then re-run `forge ml discover` to adopt it. forge never changes an operator it did not install.",
				Docs:    "https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html"}
		case len(sources) > 0:
			names := make([]string, len(sources))
			for i, src := range sources {
				names[i] = componentRef(src)
			}
			f.Evidence = append(f.Evidence, "metrics source covering other nodes: "+strings.Join(names, ", "))
			f.Fix = Fix{Kind: FixManual,
				Summary: "Extend the node selector and tolerations of your existing exporter (" + strings.Join(names, ", ") + ") to these nodes. A second exporter beside it would break DCGM's profiling counters, so forge does not install one."}
		case v == mldiscover.VendorNVIDIA:
			summary := "Install NVIDIA's dcgm-exporter chart as the stack's NVIDIA metrics source (forge.Install, pinned); forge scrapes it with the allow list in ml.k."
			if c.inv.Cloud == mldiscover.CloudGKE {
				summary = "On GKE, first consider GKE's managed DCGM metrics (`gcloud container clusters update <cluster> --monitoring=SYSTEM,DCGM`), which discovery then adopts; otherwise " +
					"install NVIDIA's dcgm-exporter chart as the stack's NVIDIA metrics source (forge.Install, pinned)."
			}
			f.Fix = Fix{Kind: FixKCL, Summary: summary, Docs: "https://github.com/NVIDIA/dcgm-exporter"}
		default:
			f.Severity = SeverityMedium
			f.Fix = vendorExporterFix(v)
		}
		out = append(out, f)
	}
	return out
}

func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func operatorExporterDisabled(c *evalCtx) bool {
	if len(c.present(mldiscover.KindGPUOperator)) == 0 {
		return false
	}
	for _, p := range mldiscover.ClusterPolicies(c.s) {
		if !p.DCGMExporter {
			return true
		}
	}
	return false
}

func vendorExporterFix(vendor string) Fix {
	switch vendor {
	case mldiscover.VendorAMD:
		return Fix{Kind: FixManual, Summary: "Install AMD's device-metrics-exporter (enable it in the AMD GPU Operator's DeviceConfig, or its helm chart), then re-run `forge ml discover` to adopt it.",
			Docs: "https://github.com/ROCm/device-metrics-exporter"}
	case mldiscover.VendorNeuron:
		return Fix{Kind: FixManual, Summary: "Run neuron-monitor with its Prometheus exporter as a DaemonSet on the Neuron nodes, then re-run `forge ml discover` to adopt it.",
			Docs: "https://awsdocs-neuron.readthedocs-hosted.com/en/latest/tools/neuron-sys-tools/neuron-monitor-user-guide.html"}
	case mldiscover.VendorGaudi:
		return Fix{Kind: FixManual, Summary: "Install the Intel Gaudi Prometheus metric exporter as a DaemonSet on the Gaudi nodes, then re-run `forge ml discover` to adopt it."}
	}
	return Fix{Kind: FixManual, Summary: "forge knows no exporter for this resource, so it shows allocation only. Install the device vendor's exporter and adopt it in ml.k with forge.Adopt (selector, port, allow)."}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- accel-exporter-duplicate ------------------------------------------

// exporterSource names the install a DCGM client belongs to: the operator
// or GKE when one manages it, else its namespace (an exporter beside its
// own host engine is one install).
func exporterSource(comp mldiscover.Component) string {
	if comp.Via != "" {
		return "via:" + comp.Via
	}
	return "ns:" + comp.Namespace
}

func sourceRank(comp mldiscover.Component) int {
	switch {
	case comp.Via != "":
		return 0
	case comp.Owner == mldiscover.OwnerTheirs:
		return 1
	}
	return 2 // forge's own install yields to theirs
}

func ruleExporterDuplicate(c *evalCtx) []Finding {
	var comps []mldiscover.Component
	for _, comp := range c.inv.Components {
		if comp.Status == mldiscover.StatusPresent && (comp.Kind == mldiscover.KindDCGMExporter || comp.Kind == mldiscover.KindDCGMHostEngine) {
			comps = append(comps, comp)
		}
	}
	if len(comps) < 2 {
		return nil
	}
	// node → source → components
	nodes := map[string]map[string][]mldiscover.Component{}
	for _, comp := range comps {
		for _, n := range mldiscover.NodeCoverage(c.s, comp) {
			if nodes[n] == nil {
				nodes[n] = map[string][]mldiscover.Component{}
			}
			src := exporterSource(comp)
			nodes[n][src] = append(nodes[n][src], comp)
		}
	}
	// One finding per distinct set of colliding installs.
	type group struct {
		sources []string
		comps   []mldiscover.Component
		nodes   []string
	}
	groups := map[string]*group{}
	for _, n := range sortedKeys(nodes) {
		srcs := nodes[n]
		if len(srcs) < 2 {
			continue
		}
		keys := sortedKeys(srcs)
		k := strings.Join(keys, "|")
		g, ok := groups[k]
		if !ok {
			g = &group{sources: keys}
			for _, src := range keys {
				g.comps = append(g.comps, srcs[src]...)
			}
			groups[k] = g
		}
		g.nodes = append(g.nodes, n)
	}
	var out []Finding
	for _, k := range sortedKeys(groups) {
		g := groups[k]
		sort.SliceStable(g.comps, func(i, j int) bool {
			if sourceRank(g.comps[i]) != sourceRank(g.comps[j]) {
				return sourceRank(g.comps[i]) < sourceRank(g.comps[j])
			}
			return componentRef(g.comps[i]) < componentRef(g.comps[j])
		})
		var ev []string
		for _, comp := range g.comps {
			desc := componentRef(comp)
			if comp.Version != "" {
				desc += " " + comp.Version
			}
			if comp.Owner == mldiscover.OwnerForge {
				desc += " (installed by forge)"
			}
			ev = append(ev, fmt.Sprintf("%s: %s", comp.Kind, desc))
		}
		ev = append(ev, "on nodes: "+strings.Join(g.nodes, ", "))
		keep := g.comps[0]
		var remove []string
		for _, comp := range g.comps[1:] {
			if exporterSource(comp) != exporterSource(keep) {
				remove = append(remove, componentRef(comp))
			}
		}
		summary := fmt.Sprintf("DCGM's profiling counters allow one client per GPU, so these break each other. Keep %s and remove %s. forge touches neither.",
			componentRef(keep), strings.Join(remove, ", "))
		if keep.Via != "" && len(g.comps) > 1 && g.comps[1].Via != "" && g.comps[1].Via != keep.Via {
			summary = "DCGM's profiling counters allow one client per GPU, so these break each other. Disable one: set spec.dcgmExporter.enabled=false in your ClusterPolicy, or turn off GKE's managed DCGM metrics. forge touches neither."
		}
		out = append(out, Finding{
			Confidence: ConfidenceCertain,
			Title:      fmt.Sprintf("%s more than one DCGM exporter", pv(len(g.nodes), "node", "nodes", "runs", "run")),
			Evidence:   capEvidence(ev),
			Fix:        Fix{Kind: FixManual, Summary: summary, Docs: "https://github.com/NVIDIA/dcgm-exporter"},
		})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- bad-accelerator ---------------------------------------------------

func ruleBadAccelerator(c *evalCtx) []Finding {
	byVendor := map[string][]string{}
	nodes := map[string][]string{}
	for _, m := range c.inv.Machines {
		if m.DetectedBy != mldiscover.DetectedByResource || m.Allocatable >= m.Capacity {
			continue
		}
		byVendor[m.Vendor] = append(byVendor[m.Vendor], fmt.Sprintf("Node/%s: allocatable %d of capacity %d %s (%d withheld by the device plugin)",
			m.Name, m.Allocatable, m.Capacity, m.Resource, m.Capacity-m.Allocatable))
		nodes[m.Vendor] = append(nodes[m.Vendor], m.Name)
	}
	var out []Finding
	for _, v := range sortedKeys(byVendor) {
		out = append(out, Finding{
			Vendor:     v,
			Confidence: ConfidenceLikely,
			Title:      fmt.Sprintf("%s unhealthy accelerators", pv(len(nodes[v]), "node", "nodes", "reports", "report")),
			Evidence:   capEvidence(byVendor[v]),
			Fix: Fix{Kind: FixManual,
				Summary: fmt.Sprintf("The device plugin stopped offering a device it reports unhealthy. Cordon the node (`kubectl cordon %s`), drain it when its work allows, then reset the device or open an RMA with the vendor.", strings.Join(nodes[v], " "))},
		})
	}
	return out
}

// --- mig-config-failed -------------------------------------------------

func ruleMIGFailed(c *evalCtx) []Finding {
	var ev, names []string
	for _, n := range sortedNodeList(c) {
		if n.Labels[mldiscover.LabelMIGConfigState] != "failed" {
			continue
		}
		names = append(names, n.Name)
		ev = append(ev, fmt.Sprintf("Node/%s: %s=failed (desired %s=%q)", n.Name, mldiscover.LabelMIGConfigState, mldiscover.LabelMIGConfig, n.Labels[mldiscover.LabelMIGConfig]))
	}
	if len(ev) == 0 {
		return nil
	}
	ns := "gpu-operator"
	if ops := c.present(mldiscover.KindGPUOperator); len(ops) > 0 && ops[0].Namespace != "" {
		ns = ops[0].Namespace
	}
	return []Finding{{
		Confidence: ConfidenceCertain,
		Title:      fmt.Sprintf("MIG partitioning failed on %s", plural(len(names), "node", "nodes")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: fmt.Sprintf("Read the MIG manager's log for the node (`kubectl -n %s logs -l app=nvidia-mig-manager --field-selector spec.nodeName=<node>` on the pod there) and its events; fix the profile in your mig-parted config, or set %s back to a config the GPUs support.", ns, mldiscover.LabelMIGConfig),
			Docs:    "https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-mig.html"},
	}}
}

func sortedNodeList(c *evalCtx) []*corev1.Node {
	out := make([]*corev1.Node, 0, len(c.s.Nodes))
	for i := range c.s.Nodes {
		out = append(out, &c.s.Nodes[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- driver-skew -------------------------------------------------------

func ruleDriverSkew(c *evalCtx) []Finding {
	pools := map[string]map[string][]string{}
	for _, m := range c.inv.Machines {
		if m.Vendor != mldiscover.VendorNVIDIA || m.Pool == "" || m.DriverVersion == "" {
			continue
		}
		if pools[m.Pool] == nil {
			pools[m.Pool] = map[string][]string{}
		}
		pools[m.Pool][m.DriverVersion] = append(pools[m.Pool][m.DriverVersion], m.Name)
	}
	var out []Finding
	for _, pool := range sortedKeys(pools) {
		versions := pools[pool]
		if len(versions) < 2 {
			continue
		}
		var ev []string
		for _, v := range sortedKeys(versions) {
			ev = append(ev, fmt.Sprintf("driver %s on %s", v, strings.Join(versions[v], ", ")))
		}
		out = append(out, Finding{
			Confidence: ConfidenceCertain,
			Title:      fmt.Sprintf("Pool %s runs %d NVIDIA driver versions", pool, len(versions)),
			Evidence:   ev,
			Fix: Fix{Kind: FixManual,
				Summary: fmt.Sprintf("Align pool %s's node image so every node runs one driver: upgrade or recreate the older nodes (GKE: the pool's gpu-driver-version; EKS: the AMI release; self-managed: the driver install). Mixed drivers make NCCL and CUDA behaviour differ by node.", pool)},
		})
	}
	return out
}

// --- no-node-health ----------------------------------------------------

func ruleNoNodeHealth(c *evalCtx) []Finding {
	if len(c.inv.Machines) == 0 || len(c.present(mldiscover.KindNodeProblemDetector)) > 0 || len(c.present(mldiscover.KindEKSNodeMonitoring)) > 0 {
		return nil
	}
	// Only conditions a health source sets count (a controller's own
	// condition, such as fake-gpu-operator's gpu-fractioning.nvidia.com/Ready,
	// does not): the inventory's node.health row decides.
	if r, ok := c.coverage(mldiscover.SignalNodeHealth); ok && r.Status != mldiscover.CoverageMissing {
		return nil
	}
	first := ""
	switch c.inv.Cloud {
	case mldiscover.CloudGKE:
		first = "First option: keep GKE node auto-repair on for the accelerator pools (it repairs nodes whose GPU device or driver fails). "
	case mldiscover.CloudEKS:
		first = "First option: the EKS node monitoring agent add-on with node auto repair, which sets AcceleratedHardwareReady and replaces bad GPU nodes. "
	case mldiscover.CloudAKS:
		first = "First option: AKS node auto-repair (it acts on NotReady nodes). "
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s no node health detection: bad nodes keep getting work", pv(len(c.inv.Machines), "accelerator node", "accelerator nodes", "has", "have")),
		Evidence:   []string{"no node-problem-detector, no EKS node monitoring agent, and no node condition a health source sets"},
		Fix: Fix{Kind: FixKCL,
			Summary: first + "Otherwise install node-problem-detector as the stack's node_health, with a kernel-log rule that sets GPUProblem on a fatal NVIDIA Xid.",
			Docs:    "https://github.com/kubernetes/node-problem-detector"},
	}}
}

// --- gpu-jobs-bypass-queue / job-not-queued ----------------------------

func ruleJobsBypassQueue(c *evalCtx) []Finding {
	if len(c.present(mldiscover.KindKueue)) > 0 || len(c.present(mldiscover.KindVolcano)) > 0 {
		return nil
	}
	var ev []string
	for _, i := range c.accelJobs() {
		j := &c.s.Jobs[i]
		ev = append(ev, fmt.Sprintf("Job/%s/%s: %s per pod", j.Namespace, j.Name, describeAccel(mldiscover.PodSpecAccelerators(&j.Spec.Template.Spec))))
	}
	if len(ev) == 0 {
		return nil
	}
	if len(c.inv.Find(mldiscover.KindKueue)) > 0 {
		ev = append(ev, "kueue: CRDs installed but no controller runs")
	}
	summary := "Install Kueue as the stack's queue (forge.Install, pinned; delivery \"bootstrap\" because it ships CRDs, so apply it with `forge env deploy <env> --target kueue`), then give each team a LocalQueue and label jobs with kueue.x-k8s.io/queue-name."
	if len(c.present(mldiscover.KindJobSet)) == 0 {
		summary += " The diff also installs JobSet (`--target jobset`), Kueue's unit for multi-pod training."
	}
	return []Finding{{
		Confidence: ConfidenceCertain,
		Title:      fmt.Sprintf("%s accelerators with no queue: no fair share or preemption", pv(len(ev), "Job", "Jobs", "requests", "request")),
		Evidence:   capEvidence(ev),
		Fix:        Fix{Kind: FixKCL, Summary: summary, Docs: "https://kueue.sigs.k8s.io/docs/overview/"},
	}}
}

func ruleJobNotQueued(c *evalCtx) []Finding {
	if len(c.present(mldiscover.KindKueue)) == 0 {
		return nil
	}
	var ev []string
	for _, i := range c.accelJobs() {
		j := &c.s.Jobs[i]
		if j.Labels[labelKueueQueue] != "" {
			continue
		}
		ev = append(ev, fmt.Sprintf("Job/%s/%s: %s per pod, no %s label", j.Namespace, j.Name, describeAccel(mldiscover.PodSpecAccelerators(&j.Spec.Template.Spec)), labelKueueQueue))
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s Kueue's quota", pv(len(ev), "accelerator Job", "accelerator Jobs", "skips", "skip")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Add the label kueue.x-k8s.io/queue-name: <LocalQueue> to each Job. (Unless Kueue's manageJobsWithoutQueueName is on, which discovery cannot read.)",
			Docs:    "https://kueue.sigs.k8s.io/docs/overview/"},
	}}
}

// --- spot-no-termination-handling --------------------------------------

func ruleSpotNoTermination(c *evalCtx) []Finding {
	if c.inv.Cloud != mldiscover.CloudEKS && c.inv.Provider != mldiscover.ProviderAWS {
		return nil
	}
	if len(c.present(mldiscover.KindNodeTerminationHandler)) > 0 || karpenterInterruption(c) {
		return nil
	}
	var ev []string
	for _, m := range c.inv.Machines {
		if !m.Spot {
			continue
		}
		if n := c.nodes[m.Name]; n != nil && n.Labels[mldiscover.LabelEKSComputeType] == "auto" {
			return nil // EKS Auto Mode handles interruptions itself
		}
		ev = append(ev, fmt.Sprintf("Node/%s: spot, %d × %s", m.Name, m.Capacity, m.Resource))
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s no graceful interruption notice", pv(len(ev), "spot accelerator node", "spot accelerator nodes", "gets", "get")),
		Cloud:      mldiscover.CloudEKS,
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Install aws-node-termination-handler in queue mode (its helm chart, oci://public.ecr.aws/aws-ec2/helm/aws-node-termination-handler), or, if Karpenter provisions these nodes, set its interruption queue (settings.interruptionQueue), so a two-minute notice drains pods and lets training checkpoint.",
			Docs:    "https://github.com/aws/aws-node-termination-handler"},
	}}
}

// karpenterInterruption reports whether a Karpenter controller is
// configured with an interruption queue (env INTERRUPTION_QUEUE, or the
// --interruption-queue flag).
func karpenterInterruption(c *evalCtx) bool {
	for _, comp := range c.present(mldiscover.KindKarpenter) {
		_, name, _ := strings.Cut(comp.Workload, "/")
		for i := range c.s.Deployments {
			d := &c.s.Deployments[i]
			if d.Namespace != comp.Namespace || d.Name != name {
				continue
			}
			for _, ctr := range d.Spec.Template.Spec.Containers {
				for _, e := range ctr.Env {
					if e.Name == "INTERRUPTION_QUEUE" || e.Name == "INTERRUPTION_QUEUE_NAME" {
						return true
					}
				}
				for _, a := range ctr.Args {
					if strings.HasPrefix(a, "--interruption-queue") {
						return true
					}
				}
			}
		}
	}
	return false
}

// --- shm-too-small -----------------------------------------------------

func hasMemoryShm(spec *corev1.PodSpec, ctr *corev1.Container) bool {
	for _, vm := range ctr.VolumeMounts {
		if strings.TrimSuffix(vm.MountPath, "/") != "/dev/shm" {
			continue
		}
		for _, v := range spec.Volumes {
			if v.Name == vm.Name && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
				return true
			}
		}
	}
	return false
}

func ruleShmTooSmall(c *evalCtx) []Finding {
	order, by := c.podsByOwner(c.accelPods)
	var ev []string
	for _, o := range order {
		p := by[o][0]
		var bad []string
		for i := range p.Spec.Containers {
			ctr := &p.Spec.Containers[i]
			if len(mldiscover.PodSpecAccelerators(&corev1.PodSpec{Containers: []corev1.Container{*ctr}})) == 0 {
				continue
			}
			if !hasMemoryShm(&p.Spec, ctr) {
				bad = append(bad, ctr.Name)
			}
		}
		if len(bad) > 0 {
			ev = append(ev, fmt.Sprintf("%s: %s, container %s mounts no memory emptyDir at /dev/shm", o, plural(len(by[o]), "pod", "pods"), strings.Join(bad, ", ")))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceCertain,
		Title:      fmt.Sprintf("%s accelerators with the 64 MiB /dev/shm default", pv(len(ev), "workload", "workloads", "runs", "run")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Mount an emptyDir {medium: Memory} (with a sizeLimit) at /dev/shm in each accelerator container: NCCL, PyTorch DataLoader workers and vLLM's tensor parallelism need more than 64 MiB."},
	}}
}

// --- llm-scales-on-cpu -------------------------------------------------

func ruleLLMScalesOnCPU(c *evalCtx) []Finding {
	var ev []string
	for _, kind := range []string{mldiscover.KindVLLM, mldiscover.KindTriton} {
		for _, comp := range c.present(kind) {
			wk, name, _ := strings.Cut(comp.Workload, "/")
			if wk != mldiscover.WorkloadDeployment && wk != mldiscover.WorkloadStatefulSet {
				continue
			}
			ref := wk + "/" + comp.Namespace + "/" + name
			hpas := c.hpasFor(wk, comp.Namespace, name)
			if len(hpas) == 0 {
				if c.knativeManaged(wk, comp.Namespace, name) {
					continue // KServe serverless: the Knative autoscaler scales it on concurrency
				}
				ev = append(ev, fmt.Sprintf("%s (%s): no HorizontalPodAutoscaler or KEDA ScaledObject", ref, kind))
				continue
			}
			for _, h := range hpas {
				if sig, wrong := wrongSignal(h); wrong {
					ev = append(ev, fmt.Sprintf("%s (%s): HorizontalPodAutoscaler/%s/%s scales on %s", ref, kind, h.Namespace, h.Name, sig))
				}
			}
		}
	}
	if len(ev) == 0 {
		return nil
	}
	sort.Strings(ev)
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s on the wrong signal", pv(len(ev), "inference server", "inference servers", "scales", "scale")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Scale on queue depth, not CPU: a KEDA ScaledObject with a Prometheus trigger on vllm:num_requests_waiting (or KV-cache usage; Triton: nv_inference_queue_duration_us).",
			Docs:    "https://keda.sh/docs/latest/scalers/prometheus/"},
	}}
}

func (c *evalCtx) hpasFor(kind, ns, name string) []autoscalingv2.HorizontalPodAutoscaler {
	var out []autoscalingv2.HorizontalPodAutoscaler
	for _, h := range c.s.HPAs {
		if h.Namespace == ns && h.Spec.ScaleTargetRef.Kind == kind && h.Spec.ScaleTargetRef.Name == name {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *evalCtx) knativeManaged(kind, ns, name string) bool {
	if kind != mldiscover.WorkloadDeployment {
		return false
	}
	for i := range c.s.Deployments {
		d := &c.s.Deployments[i]
		if d.Namespace == ns && d.Name == name {
			return d.Spec.Template.Labels[labelKnativeRevision] != "" || d.Labels[labelKnativeRevision] != ""
		}
	}
	return false
}

// wrongSignal reports whether an HPA scales an LLM server only on resource
// or GPU-utilisation metrics, and names them.
func wrongSignal(h autoscalingv2.HorizontalPodAutoscaler) (string, bool) {
	if len(h.Spec.Metrics) == 0 {
		return "cpu (the HPA default)", true
	}
	var sigs []string
	for _, m := range h.Spec.Metrics {
		switch m.Type {
		case autoscalingv2.ResourceMetricSourceType:
			if m.Resource != nil {
				sigs = append(sigs, string(m.Resource.Name))
			}
		case autoscalingv2.ContainerResourceMetricSourceType:
			if m.ContainerResource != nil {
				sigs = append(sigs, string(m.ContainerResource.Name))
			}
		default:
			name := metricName(m)
			if !strings.Contains(strings.ToUpper(name), "GPU_UTIL") {
				return "", false
			}
			sigs = append(sigs, name)
		}
	}
	return strings.Join(sigs, ", "), true
}

func metricName(m autoscalingv2.MetricSpec) string {
	switch {
	case m.Pods != nil:
		return m.Pods.Metric.Name
	case m.Object != nil:
		return m.Object.Metric.Name
	case m.External != nil:
		return m.External.Metric.Name
	}
	return ""
}

// --- model-downloaded-per-pod / hf-transfer-off ------------------------

var remoteModelRE = regexp.MustCompile(`\b(s3|gs|hf)://[^\s"',]+`)

// containerStrings is a container's command, args and env names: what the
// collector keeps (it blanks env values).
func containerStrings(ctr *corev1.Container) []string {
	out := append(append([]string{}, ctr.Command...), ctr.Args...)
	for _, e := range ctr.Env {
		if e.Value != "" {
			out = append(out, e.Value)
		}
	}
	return out
}

func ruleModelDownloaded(c *evalCtx) []Finding {
	order, by := c.podsByOwner(c.accelPods)
	var ev []string
	for _, o := range order {
		p := by[o][0]
		fetchers := map[string]bool{}
		for i := range p.Spec.InitContainers {
			fetchers[p.Spec.InitContainers[i].Name] = true
		}
		for _, ctr := range mldiscover.Containers(&p.Spec) {
			if !fetchers[ctr.Name] && ctr.Name != "storage-initializer" {
				continue
			}
			for _, s := range containerStrings(ctr) {
				if u := remoteModelRE.FindString(s); u != "" {
					ev = append(ev, fmt.Sprintf("%s: %s fetches %s on every start", o, ctr.Name, u))
					break
				}
			}
		}
	}
	if len(ev) == 0 {
		return nil
	}
	conf := ConfidenceCertain
	if c.s.CustomResources["localmodelcaches.serving.kserve.io"] > 0 {
		conf = ConfidenceLikely
		ev = append(ev, "KServe LocalModelCache objects exist: check whether they cover these models")
	}
	return []Finding{{
		Confidence: conf,
		Title:      fmt.Sprintf("%s its model on every pod start", pv(len(ev), "workload", "workloads", "downloads", "download")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Ship the weights by digest as an OCI artifact the node pulls once (an image volume or a KServe modelcar), or cache them on the nodes with KServe LocalModelCache.",
			Docs:    "https://kserve.github.io/website/latest/modelserving/storage/modelcache/localmodel/"},
	}}
}

var hfRepoIDRE = regexp.MustCompile(`^[A-Za-z0-9][\w.-]*/[\w.-]+$`)

// hfDownload returns what a container downloads from the Hugging Face Hub:
// an hf:// URL, or the model id of `vllm serve <id>` / `--model <id>`.
func hfDownload(ctr *corev1.Container) string {
	strs := containerStrings(ctr)
	for _, s := range strs {
		if i := strings.Index(s, "hf://"); i >= 0 {
			return remoteModelRE.FindString(s[i:])
		}
	}
	args := append(append([]string{}, ctr.Command...), ctr.Args...)
	vllm := strings.Contains(strings.Join(args, " "), "vllm") || strings.Contains(strings.ToLower(ctr.Image), "vllm")
	if !vllm {
		return ""
	}
	for i, a := range args {
		var id string
		switch {
		case (a == "serve" || a == "--model") && i+1 < len(args):
			id = args[i+1]
		case strings.HasPrefix(a, "--model="):
			id = strings.TrimPrefix(a, "--model=")
		}
		if hfRepoIDRE.MatchString(id) {
			return id
		}
	}
	return ""
}

func hasEnv(ctr *corev1.Container, names ...string) bool {
	for _, e := range ctr.Env {
		for _, n := range names {
			if e.Name == n {
				return true
			}
		}
	}
	return false
}

func ruleHFTransferOff(c *evalCtx) []Finding {
	order, by := c.podsByOwner(c.accelPods)
	var ev []string
	for _, o := range order {
		p := by[o][0]
		for _, ctr := range mldiscover.Containers(&p.Spec) {
			what := hfDownload(ctr)
			if what == "" || hasEnv(ctr, envXetHighPerf, envHFTransfer, envHFOffline) {
				continue
			}
			ev = append(ev, fmt.Sprintf("%s: %s downloads %s from the Hugging Face Hub with neither %s nor %s set", o, ctr.Name, what, envXetHighPerf, envHFTransfer))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s from Hugging Face without the fast path", pv(len(ev), "container", "containers", "downloads", "download")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Set HF_XET_HIGH_PERFORMANCE=1 (huggingface_hub with hf_xet; HF_HUB_ENABLE_HF_TRANSFER is deprecated), or better, stop downloading per pod.",
			Docs:    "https://huggingface.co/docs/huggingface_hub/package_reference/environment_variables"},
	}}
}

// --- image facts -------------------------------------------------------

// normalizeImage writes an image reference the way the kubelet lists it in
// node.status.images: docker.io/library/ for bare names, :latest for no
// tag.
func normalizeImage(image string) string {
	ref := image
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return withDomain(ref[:i]) + ref[i:]
	}
	if mldiscover.ImageTag(ref) == "" {
		ref += ":latest"
	}
	return withDomain(ref)
}

func withDomain(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return "docker.io/library/" + ref
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + ref
	}
	return ref
}

// imageOnNode reports whether node lists image, and its size.
func imageOnNode(n *corev1.Node, image string) (int64, bool) {
	want := normalizeImage(image)
	for _, img := range n.Status.Images {
		for _, name := range img.Names {
			if name == image || normalizeImage(name) == want {
				return img.SizeBytes, true
			}
		}
	}
	return 0, false
}

type imageUse struct {
	image string
	pods  []*corev1.Pod
}

// accelImages groups the accelerator pods' main-container images (those
// that request accelerators), sorted by image.
func (c *evalCtx) accelImages() []imageUse {
	by := map[string][]*corev1.Pod{}
	for _, p := range c.accelPods {
		seen := map[string]bool{}
		for i := range p.Spec.Containers {
			ctr := &p.Spec.Containers[i]
			if len(mldiscover.PodSpecAccelerators(&corev1.PodSpec{Containers: []corev1.Container{*ctr}})) == 0 || seen[ctr.Image] {
				continue
			}
			seen[ctr.Image] = true
			img := normalizeImage(ctr.Image)
			by[img] = append(by[img], p)
		}
	}
	out := make([]imageUse, 0, len(by))
	for _, img := range sortedKeys(by) {
		out = append(out, imageUse{img, by[img]})
	}
	return out
}

func gb(b int64) string { return strconv.FormatFloat(float64(b)/1e9, 'f', 1, 64) + " GB" }

func ruleWeightsInImage(c *evalCtx) []Finding {
	var ev []string
	for _, use := range c.accelImages() {
		var size int64
		where := ""
		for _, n := range sortedNodeList(c) {
			if s, ok := imageOnNode(n, use.image); ok && s > size {
				size, where = s, n.Name
			}
		}
		if size <= WeightsInImageBytes {
			continue
		}
		order, _ := c.podsByOwner(use.pods)
		owners := make([]string, len(order))
		for i, o := range order {
			owners[i] = o.String()
		}
		ev = append(ev, fmt.Sprintf("%s: %s on Node/%s, used by %s", use.image, gb(size), where, strings.Join(owners, ", ")))
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceCertain,
		Title:      fmt.Sprintf("%s over 10 GB: weights are likely baked into the image", plural(len(ev), "accelerator image is", "accelerator images are")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Split it into a weights artifact (a CNCF ModelPack OCI artifact, one layer per weight file) and a slim runtime image, so a code change does not re-pull the weights."},
	}}
}

func ruleNoP2PCache(c *evalCtx) []Finding {
	if len(c.present(mldiscover.KindSpegel)) > 0 || len(c.present(mldiscover.KindDragonfly)) > 0 {
		return nil
	}
	var ev []string
	for _, use := range c.accelImages() {
		nodes := map[string]bool{}
		for _, p := range use.pods {
			if p.Spec.NodeName != "" {
				nodes[p.Spec.NodeName] = true
			}
		}
		if len(use.pods) < P2PFanOut || len(nodes) < 2 {
			continue
		}
		ev = append(ev, fmt.Sprintf("%s: %d accelerator pods on %d nodes", use.image, len(use.pods), len(nodes)))
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      "Accelerator images fan out to many nodes with no peer-to-peer pull cache",
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixKCL,
			Summary: "Install Spegel as the stack's pull_cache (one small DaemonSet: nodes pull layers from each other instead of the registry). It needs containerd's registry config_path and discard_unpacked_layers=false; check its compatibility notes for your node image first.",
			Docs:    "https://spegel.dev/docs/getting-started/"},
	}}
}

// gkeContext parses a gcloud kube context, gke_<project>_<location>_<cluster>.
func gkeContext(ctx string) (project, location, cluster string, ok bool) {
	parts := strings.SplitN(ctx, "_", 4)
	if len(parts) != 4 || parts[0] != "gke" {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

func isArtifactRegistry(image string) bool {
	host, _, _ := strings.Cut(normalizeImage(image), "/")
	return strings.HasSuffix(host, "-docker.pkg.dev")
}

func ruleImageStreamingOff(c *evalCtx) []Finding {
	if c.inv.Cloud != mldiscover.CloudGKE {
		return nil
	}
	type poolUse struct {
		images map[string]bool
		nodes  map[string]bool
	}
	pools := map[string]*poolUse{}
	for _, use := range c.accelImages() {
		if !isArtifactRegistry(use.image) {
			continue
		}
		for _, p := range use.pods {
			n := c.nodes[p.Spec.NodeName]
			if n == nil {
				continue
			}
			pool := mldiscover.NodePool(n)
			if pool == "" {
				continue
			}
			if pools[pool] == nil {
				pools[pool] = &poolUse{images: map[string]bool{}, nodes: map[string]bool{}}
			}
			pools[pool].images[use.image] = true
		}
	}
	project, location, cluster, ok := gkeContext(c.inv.Context)
	if !ok {
		project, location, cluster = "<project>", "<location>", "<cluster>"
	}
	var out []Finding
	for _, pool := range sortedKeys(pools) {
		pu := pools[pool]
		for _, n := range sortedNodeList(c) {
			if mldiscover.NodePool(n) == pool && n.Labels[mldiscover.LabelGKEImageStreaming] != "true" {
				pu.nodes[n.Name] = true
			}
		}
		if len(pu.nodes) == 0 {
			continue
		}
		ev := []string{fmt.Sprintf("pool %s: %s without %s=true", pool, strings.Join(sortedSet(pu.nodes), ", "), mldiscover.LabelGKEImageStreaming)}
		for _, img := range sortedSet(pu.images) {
			ev = append(ev, "Artifact Registry image: "+img)
		}
		out = append(out, Finding{
			Confidence: ConfidenceCertain,
			Cloud:      mldiscover.CloudGKE,
			Title:      fmt.Sprintf("Image Streaming is off on GPU pool %s", pool),
			Evidence:   ev,
			Fix: Fix{Kind: FixCommand,
				Summary: fmt.Sprintf("Enable Image Streaming on node pool %s, so accelerator pods start before their Artifact Registry image is fully pulled.", pool),
				Command: fmt.Sprintf("gcloud container node-pools update %s --cluster=%s --location=%s --project=%s --enable-image-streaming", pool, cluster, location, project),
				Docs:    "https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming"},
		})
	}
	return out
}

// --- accel-unallocated -------------------------------------------------

func ruleUnallocated(c *evalCtx) []Finding {
	byPool := map[string][]string{}
	total := 0
	for _, m := range c.inv.Machines {
		if m.Allocatable <= 0 || m.Allocated > 0 || !m.Ready || m.Unschedulable {
			continue
		}
		pool := m.Pool
		if pool == "" {
			pool = "(no pool label)"
		}
		byPool[pool] = append(byPool[pool], fmt.Sprintf("Node/%s: %d × %s allocatable, none requested", m.Name, m.Allocatable, m.Resource))
		total++
	}
	if total == 0 {
		return nil
	}
	var ev []string
	for _, pool := range sortedKeys(byPool) {
		for _, l := range byPool[pool] {
			ev = append(ev, "pool "+pool+": "+l)
		}
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s had idle, unrequested accelerators at snapshot time", plural(total, "node", "nodes")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "One snapshot is not a trend. If these nodes stay unrequested, lower the pool's minimum size in its autoscaler (cluster autoscaler, Karpenter, the cloud's node pool) so it scales down."},
	}}
}

// --- image-not-warm ----------------------------------------------------

func ruleImageNotWarm(c *evalCtx) []Finding {
	// Pools of accelerator nodes: the pool label, else vendor/product.
	pools := map[string][]*corev1.Node{}
	poolOf := map[string]string{}
	for _, m := range c.inv.Machines {
		n := c.nodes[m.Name]
		if n == nil || !m.Ready || m.Unschedulable {
			continue
		}
		key := m.Pool
		if key == "" {
			key = m.Vendor + "/" + m.Product
		}
		pools[key] = append(pools[key], n)
		poolOf[m.Name] = key
	}
	var ev []string
	for _, use := range c.accelImages() {
		keys := map[string]bool{}
		for _, p := range use.pods {
			if k, ok := poolOf[p.Spec.NodeName]; ok {
				keys[k] = true
			}
		}
		for _, k := range sortedSet(keys) {
			nodes := pools[k]
			if len(nodes) < 2 {
				continue
			}
			var has, missing []string
			for _, n := range nodes {
				if _, ok := imageOnNode(n, use.image); ok {
					has = append(has, n.Name)
				} else {
					missing = append(missing, n.Name)
				}
			}
			// An image no node lists is beyond what status.images shows
			// (the kubelet lists at most 50 per node): no claim.
			if len(has) == 0 || len(missing) == 0 {
				continue
			}
			sort.Strings(missing)
			ev = append(ev, fmt.Sprintf("%s: not on %s (pool %s; present on %d of %d nodes)", use.image, strings.Join(missing, ", "), k, len(has), len(nodes)))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: ConfidenceLikely,
		Title:      fmt.Sprintf("%s not warm on every node of its pool", plural(len(ev), "accelerator image is", "accelerator images are")),
		Evidence:   capEvidence(append(ev, "node.status.images lists at most 50 images per node by default, so a busy node may hold an image it does not list")),
		Fix: Fix{Kind: FixManual,
			Summary: "Pre-pull the image on the pool (a pre-pull DaemonSet, kube-fledged), or bake it into the node image (GKE: a secondary boot disk), so a new pod does not wait for the pull.",
			Docs:    "https://github.com/senthilrch/kube-fledged"},
	}}
}

// --- image-volumes-unavailable -----------------------------------------

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?`)

func minorOf(version string) (major, minor int, ok bool) {
	m := versionRE.FindStringSubmatch(version)
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

func ruleImageVolumes(c *evalCtx) []Finding {
	if len(c.inv.Machines) == 0 {
		return nil
	}
	var ev []string
	conf := ConfidenceCertain
	if major, minor, ok := minorOf(c.inv.ServerVersion); ok && major == 1 && minor < ImageVolumeDefaultOnMinor {
		ev = append(ev, fmt.Sprintf("API server %s: the ImageVolume feature gate is on by default from Kubernetes 1.%d", c.inv.ServerVersion, ImageVolumeDefaultOnMinor))
	}
	for _, m := range c.inv.Machines {
		rt, ver, found := strings.Cut(m.ContainerRuntime, "://")
		if !found || rt != "containerd" {
			continue
		}
		if major, minor, ok := minorOf(ver); ok && (major < 2 || (major == 2 && minor < 1)) {
			ev = append(ev, fmt.Sprintf("Node/%s: containerd %s, image volumes need 2.1+", m.Name, ver))
			if len(ev) == 1 {
				conf = ConfidenceLikely
			}
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: conf,
		Title:      "Image volumes are unavailable: weights cannot be mounted from an OCI artifact without a sidecar",
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: fmt.Sprintf("Upgrade to Kubernetes 1.%d+ with containerd 2.1+ to mount weights as an image volume. Until then, KServe's modelcar sidecar or an init-container copy is the fallback.", ImageVolumeDefaultOnMinor),
			Docs:    "https://kubernetes.io/docs/concepts/storage/volumes/#image"},
	}}
}
