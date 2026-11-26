package mldiscover

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	inv := &Inventory{Context: s.Context, ServerVersion: s.ServerVersion, CollectedAt: s.CollectedAt,
		Accelerators: []AcceleratorGroup{}, Machines: []Machine{}, Components: []Component{}}
	inv.Cloud, inv.Provider = detectCloud(s)
	inv.Machines, inv.Accelerators, inv.Warnings = discoverMachines(s)

	d := &discoverer{s: s, inv: inv, byKey: map[string]int{}}
	d.matchWorkloads()
	d.matchPods()
	d.matchClusterPolicy()
	d.matchNodeFacts()
	d.matchCRDs()
	d.finish()
	return inv
}

// detectCloud reads the managed service from node labels and the server
// version, and the infrastructure from provider IDs.
func detectCloud(s *Snapshot) (cloud, provider string) {
	cloud = CloudOther
	nodes := sortedNodes(s)
	for _, n := range nodes {
		if provider == "" {
			switch {
			case strings.HasPrefix(n.Spec.ProviderID, "gce://"):
				provider = ProviderGCP
			case strings.HasPrefix(n.Spec.ProviderID, "aws://"):
				provider = ProviderAWS
			case strings.HasPrefix(n.Spec.ProviderID, "azure://"):
				provider = ProviderAzure
			}
		}
	}
	// The labels only the managed service's node pools carry: a TPU or
	// accelerator label alone does not make a cluster GKE.
	for _, c := range []struct {
		cloud   string
		labels  []string
		version string
	}{
		{CloudGKE, []string{LabelGKENodePool}, "-gke."},
		{CloudEKS, []string{LabelEKSNodeGroup, LabelEKSComputeType, "eks.amazonaws.com/capacityType"}, "-eks-"},
		{CloudAKS, []string{LabelAKSAgentPool, "kubernetes.azure.com/cluster"}, ""},
	} {
		if c.version != "" && strings.Contains(s.ServerVersion, c.version) {
			return c.cloud, provider
		}
		for _, n := range nodes {
			for _, l := range c.labels {
				if _, ok := n.Labels[l]; ok {
					return c.cloud, provider
				}
			}
		}
	}
	return cloud, provider
}

func sortedNodes(s *Snapshot) []*corev1.Node {
	out := make([]*corev1.Node, len(s.Nodes))
	for i := range s.Nodes {
		out[i] = &s.Nodes[i]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func discoverMachines(s *Snapshot) ([]Machine, []AcceleratorGroup, []string) {
	machines := []Machine{}
	var warnings []string
	type groupAcc struct {
		g       AcceleratorGroup
		sharing map[string]bool
		drivers map[string]bool
	}
	groups := map[string]*groupAcc{}
	for _, n := range sortedNodes(s) {
		acc, ok := NodeAcceleratorOf(n)
		if !ok {
			continue
		}
		m := Machine{
			Name: n.Name, Vendor: acc.Vendor, Resource: acc.Resource,
			Product: NodeProduct(acc.Vendor, n.Labels), Pool: NodePool(n),
			Capacity: acc.Capacity, Allocatable: acc.Allocatable,
			Allocated:     allocatedOn(s.Pods, n.Name, acc.Family),
			Unschedulable: n.Spec.Unschedulable, Spot: IsSpot(n),
			ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
			KubeletVersion:   n.Status.NodeInfo.KubeletVersion,
			DetectedBy:       acc.DetectedBy,
			InstanceType:     InstanceType(n.Labels),
		}
		if acc.Vendor == VendorNVIDIA {
			m.Sharing = NodeSharing(n)
			m.DriverVersion = DriverVersion(n.Labels)
		}
		m.Ready, m.Conditions = nodeConditions(n)
		machines = append(machines, m)
		if acc.DetectedBy == DetectedByLabel {
			warnings = append(warnings, fmt.Sprintf("node %s has %s labels but no %s capacity: the device plugin is missing or not ready", n.Name, acc.Vendor, acc.Resource))
		}

		key := strings.Join([]string{m.Vendor, m.Resource, m.Product, m.Pool, m.DetectedBy}, "\x00")
		ga, ok := groups[key]
		if !ok {
			ga = &groupAcc{g: AcceleratorGroup{Vendor: m.Vendor, Resource: m.Resource, Product: m.Product, Pool: m.Pool, DetectedBy: m.DetectedBy},
				sharing: map[string]bool{}, drivers: map[string]bool{}}
			groups[key] = ga
		}
		ga.g.Nodes++
		ga.g.Capacity += m.Capacity
		ga.g.Allocatable += m.Allocatable
		ga.g.Allocated += m.Allocated
		ga.sharing[m.Sharing] = true
		if m.DriverVersion != "" {
			ga.drivers[m.DriverVersion] = true
		}
	}
	out := make([]AcceleratorGroup, 0, len(groups))
	for _, ga := range groups {
		g := ga.g
		if len(ga.sharing) == 1 {
			for v := range ga.sharing {
				g.Sharing = v
			}
		} else {
			g.Sharing = SharingMixed
		}
		g.DriverVersions = sortedKeys(ga.drivers)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if VendorRank(a.Vendor) != VendorRank(b.Vendor) {
			return VendorRank(a.Vendor) < VendorRank(b.Vendor)
		}
		return strings.Join([]string{a.Resource, a.Pool, a.Product, a.DetectedBy}, "\x00") <
			strings.Join([]string{b.Resource, b.Pool, b.Product, b.DetectedBy}, "\x00")
	})
	return machines, out, warnings
}

// nodeConditions reports Ready and every condition that is not its type's
// healthy value: True for Ready and the `*Ready` types the EKS agent sets,
// False for every other type.
func nodeConditions(n *corev1.Node) (bool, []string) {
	ready := false
	var bad []string
	conds := append([]corev1.NodeCondition(nil), n.Status.Conditions...)
	sort.Slice(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	for _, c := range conds {
		t := string(c.Type)
		healthy := corev1.ConditionFalse
		if strings.HasSuffix(t, "Ready") {
			healthy = corev1.ConditionTrue
		}
		if t == string(corev1.NodeReady) {
			ready = c.Status == corev1.ConditionTrue
		}
		if c.Status != healthy {
			bad = append(bad, t+"="+string(c.Status))
		}
	}
	return ready, bad
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type discoverer struct {
	s     *Snapshot
	inv   *Inventory
	comps []Component
	byKey map[string]int
}

func compKey(kind, namespace, workload string) string {
	return kind + "\x00" + namespace + "\x00" + workload
}

func (d *discoverer) add(c Component) *Component {
	k := compKey(c.Kind, c.Namespace, c.Workload)
	if i, ok := d.byKey[k]; ok {
		d.comps[i].MatchedBy = append(d.comps[i].MatchedBy, c.MatchedBy...)
		return &d.comps[i]
	}
	d.byKey[k] = len(d.comps)
	d.comps = append(d.comps, c)
	return &d.comps[len(d.comps)-1]
}

func (d *discoverer) ofKind(kind string) []*Component {
	var out []*Component
	for i := range d.comps {
		if d.comps[i].Kind == kind {
			out = append(out, &d.comps[i])
		}
	}
	return out
}

// workload is the part of a DaemonSet, Deployment or StatefulSet discovery
// reads.
type workload struct {
	kind     string
	meta     metav1.ObjectMeta
	selector *metav1.LabelSelector
	template *corev1.PodTemplateSpec
	ready    int32
}

func (d *discoverer) workloads() []workload {
	var out []workload
	for i := range d.s.DaemonSets {
		w := &d.s.DaemonSets[i]
		out = append(out, workload{WorkloadDaemonSet, w.ObjectMeta, w.Spec.Selector, &w.Spec.Template, w.Status.NumberReady})
	}
	for i := range d.s.Deployments {
		w := &d.s.Deployments[i]
		out = append(out, workload{WorkloadDeployment, w.ObjectMeta, w.Spec.Selector, &w.Spec.Template, w.Status.ReadyReplicas})
	}
	for i := range d.s.StatefulSets {
		w := &d.s.StatefulSets[i]
		out = append(out, workload{WorkloadStatefulSet, w.ObjectMeta, w.Spec.Selector, &w.Spec.Template, w.Status.ReadyReplicas})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.kind+"/"+a.meta.Namespace+"/"+a.meta.Name < b.kind+"/"+b.meta.Namespace+"/"+b.meta.Name
	})
	return out
}

// ownership reports the owner of a workload from its labels and whether it
// is forge's own platform (forge.dev/ labels without a component owner),
// which is not an ML component.
func ownership(l ...map[string]string) (owner string, platform bool) {
	for _, m := range l {
		if m[LabelComponentOwner] != "" {
			return OwnerForge, false
		}
	}
	for _, m := range l {
		for k := range m {
			if strings.HasPrefix(k, "forge.dev/") {
				return "", true
			}
		}
	}
	return OwnerTheirs, false
}

func (d *discoverer) matchWorkloads() {
	for _, w := range d.workloads() {
		owner, platform := ownership(w.meta.Labels, w.template.Labels)
		if platform {
			continue
		}
		for si := range signatures {
			sig := &signatures[si]
			if !sig.allowsWorkload(w.kind) {
				continue
			}
			if len(w.template.Spec.Containers) == 0 {
				continue
			}
			var matched []string
			ci := 0
			for _, p := range sig.names {
				if strings.HasPrefix(w.meta.Name, p) {
					matched = append(matched, "name:"+p)
				}
			}
			for i := range w.template.Spec.Containers {
				c := &w.template.Spec.Containers[i]
				if m := sig.matchContainer(w.meta.Namespace, c.Name, c.Image, append(append([]string{}, c.Command...), c.Args...)); len(m) > 0 {
					matched, ci = append(matched, m...), i
					break
				}
			}
			if m := sig.matchLabels(w.template.Labels); m != "" {
				matched = append(matched, m)
			}
			if len(matched) == 0 {
				continue
			}
			c := &w.template.Spec.Containers[ci]
			comp := Component{
				Kind: sig.kind, Category: sig.category, Vendor: sig.vendor, Status: StatusPresent, Owner: owner,
				Version: ImageTag(c.Image), Namespace: w.meta.Namespace, Workload: w.kind + "/" + w.meta.Name,
				Via: viaOf(w.meta), Pods: int(w.ready), MatchedBy: matched,
				ChartVersion: chartVersion(w.meta.Labels[LabelHelmChart]),
			}
			if w.selector != nil && len(w.selector.MatchLabels) > 0 {
				comp.Selector = copyMap(w.selector.MatchLabels)
			}
			if sig.category == CategoryAccelMetrics {
				comp.MetricsPort = metricsPort(c, sig.kind)
			}
			d.add(comp)
		}
	}
}

func (d *discoverer) matchPods() {
	type group struct {
		comp   Component
		labels map[string]string
	}
	groups := map[string]*group{}
	var order []string
	pods := make([]*corev1.Pod, 0, len(d.s.Pods))
	for i := range d.s.Pods {
		pods = append(pods, &d.s.Pods[i])
	}
	sort.Slice(pods, func(i, j int) bool { return podKey(pods[i]) < podKey(pods[j]) })
	for _, p := range pods {
		if !PodActive(p) {
			continue
		}
		owner, platform := ownership(p.Labels)
		if platform {
			continue
		}
		for si := range signatures {
			sig := &signatures[si]
			if !sig.pods {
				continue
			}
			labelMatch := sig.matchLabels(p.Labels)
			for i := range p.Spec.Containers {
				c := &p.Spec.Containers[i]
				m := sig.matchContainer(p.Namespace, c.Name, c.Image, append(append([]string{}, c.Command...), c.Args...))
				if labelMatch != "" && (len(m) > 0 || i == len(p.Spec.Containers)-1) {
					m = append(m, labelMatch)
				}
				if len(m) == 0 {
					continue
				}
				o := PodOwner(d.s, p)
				workloadName := o.Kind + "/" + o.Name
				if _, ok := d.byKey[compKey(sig.kind, p.Namespace, workloadName)]; ok {
					break
				}
				k := compKey(sig.kind, p.Namespace, workloadName)
				g, ok := groups[k]
				if !ok {
					via := viaOf(p.ObjectMeta)
					comp := Component{
						Kind: sig.kind, Category: sig.category, Vendor: sig.vendor, Status: StatusPresent, Owner: owner,
						Version: ImageTag(c.Image), Namespace: p.Namespace, Workload: workloadName, Via: via,
						MatchedBy: m, ChartVersion: chartVersion(p.Labels[LabelHelmChart]),
					}
					if sig.category == CategoryAccelMetrics {
						comp.MetricsPort = metricsPort(c, sig.kind)
					}
					g = &group{comp: comp, labels: copyMap(p.Labels)}
					groups[k] = g
					order = append(order, k)
				} else {
					for lk, lv := range g.labels {
						if p.Labels[lk] != lv {
							delete(g.labels, lk)
						}
					}
				}
				if p.Status.Phase == corev1.PodRunning {
					g.comp.Pods++
				}
				break
			}
		}
	}
	for _, k := range order {
		g := groups[k]
		for _, h := range []string{"pod-template-hash", "controller-revision-hash", "pod-template-generation", "statefulset.kubernetes.io/pod-name", "apps.kubernetes.io/pod-index"} {
			delete(g.labels, h)
		}
		if len(g.labels) > 0 {
			g.comp.Selector = g.labels
		}
		d.add(g.comp)
	}
}

// ClusterPolicySummary is what the rules read from one NVIDIA GPU Operator
// ClusterPolicy.
type ClusterPolicySummary struct {
	Name string
	// DCGMExporter is spec.dcgmExporter.enabled (the operator's default,
	// true, when unset).
	DCGMExporter bool
	// DCGM is spec.dcgm.enabled, the standalone host engine (true when
	// unset, as in the operator's API types; its helm chart sets false).
	DCGM bool
}

// ClusterPolicies summarises the snapshot's ClusterPolicy objects, sorted
// by name.
func ClusterPolicies(s *Snapshot) []ClusterPolicySummary {
	var out []ClusterPolicySummary
	for _, obj := range s.CustomObjects["clusterpolicies.nvidia.com"] {
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		spec, _ := obj["spec"].(map[string]any)
		out = append(out, ClusterPolicySummary{
			Name:         name,
			DCGMExporter: enabledOr(spec, "dcgmExporter", true),
			DCGM:         enabledOr(spec, "dcgm", true),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func enabledOr(spec map[string]any, field string, def bool) bool {
	sub, ok := spec[field].(map[string]any)
	if !ok {
		return def
	}
	v, ok := sub["enabled"].(bool)
	if !ok {
		return def
	}
	return v
}

func (d *discoverer) matchClusterPolicy() {
	policies := ClusterPolicies(d.s)
	if len(policies) == 0 {
		return
	}
	opNS := "gpu-operator"
	for _, c := range d.ofKind(KindGPUOperator) {
		if c.Namespace != "" {
			opNS = c.Namespace
			break
		}
	}
	// A DaemonSet in the operator's namespace with the operator's names is
	// the operator's even when its owner reference was not recorded.
	for i := range d.comps {
		c := &d.comps[i]
		if c.Via == "" && c.Namespace == opNS && strings.Contains(c.Workload, "/nvidia-") {
			c.Via = ViaGPUOperator
		}
	}
	exporter, hostengine := false, false
	for _, p := range policies {
		exporter = exporter || p.DCGMExporter
		hostengine = hostengine || p.DCGM
	}
	derive := func(kind, field string, port int32, selector map[string]string) {
		sig := "clusterpolicy:spec." + field + ".enabled"
		for _, c := range d.ofKind(kind) {
			if c.Via == ViaGPUOperator {
				c.MatchedBy = append(c.MatchedBy, sig)
				return
			}
		}
		d.add(Component{Kind: kind, Category: CategoryAccelMetrics, Vendor: VendorNVIDIA, Status: StatusPresent,
			Owner: OwnerTheirs, Namespace: opNS, Via: ViaGPUOperator, Selector: selector, MetricsPort: port,
			MatchedBy: []string{sig}})
	}
	if exporter {
		derive(KindDCGMExporter, "dcgmExporter", DefaultMetricsPort(KindDCGMExporter), map[string]string{"app": "nvidia-dcgm-exporter"})
	}
	if hostengine {
		derive(KindDCGMHostEngine, "dcgm", 0, map[string]string{"app": "nvidia-dcgm"})
	}
}

// matchNodeFacts finds what node capacity, conditions and labels show:
// device plugins with no workload in the snapshot, node-problem-detector
// conditions (GKE runs it on the node, not as a pod), the EKS node
// monitoring agent's conditions, Karpenter-provisioned nodes, DRA slices.
func (d *discoverer) matchNodeFacts() {
	capacity := map[string]bool{}
	conds := map[string]bool{}
	karpenter := false
	for _, n := range sortedNodes(d.s) {
		for name, q := range n.Status.Capacity {
			if q.Value() > 0 {
				capacity[string(name)] = true
			}
		}
		for _, c := range n.Status.Conditions {
			conds[string(c.Type)] = true
		}
		if n.Labels[LabelKarpenterNodePool] != "" {
			karpenter = true
		}
	}
	for _, dp := range []struct{ kind, vendor, resource string }{
		{KindNVIDIADevicePlugin, VendorNVIDIA, ResourceNVIDIAGPU},
		{KindNeuronDevicePlugin, VendorNeuron, ResourceNeuronCore},
		{KindNeuronDevicePlugin, VendorNeuron, ResourceNeuron},
	} {
		if capacity[dp.resource] && len(d.ofKind(dp.kind)) == 0 {
			via := ""
			if d.inv.Cloud == CloudGKE {
				via = ViaGKEManaged
			}
			d.add(Component{Kind: dp.kind, Category: CategoryDevicePlugin, Vendor: dp.vendor, Status: StatusPresent,
				Owner: OwnerTheirs, Via: via, MatchedBy: []string{"capacity:" + dp.resource}})
		}
	}
	var npd, eks []string
	for _, t := range sortedKeys(conds) {
		switch {
		case IsCoreCondition(t):
		case IsEKSAgentCondition(t):
			eks = append(eks, "condition:"+t)
		default:
			npd = append(npd, "condition:"+t)
		}
	}
	nodeHealth := func(kind string, matched []string) {
		if len(matched) == 0 {
			return
		}
		if cs := d.ofKind(kind); len(cs) > 0 {
			for _, c := range cs {
				c.MatchedBy = append(c.MatchedBy, matched...)
			}
			return
		}
		via := ""
		if kind == KindNodeProblemDetector && d.inv.Cloud == CloudGKE {
			via = ViaGKEManaged
		}
		d.add(Component{Kind: kind, Category: CategoryNodeHealth, Status: StatusPresent, Owner: OwnerTheirs, Via: via, MatchedBy: matched})
	}
	nodeHealth(KindNodeProblemDetector, npd)
	nodeHealth(KindEKSNodeMonitoring, eks)
	if karpenter && len(d.ofKind(KindKarpenter)) == 0 {
		d.add(Component{Kind: KindKarpenter, Category: CategoryAutoscaling, Status: StatusPresent, Owner: OwnerTheirs,
			MatchedBy: []string{"label:" + LabelKarpenterNodePool}})
	}
	if contains(d.s.APIGroups, "resource.k8s.io") && d.s.CustomResources["resourceslices.resource.k8s.io"] > 0 {
		d.add(Component{Kind: KindDRA, Category: CategoryDevicePlugin, Status: StatusPresent, Owner: OwnerTheirs,
			Resources: d.s.CustomResources["resourceslices.resource.k8s.io"], MatchedBy: []string{"group:resource.k8s.io"}})
	}
}

func (d *discoverer) matchCRDs() {
	crds := append([]string(nil), d.s.CRDs...)
	sort.Strings(crds)
	for si := range signatures {
		sig := &signatures[si]
		if len(sig.crds) == 0 && len(sig.groups) == 0 {
			continue
		}
		var matched []string
		objects := 0
		for _, name := range crds {
			ok := contains(sig.crds, name)
			for _, g := range sig.groups {
				ok = ok || strings.HasSuffix(name, "."+g)
			}
			if !ok {
				continue
			}
			matched = append(matched, "crd:"+name)
			n := d.s.CustomResources[name]
			if l := len(d.s.CustomObjects[name]); l > n {
				n = l
			}
			objects += n
		}
		if len(matched) == 0 {
			continue
		}
		if cs := d.ofKind(sig.kind); len(cs) > 0 {
			for _, c := range cs {
				c.MatchedBy = append(c.MatchedBy, matched...)
				c.Resources = objects
			}
			continue
		}
		c := Component{Kind: sig.kind, Category: sig.category, Vendor: sig.vendor, Status: StatusPresent,
			Owner: OwnerTheirs, Resources: objects, MatchedBy: matched}
		switch {
		case sig.objects && objects > 0:
		case sig.kind == KindGMP && d.inv.Cloud == CloudGKE:
			c.Via = ViaGKEManaged
		default:
			c.Status = StatusCRDOnly
			names := make([]string, len(matched))
			for i, m := range matched {
				names[i] = strings.TrimPrefix(m, "crd:")
			}
			d.inv.Warnings = append(d.inv.Warnings, fmt.Sprintf(
				"%s: CRDs %s are installed but no controller runs; forge treats it as absent (a leftover from an uninstall, or a controller in another cluster)",
				sig.kind, strings.Join(names, ", ")))
		}
		d.add(c)
	}
}

func (d *discoverer) finish() {
	for i := range d.comps {
		c := &d.comps[i]
		c.MatchedBy = uniqueSorted(c.MatchedBy)
	}
	sort.SliceStable(d.comps, func(i, j int) bool {
		a, b := d.comps[i], d.comps[j]
		return strings.Join([]string{a.Category, a.Kind, a.Namespace, a.Workload}, "\x00") <
			strings.Join([]string{b.Category, b.Kind, b.Namespace, b.Workload}, "\x00")
	})
	d.inv.Components = append(d.inv.Components, d.comps...)
	errs := append([]ReadError(nil), d.s.ReadErrors...)
	sort.Slice(errs, func(i, j int) bool { return errs[i].Resource < errs[j].Resource })
	for _, e := range errs {
		d.inv.Warnings = append(d.inv.Warnings, fmt.Sprintf("could not list %s (%s): components it would show are missing from this inventory", e.Resource, e.Error))
	}
	sort.Strings(d.inv.Warnings)
}

func uniqueSorted(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	out := sortedKeys(set)
	if out == nil {
		out = []string{}
	}
	return out
}

func viaOf(meta metav1.ObjectMeta) string {
	for _, ref := range meta.OwnerReferences {
		if ref.Kind == "ClusterPolicy" {
			return ViaGPUOperator
		}
	}
	if meta.Labels["app.kubernetes.io/managed-by"] == "gpu-operator" {
		return ViaGPUOperator
	}
	if meta.Namespace == NamespaceGKEManagedSystem {
		return ViaGKEManaged
	}
	return ""
}

// metricsPort is the container's port named like metrics, else the kind's
// default (fake-gpu-operator serves DCGM's 9400 on a port named "http").
func metricsPort(c *corev1.Container, kind string) int32 {
	for _, p := range c.Ports {
		switch p.Name {
		case "metrics", "gpu-metrics", "http-metrics", "prometheus":
			return p.ContainerPort
		}
	}
	return DefaultMetricsPort(kind)
}

var chartVersionRE = regexp.MustCompile(`-(v?[0-9]+\.[0-9]+.*)$`)

// chartVersion takes the version out of a helm.sh/chart label
// (dcgm-exporter-4.9.0 → 4.9.0).
func chartVersion(label string) string {
	m := chartVersionRE.FindStringSubmatch(label)
	if m == nil {
		return ""
	}
	return m[1]
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
