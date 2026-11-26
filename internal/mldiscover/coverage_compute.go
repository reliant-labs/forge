package mldiscover

import (
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// Coverage categories.
const (
	CoverageCategoryAccelerator = "accelerator"
	CoverageCategoryNodeHealth  = "node-health"
	CoverageCategoryTracing     = "tracing"
	CoverageCategoryTelemetry   = "telemetry"
	CoverageCategoryProfiling   = "profiling"
	CoverageCategoryInference   = "inference"
)

// signalCatalogue is every signal in display order, with its category.
var signalCatalogue = []struct{ id, category string }{
	{SignalAccelUtilisation, CoverageCategoryAccelerator},
	{SignalAccelProfiling, CoverageCategoryAccelerator},
	{SignalAccelMemory, CoverageCategoryAccelerator},
	{SignalAccelXid, CoverageCategoryAccelerator},
	{SignalAccelECC, CoverageCategoryAccelerator},
	{SignalAccelRetiredPages, CoverageCategoryAccelerator},
	{SignalAccelRowRemap, CoverageCategoryAccelerator},
	{SignalAccelNVLink, CoverageCategoryAccelerator},
	{SignalAccelPCIe, CoverageCategoryAccelerator},
	{SignalAccelThrottle, CoverageCategoryAccelerator},
	{SignalAccelPowerThermal, CoverageCategoryAccelerator},
	{SignalAccelPodAttribution, CoverageCategoryAccelerator},
	{SignalNodeHealth, CoverageCategoryNodeHealth},
	{SignalNodeHealthGPURules, CoverageCategoryNodeHealth},
	{SignalKubeletTracing, CoverageCategoryTracing},
	{SignalAPIServerTracing, CoverageCategoryTracing},
	{SignalCollector, CoverageCategoryTelemetry},
	{SignalOTLPReceiver, CoverageCategoryTelemetry},
	{SignalK8sEvents, CoverageCategoryTelemetry},
	{SignalKubeletStats, CoverageCategoryTelemetry},
	{SignalContainerLogs, CoverageCategoryTelemetry},
	{SignalContinuousProfiling, CoverageCategoryProfiling},
	{SignalGPUProfiling, CoverageCategoryProfiling},
	{SignalTrainingTracing, CoverageCategoryTracing},
	{SignalInferenceTracing, CoverageCategoryTracing},
	{SignalInferenceMetrics, CoverageCategoryInference},
}

// Signals lists every signal id in display order.
func Signals() []string {
	out := make([]string, len(signalCatalogue))
	for i, s := range signalCatalogue {
		out[i] = s.id
	}
	return out
}

// SignalCategory returns a signal's display category.
func SignalCategory(signal string) string {
	for _, s := range signalCatalogue {
		if s.id == signal {
			return s.category
		}
	}
	return ""
}

// determinedRank orders DeterminedBy values, strongest first.
var determinedRank = map[string]int{
	DeterminedByProbe: 0, DeterminedByConfig: 1, DeterminedByArgs: 2,
	DeterminedByVersionDefault: 3, DeterminedByPresence: 4, DeterminedByUnreadable: 5,
}

// weakest returns the weakest of DeterminedBy values.
func weakest(vals ...string) string {
	out := ""
	for _, v := range vals {
		if v == "" {
			continue
		}
		if out == "" || determinedRank[v] > determinedRank[out] {
			out = v
		}
	}
	return out
}

// strongest returns the strongest of DeterminedBy values.
func strongest(vals ...string) string {
	out := ""
	for _, v := range vals {
		if v == "" {
			continue
		}
		if out == "" || determinedRank[v] < determinedRank[out] {
			out = v
		}
	}
	return out
}

// ComponentRef names a component as kind/namespace/name of its workload,
// or by kind and manager when it has none.
func ComponentRef(c Component) string {
	switch {
	case c.Workload != "":
		kind, name, _ := strings.Cut(c.Workload, "/")
		return kind + "/" + c.Namespace + "/" + name
	case c.Via != "":
		return c.Kind + " (via " + c.Via + ")"
	}
	return c.Kind
}

func versionOf(c Component) string {
	if c.Version == "" {
		return ""
	}
	return c.Kind + " " + c.Version
}

func settingValue(cfg *ComponentConfig, key string) string {
	if cfg == nil {
		return ""
	}
	for _, s := range cfg.Settings {
		if k, v, ok := strings.Cut(s, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// coverageCtx holds what every coverage row reads.
type coverageCtx struct {
	s     *Snapshot
	inv   *Inventory
	nodes []*corev1.Node
}

func (c *coverageCtx) present(kinds ...string) []Component {
	var out []Component
	for _, comp := range c.inv.Components {
		if comp.Status == StatusPresent && contains(kinds, comp.Kind) {
			out = append(out, comp)
		}
	}
	return out
}

func (c *coverageCtx) machinesOf(vendor string) []string {
	var out []string
	for _, m := range c.inv.Machines {
		if vendor == "" || m.Vendor == vendor {
			out = append(out, m.Name)
		}
	}
	sort.Strings(out)
	return out
}

// targetNodes are the nodes a node-scoped, non-accelerator signal applies
// to: the accelerator nodes when there are any, else every node.
func (c *coverageCtx) targetNodes() []string {
	if m := c.machinesOf(""); len(m) > 0 {
		return m
	}
	out := make([]string, len(c.nodes))
	for i, n := range c.nodes {
		out[i] = n.Name
	}
	return out
}

func refs(comps []Component) []string {
	set := map[string]bool{}
	for _, c := range comps {
		set[ComponentRef(c)] = true
	}
	return sortedKeysNil(set)
}

func versions(comps []Component) []string {
	set := map[string]bool{}
	for _, c := range comps {
		if v := versionOf(c); v != "" {
			set[v] = true
		}
	}
	return sortedKeysNil(set)
}

func sortedKeysNil(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return sortedKeys(m)
}

func setOf(list []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range list {
		out[s] = true
	}
	return out
}

func minus(expected, present []string) []string {
	p := setOf(present)
	var out []string
	for _, e := range expected {
		if !p[e] {
			out = append(out, e)
		}
	}
	return out
}

// nodeStatus is the status of a node-scoped signal from how many target
// nodes it reaches.
func nodeStatus(covered, total int) string {
	switch {
	case total == 0:
		return CoverageNotApplicable
	case covered == 0:
		return CoverageMissing
	case covered < total:
		return CoveragePartial
	}
	return CoverageCollected
}

// computeCoverage fills one row per signal.
func computeCoverage(s *Snapshot, inv *Inventory) []SignalCoverage {
	c := &coverageCtx{s: s, inv: inv, nodes: sortedNodes(s)}
	rows := map[string]SignalCoverage{}
	for _, r := range c.acceleratorRows() {
		rows[r.Signal] = r
	}
	for _, r := range []SignalCoverage{
		c.nodeHealthRow(), c.gpuRulesRow(), c.kubeletTracingRow(), c.apiserverTracingRow(),
		c.collectorRow(),
		c.telemetryRow(SignalOTLPReceiver, "otlp"), c.telemetryRow(SignalK8sEvents, "k8s_events"),
		c.telemetryRow(SignalKubeletStats, "kubeletstats"), c.telemetryRow(SignalContainerLogs, "container-logs"),
		c.profilingRow(), c.gpuProfilingRow(), c.trainingTracingRow(), c.inferenceTracingRow(), c.inferenceMetricsRow(),
	} {
		rows[r.Signal] = r
	}
	out := make([]SignalCoverage, 0, len(signalCatalogue))
	for _, sc := range signalCatalogue {
		r := rows[sc.id]
		r.Signal, r.Category = sc.id, sc.category
		if r.Status == "" {
			r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByPresence
		}
		if (r.Status == CoverageCollected || r.Status == CoveragePartial || r.Status == CoverageMissing) && len(r.Expected) > 0 {
			r.Missing = minus(r.Expected, r.Present)
		}
		out = append(out, r)
	}
	return out
}

// --- accelerator -------------------------------------------------------

type dcgmSource struct {
	comp   Component
	nodes  []string
	known  bool
	parts  map[string][]string // signal -> parts present
	attr   string              // "true", "false", "unknown"
	attrBy string
}

func (c *coverageCtx) acceleratorRows() []SignalCoverage {
	accelSignals := []string{SignalAccelUtilisation, SignalAccelProfiling, SignalAccelMemory, SignalAccelXid, SignalAccelECC,
		SignalAccelRetiredPages, SignalAccelRowRemap, SignalAccelNVLink, SignalAccelPCIe, SignalAccelThrottle,
		SignalAccelPowerThermal, SignalAccelPodAttribution}
	nv := c.machinesOf(VendorNVIDIA)
	all := c.machinesOf("")
	var out []SignalCoverage
	if len(nv) == 0 {
		for _, sig := range accelSignals {
			r := SignalCoverage{Signal: sig, Status: CoverageNotApplicable, DeterminedBy: DeterminedByPresence,
				Note: "no accelerator nodes"}
			if len(all) > 0 {
				r = c.otherVendorRow(sig)
			}
			out = append(out, r)
		}
		return out
	}
	nvSet := setOf(nv)
	var sources []dcgmSource
	for _, comp := range c.present(KindDCGMExporter) {
		src := dcgmSource{comp: comp, parts: map[string][]string{}}
		for _, n := range NodeCoverage(c.s, comp) {
			if nvSet[n] {
				src.nodes = append(src.nodes, n)
			}
		}
		if len(src.nodes) == 0 {
			continue
		}
		if comp.Config != nil && comp.Config.DeterminedBy != DeterminedByPresence {
			src.known = true
			for _, sig := range accelSignals {
				src.parts[sig] = DCGMPartsPresent(sig, comp.Config.Fields)
			}
		}
		src.attr = settingValue(comp.Config, SettingKubernetesAttribution)
		src.attrBy = settingValue(comp.Config, SettingKubernetesAttribution+".determined-by")
		sources = append(sources, src)
	}
	for _, sig := range accelSignals {
		out = append(out, c.dcgmRow(sig, nv, sources))
	}
	return out
}

func (c *coverageCtx) dcgmRow(sig string, nv []string, sources []dcgmSource) SignalCoverage {
	r := SignalCoverage{Signal: sig, Vendor: VendorNVIDIA, NodesTotal: len(nv)}
	if sig == SignalAccelPodAttribution {
		r.Expected = []string{"pod-labels"}
	} else {
		r.Expected = DCGMPartNames(sig)
	}
	var comps []Component
	for _, s := range sources {
		comps = append(comps, s.comp)
	}
	r.Sources, r.Versions = refs(comps), versions(comps)
	if len(sources) == 0 {
		r.Status, r.DeterminedBy = CoverageMissing, DeterminedByPresence
		r.Note = "no DCGM exporter runs on the NVIDIA nodes"
		return r
	}
	nodeParts := map[string]map[string]bool{}
	unknownNodes := map[string]bool{}
	var by []string
	for _, s := range sources {
		var parts []string
		known := s.known
		if sig == SignalAccelPodAttribution {
			switch s.attr {
			case "true":
				parts = []string{"pod-labels"}
			case "false":
			default:
				known = false
			}
		} else {
			parts = s.parts[sig]
		}
		for _, n := range s.nodes {
			if !known {
				if nodeParts[n] == nil {
					unknownNodes[n] = true
				}
				continue
			}
			delete(unknownNodes, n)
			if nodeParts[n] == nil {
				nodeParts[n] = map[string]bool{}
			}
			for _, p := range parts {
				nodeParts[n][p] = true
			}
		}
		if known {
			if sig == SignalAccelPodAttribution {
				by = append(by, s.attrBy)
			} else {
				by = append(by, s.comp.Config.DeterminedBy)
			}
		}
	}
	present := map[string]bool{}
	covered := 0
	var perNode []string
	for _, n := range nv {
		ps := nodeParts[n]
		if len(ps) > 0 {
			covered++
			perNode = append(perNode, strings.Join(sortedKeys(ps), ","))
		}
		for p := range ps {
			present[p] = true
		}
	}
	for _, p := range r.Expected {
		if present[p] {
			r.Present = append(r.Present, p)
		}
	}
	r.NodesCovered = covered
	r.DeterminedBy = weakest(by...)
	switch {
	case len(nodeParts) == 0:
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByPresence
		r.Note = "the exporter's field set is not readable (a counters file discovery could not read, or not NVIDIA's dcgm-exporter image); --probe reads it from the endpoint"
		if sig == SignalAccelPodAttribution {
			r.Note = "whether the exporter adds pod labels is not readable; --probe reads its label keys"
		}
	case len(r.Present) == 0:
		r.Status = CoverageMissing
	case len(r.Present) == len(r.Expected) && covered == len(nv) && len(unknownNodes) == 0 && allSame(perNode):
		r.Status = CoverageCollected
	default:
		r.Status = CoveragePartial
	}
	if len(r.Present) > 0 && !allSame(perNode) {
		r.Note = "the exported parts differ between nodes"
	}
	if len(unknownNodes) > 0 && r.Status != CoverageUnknown {
		r.Note = joinNote(r.Note, fmt.Sprintf("%d node(s) run an exporter whose field set is not readable", len(unknownNodes)))
	}
	if r.DeterminedBy == DeterminedByVersionDefault && r.Note == "" {
		r.Note = "the field set is the found release's default counters file; a custom file would change it"
	}
	return r
}

func allSame(xs []string) bool {
	for _, x := range xs {
		if x != xs[0] {
			return false
		}
	}
	return true
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// otherVendorRow covers an accelerator signal on a cluster whose
// accelerators are not NVIDIA's.
func (c *coverageCtx) otherVendorRow(sig string) SignalCoverage {
	vendors := map[string]bool{}
	for _, m := range c.inv.Machines {
		vendors[m.Vendor] = true
	}
	vs := sortedKeys(vendors)
	r := SignalCoverage{Signal: sig, Vendor: strings.Join(vs, ","), Status: CoverageUnknown, DeterminedBy: DeterminedByPresence,
		Note: "only NVIDIA's DCGM field groups are modelled; " + strings.Join(vs, ", ") + " accelerators are inventoried at allocation level"}
	if sig != SignalAccelUtilisation && sig != SignalAccelMemory {
		return r
	}
	nodes := c.machinesOf("")
	vendorOf := map[string]string{}
	for _, m := range c.inv.Machines {
		vendorOf[m.Name] = m.Vendor
	}
	covered := map[string]bool{}
	var comps []Component
	for _, comp := range c.present(KindAMDMetricsExporter, KindNeuronMonitor, KindGaudiExporter) {
		comps = append(comps, comp)
		for _, n := range NodeCoverage(c.s, comp) {
			// An exporter covers only nodes of its own vendor.
			if vendorOf[n] == comp.Vendor {
				covered[n] = true
			}
		}
	}
	tpu := 0
	for _, m := range c.inv.Machines {
		if m.Vendor == VendorTPU {
			tpu++
		}
	}
	r.Sources, r.Versions = refs(comps), versions(comps)
	r.NodesTotal = len(nodes) - tpu
	r.NodesCovered = len(covered)
	if r.NodesTotal == 0 {
		r.Note = "TPU utilisation and memory are in Cloud Monitoring, not an in-cluster exporter"
		return r
	}
	r.Status = nodeStatus(r.NodesCovered, r.NodesTotal)
	r.Note = "a vendor exporter's presence on the node; its metric names are not modelled"
	if tpu > 0 {
		r.Note += "; TPU nodes report to Cloud Monitoring and are not counted"
	}
	return r
}

// --- node health -------------------------------------------------------

func (c *coverageCtx) nodeHealthRow() SignalCoverage {
	target := c.targetNodes()
	r := SignalCoverage{Signal: SignalNodeHealth, NodesTotal: len(target)}
	comps := c.present(KindNodeProblemDetector, KindEKSNodeMonitoring)
	r.Sources, r.Versions = refs(comps), versions(comps)
	targetSet := setOf(target)
	covered := map[string]bool{}
	seen := map[string]bool{}
	declared := map[string]bool{}
	for _, comp := range comps {
		if comp.Config != nil && comp.Config.DeterminedBy == DeterminedByConfig {
			for _, x := range comp.Config.Conditions {
				declared[x] = true
			}
		}
	}
	// Only conditions a health source sets count: NPD's defaults, what an
	// NPD config declares, and the EKS agent's.
	for _, n := range c.nodes {
		if !targetSet[n.Name] {
			continue
		}
		for _, cond := range n.Status.Conditions {
			t := string(cond.Type)
			if IsNPDDefaultCondition(t) || IsEKSAgentCondition(t) || declared[t] {
				seen[t] = true
				covered[n.Name] = true
			}
		}
	}
	var by []string
	for _, comp := range comps {
		for _, n := range NodeCoverage(c.s, comp) {
			if targetSet[n] && comp.Workload != "" {
				covered[n] = true
			}
		}
		if comp.Config != nil {
			by = append(by, comp.Config.DeterminedBy)
		}
	}
	r.Present = sortedKeysNil(seen)
	all := setOf(r.Present)
	for k := range declared {
		all[k] = true
	}
	r.Expected = sortedKeysNil(all)
	r.NodesCovered = len(covered)
	r.Status = nodeStatus(r.NodesCovered, r.NodesTotal)
	r.DeterminedBy = strongest(by...)
	if r.DeterminedBy == "" {
		r.DeterminedBy = DeterminedByPresence
	}
	if r.DeterminedBy == DeterminedByArgs {
		r.DeterminedBy = DeterminedByPresence
	}
	switch {
	case len(comps) == 0 && r.Status == CoverageMissing:
		r.Note = "no node-problem-detector, no EKS node monitoring agent, and no node condition a health source sets"
	case len(r.Expected) > len(r.Present):
		r.Note = "conditions the config declares that no node reports may mean the monitor is not running there"
	}
	return r
}

func (c *coverageCtx) gpuRulesRow() SignalCoverage {
	nv := c.machinesOf(VendorNVIDIA)
	r := SignalCoverage{Signal: SignalNodeHealthGPURules, Vendor: VendorNVIDIA, Expected: []string{"xid-rule"}, NodesTotal: len(nv)}
	if len(nv) == 0 {
		r.Vendor, r.Expected, r.Status, r.DeterminedBy = "", nil, CoverageNotApplicable, DeterminedByPresence
		r.Note = "no NVIDIA nodes (Xid is NVIDIA's error stream)"
		return r
	}
	nvSet := setOf(nv)
	covered, unknown := map[string]bool{}, map[string]bool{}
	var by []string
	var sources []Component
	// The EKS node monitoring agent's AcceleratedHardwareReady condition is
	// its documented GPU health check (Xid included): what the agent's
	// release does, not a rule discovery read.
	for _, comp := range c.present(KindEKSNodeMonitoring) {
		sources = append(sources, comp)
		for _, n := range c.nodes {
			for _, cond := range n.Status.Conditions {
				if nvSet[n.Name] && cond.Type == "AcceleratedHardwareReady" {
					covered[n.Name] = true
					by = append(by, DeterminedByVersionDefault)
				}
			}
		}
	}
	missingBy := ""
	for _, comp := range c.present(KindNodeProblemDetector) {
		gpu := settingValue(comp.Config, SettingGPURules)
		nodes := NodeCoverage(c.s, comp)
		if comp.Workload == "" {
			nodes = nv
		}
		switch gpu {
		case "true":
			sources = append(sources, comp)
			by = append(by, comp.Config.DeterminedBy)
			for _, n := range nodes {
				if nvSet[n] {
					covered[n] = true
				}
			}
		case "false":
			sources = append(sources, comp)
			missingBy = strongest(missingBy, comp.Config.DeterminedBy)
			if comp.Config.DeterminedBy == DeterminedByArgs {
				// Only the image's built-in monitors, none with an Xid rule.
				missingBy = strongest(missingBy, DeterminedByVersionDefault)
			}
		default:
			sources = append(sources, comp)
			for _, n := range nodes {
				if nvSet[n] {
					unknown[n] = true
				}
			}
		}
	}
	for n := range covered {
		delete(unknown, n)
	}
	r.Sources, r.Versions = refs(sources), versions(sources)
	r.NodesCovered = len(covered)
	if len(covered) > 0 {
		r.Present = []string{"xid-rule"}
	}
	r.Status = nodeStatus(len(covered), len(nv))
	r.DeterminedBy = weakest(by...)
	switch {
	case len(covered) == 0 && len(unknown) > 0:
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByUnreadable
		r.Note = "node-problem-detector runs but its monitor configs are not readable (managed, or a ConfigMap discovery did not read)"
		if c.inv.Cloud == CloudGKE {
			r.Note = "GKE runs node-problem-detector on the node; its rules are not readable from the API (GKE node auto-repair covers GPU driver faults)"
		}
	case len(covered) == 0:
		r.DeterminedBy = missingBy
		if r.DeterminedBy == "" {
			r.DeterminedBy = DeterminedByPresence
		}
		if missingBy != "" {
			r.Note = "node-problem-detector's monitors have no rule matching NVIDIA Xid / NVRM kernel lines"
		} else {
			r.Note = "no node health source turns GPU (Xid) errors into a node condition"
		}
	}
	return r
}

// --- control-plane tracing ---------------------------------------------

func (c *coverageCtx) managedNote() string {
	switch c.inv.Cloud {
	case CloudGKE:
		return "GKE"
	case CloudEKS:
		return "EKS"
	case CloudAKS:
		return "AKS"
	}
	return ""
}

func (c *coverageCtx) kubeletTracingRow() SignalCoverage {
	r := SignalCoverage{Signal: SignalKubeletTracing, Expected: []string{"tracing"}}
	if m := c.managedNote(); m != "" {
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByUnreadable
		r.Note = m + " manages the kubelet configuration; it is not readable from the Kubernetes API"
		return r
	}
	cm := configMap(c.s, NamespaceKubeSystem, ConfigMapKubeletConfig)
	if cm == nil {
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByUnreadable
		r.Note = "kube-system/kubelet-config was not read (not a kubeadm cluster, or the grant refused it); the kubelet's config file lives on each node, which discovery never reads"
		return r
	}
	for _, key := range cmKeys(cm) {
		t, ok := ParseKubeletTracing(cm.Data[key])
		if !ok {
			continue
		}
		r.DeterminedBy = DeterminedByConfig
		r.Sources = []string{cmSource(NamespaceKubeSystem, ConfigMapKubeletConfig, key)}
		if !t.Enabled {
			r.Status = CoverageMissing
			r.Note = "the kubeadm KubeletConfiguration has no tracing block"
			return r
		}
		r.Status, r.Present = CoverageCollected, []string{"tracing"}
		ep := "the default endpoint (localhost:4317)"
		if t.Endpoint != "" && t.Endpoint != "REDACTED" {
			ep = t.Endpoint
		}
		r.Note = "the kubeadm KubeletConfiguration exports spans to " + ep
		if t.SamplingRatePerMillion > 0 {
			r.Note += fmt.Sprintf(" at %d per million", t.SamplingRatePerMillion)
		}
		r.Note += "; a node whose local config differs is not visible"
		return r
	}
	r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByUnreadable
	r.Note = "kube-system/kubelet-config holds no KubeletConfiguration"
	return r
}

func (c *coverageCtx) apiserverTracingRow() SignalCoverage {
	r := SignalCoverage{Signal: SignalAPIServerTracing, Expected: []string{APIServerTracingFlag}}
	if m := c.managedNote(); m != "" {
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByUnreadable
		r.Note = m + " runs the control plane; its API server flags are not readable"
		return r
	}
	var cfgSays, argsSay *bool
	if cm := configMap(c.s, NamespaceKubeSystem, ConfigMapKubeadmConfig); cm != nil {
		for _, key := range cmKeys(cm) {
			if args, ok := ParseKubeadmAPIServerArgs(cm.Data[key]); ok {
				_, set := args[APIServerTracingFlag]
				cfgSays = &set
				r.Sources = append(r.Sources, cmSource(NamespaceKubeSystem, ConfigMapKubeadmConfig, key))
				break
			}
		}
	}
	pods := APIServerPods(c.s)
	if len(pods) > 0 {
		on := 0
		for _, p := range pods {
			for i := range p.Spec.Containers {
				if FlagSet(containerArgs(&p.Spec.Containers[i]), APIServerTracingFlag) {
					on++
					break
				}
			}
			r.Sources = append(r.Sources, "Pod/"+p.Namespace+"/"+p.Name)
		}
		set := on > 0
		argsSay = &set
		if set && on < len(pods) {
			r.Note = fmt.Sprintf("%d of %d API server pods set --%s", on, len(pods), APIServerTracingFlag)
		}
	}
	sort.Strings(r.Sources)
	switch {
	case argsSay != nil && *argsSay:
		r.Status, r.DeterminedBy, r.Present = CoverageCollected, DeterminedByArgs, r.Expected
	case cfgSays != nil && *cfgSays:
		r.Status, r.DeterminedBy, r.Present = CoverageCollected, DeterminedByConfig, r.Expected
	case cfgSays != nil:
		r.Status, r.DeterminedBy = CoverageMissing, DeterminedByConfig
	case argsSay != nil:
		r.Status, r.DeterminedBy = CoverageMissing, DeterminedByArgs
	default:
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByUnreadable
		r.Note = "no kube-apiserver mirror pod and no kubeadm-config (an embedded control plane such as k3s, or one outside the cluster)"
		return r
	}
	if r.Status == CoverageCollected {
		r.Note = joinNote(r.Note, "the tracing config file's endpoint lives on the control-plane node and is not read")
	}
	if argsSay != nil && cfgSays != nil && *argsSay != *cfgSays {
		r.Note = joinNote(r.Note, "the running API server and kubeadm-config disagree")
	}
	return r
}

// --- telemetry pipeline ------------------------------------------------

var collectorKinds = []string{KindOTelCollector, KindAlloy, KindDatadogAgent}

func (c *coverageCtx) collectorRow() SignalCoverage {
	comps := c.present(collectorKinds...)
	r := SignalCoverage{Signal: SignalCollector, Sources: refs(comps), Versions: versions(comps), DeterminedBy: DeterminedByPresence}
	if len(comps) == 0 {
		r.Status = CoverageMissing
		r.Note = "no OpenTelemetry Collector, Grafana Alloy or Datadog agent runs"
		return r
	}
	r.Status = CoverageCollected
	var kinds []string
	for _, comp := range comps {
		kinds = append(kinds, comp.Kind)
	}
	r.Present = uniqueSorted(kinds)
	return r
}

func (c *coverageCtx) telemetryRow(sig, part string) SignalCoverage {
	r := SignalCoverage{Signal: sig, Expected: []string{part}}
	comps := c.present(collectorKinds...)
	var providers, unknown []Component
	readable := true
	for _, comp := range comps {
		switch {
		case collectorProvides(comp.Config)[sig]:
			providers = append(providers, comp)
		case comp.Config == nil || comp.Config.DeterminedBy == DeterminedByPresence:
			unknown = append(unknown, comp)
			readable = false
		}
	}
	if sig == SignalOTLPReceiver {
		if n := c.s.CustomResources[CRDOTelInstrumentation] + len(c.s.CustomObjects[CRDOTelInstrumentation]); n > 0 {
			r.Note = fmt.Sprintf("%d OpenTelemetry Instrumentation object(s) inject OTLP SDKs into workloads", n)
		}
	}
	switch {
	case len(providers) > 0:
		r.Status, r.Present = CoverageCollected, r.Expected
		r.Sources, r.Versions = refs(providers), versions(providers)
		var by []string
		for _, p := range providers {
			by = append(by, p.Config.DeterminedBy)
		}
		r.DeterminedBy = strongest(by...)
		if sig == SignalKubeletStats || sig == SignalContainerLogs {
			c.nodeScope(&r, providers)
		}
	case len(unknown) > 0:
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByPresence
		r.Sources, r.Versions = refs(unknown), versions(unknown)
		r.Note = joinNote(r.Note, "a collector runs whose config discovery could not read")
	case len(comps) > 0 && readable:
		var by []string
		for _, comp := range comps {
			by = append(by, comp.Config.DeterminedBy)
		}
		r.Status, r.DeterminedBy = CoverageMissing, weakest(by...)
		r.Sources = refs(comps)
		r.Note = joinNote(r.Note, "no collector config has a "+part+" receiver in a pipeline")
	default:
		r.Status, r.DeterminedBy = CoverageMissing, DeterminedByPresence
		r.Note = joinNote(r.Note, "no collector runs")
	}
	return r
}

// nodeScope sets node coverage for a signal DaemonSet collectors provide.
func (c *coverageCtx) nodeScope(r *SignalCoverage, providers []Component) {
	covered := map[string]bool{}
	for _, p := range providers {
		if !strings.HasPrefix(p.Workload, WorkloadDaemonSet+"/") {
			return // a Deployment reaches every node through the API
		}
		for _, n := range NodeCoverage(c.s, p) {
			covered[n] = true
		}
	}
	r.NodesTotal = len(c.nodes)
	r.NodesCovered = len(covered)
	r.Status = nodeStatus(r.NodesCovered, r.NodesTotal)
}

// --- profiling and workload tracing ------------------------------------

var profilerKinds = []string{KindParcaAgent, KindOTelEBPFProfiler, KindElasticProfiling, KindCorootNodeAgent, KindPixie}

func (c *coverageCtx) profilingRow() SignalCoverage {
	target := c.targetNodes()
	targetSet := setOf(target)
	r := SignalCoverage{Signal: SignalContinuousProfiling, NodesTotal: len(target)}
	var unknown []Component
	providers := c.present(profilerKinds...)
	for _, comp := range c.present(KindAlloy, KindOTelCollector) {
		switch {
		case collectorProvides(comp.Config)[SignalContinuousProfiling]:
			providers = append(providers, comp)
		case comp.Config == nil || comp.Config.DeterminedBy == DeterminedByPresence:
			if comp.Kind == KindAlloy {
				unknown = append(unknown, comp)
			}
		}
	}
	servers := c.present(KindPyroscope)
	covered := map[string]bool{}
	var by []string
	for _, p := range providers {
		for _, n := range NodeCoverage(c.s, p) {
			if targetSet[n] {
				covered[n] = true
			}
		}
		if p.Config != nil && p.Config.DeterminedBy != DeterminedByPresence {
			by = append(by, p.Config.DeterminedBy)
		} else {
			by = append(by, DeterminedByPresence)
		}
	}
	r.Sources, r.Versions = refs(append(append([]Component(nil), providers...), servers...)), versions(append(append([]Component(nil), providers...), servers...))
	r.NodesCovered = len(covered)
	switch {
	case len(providers) > 0:
		r.Status = nodeStatus(len(covered), len(target))
		r.DeterminedBy = strongest(by...)
	case len(unknown) > 0 || len(servers) > 0:
		r.Status, r.DeterminedBy = CoverageUnknown, DeterminedByPresence
		r.Sources = refs(append(unknown, servers...))
		if len(servers) > 0 {
			r.Note = "a Pyroscope server runs; agents or SDKs that push to it are not visible from the API"
		} else {
			r.Note = "Grafana Alloy runs but its config (where pyroscope.ebpf would be) was not read"
		}
	default:
		r.Status, r.DeterminedBy = CoverageMissing, DeterminedByPresence
		r.Note = "no continuous profiler (Parca, Pyroscope via Alloy, OTel eBPF profiler, Elastic, Coroot, Pixie) runs"
	}
	return r
}

func (c *coverageCtx) gpuProfilingRow() SignalCoverage {
	r := SignalCoverage{Signal: SignalGPUProfiling, DeterminedBy: DeterminedByPresence}
	if len(c.inv.Machines) == 0 {
		r.Status, r.Note = CoverageNotApplicable, "no accelerator nodes"
		return r
	}
	r.Status = CoverageUnknown
	r.Note = "CUDA, Nsight and PyTorch profiler hooks live inside workloads; discovery recognises no GPU profiling operator"
	return r
}

// otelEnv are the env var names that make an SDK export traces.
var otelEnv = []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_TRACES_EXPORTER"}

func hasOTelEnv(spec *corev1.PodSpec) bool {
	for i := range spec.Containers {
		names := envNames(&spec.Containers[i])
		for _, n := range otelEnv {
			if names[n] {
				return true
			}
		}
	}
	return false
}

func jobDone(conds []batchv1.JobCondition) bool {
	for _, c := range conds {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (c *coverageCtx) trainingTracingRow() SignalCoverage {
	r := SignalCoverage{Signal: SignalTrainingTracing, DeterminedBy: DeterminedByArgs}
	jobs := make([]*batchv1.Job, 0, len(c.s.Jobs))
	for i := range c.s.Jobs {
		j := &c.s.Jobs[i]
		if !jobDone(j.Status.Conditions) && len(PodSpecAccelerators(&j.Spec.Template.Spec)) > 0 {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Namespace+"/"+jobs[a].Name < jobs[b].Namespace+"/"+jobs[b].Name })
	if len(jobs) == 0 {
		r.Status, r.DeterminedBy, r.Note = CoverageNotApplicable, DeterminedByPresence, "no unfinished accelerator Jobs"
		return r
	}
	for _, j := range jobs {
		ref := "Job/" + j.Namespace + "/" + j.Name
		r.Expected = append(r.Expected, ref)
		if hasOTelEnv(&j.Spec.Template.Spec) {
			r.Present = append(r.Present, ref)
		}
	}
	r.Status = partsStatus(len(r.Present), len(r.Expected))
	r.Note = "OTEL_EXPORTER_OTLP_* / OTEL_TRACES_EXPORTER env names on the Job's containers; an Instrumentation injection annotation is not visible"
	return r
}

func partsStatus(present, expected int) string {
	switch {
	case expected == 0:
		return CoverageNotApplicable
	case present == 0:
		return CoverageMissing
	case present < expected:
		return CoveragePartial
	}
	return CoverageCollected
}

func (c *coverageCtx) inferenceTracingRow() SignalCoverage {
	r := SignalCoverage{Signal: SignalInferenceTracing, DeterminedBy: DeterminedByArgs}
	servers := c.present(KindVLLM, KindTriton)
	if len(servers) == 0 {
		r.Status, r.DeterminedBy, r.Note = CoverageNotApplicable, DeterminedByPresence, "no vLLM or Triton servers"
		return r
	}
	for _, comp := range servers {
		ref := ComponentRef(comp)
		r.Expected = append(r.Expected, ref)
		spec, _ := podTemplate(c.s, &comp)
		if spec != nil && serverTraces(comp.Kind, spec) {
			r.Present = append(r.Present, ref)
		}
	}
	sort.Strings(r.Expected)
	sort.Strings(r.Present)
	r.Sources, r.Versions = refs(servers), versions(servers)
	r.Status = partsStatus(len(r.Present), len(r.Expected))
	return r
}

// serverTraces reports whether a model server exports traces: vLLM's
// --otlp-traces-endpoint, Triton's --trace-config, or OTLP env names.
func serverTraces(kind string, spec *corev1.PodSpec) bool {
	if hasOTelEnv(spec) {
		return true
	}
	for i := range spec.Containers {
		args := containerArgs(&spec.Containers[i])
		switch kind {
		case KindVLLM:
			if FlagSet(args, "otlp-traces-endpoint") {
				return true
			}
		case KindTriton:
			if FlagSet(args, "trace-config") {
				return true
			}
		}
	}
	return false
}

func (c *coverageCtx) inferenceMetricsRow() SignalCoverage {
	r := SignalCoverage{Signal: SignalInferenceMetrics, DeterminedBy: DeterminedByPresence}
	servers := c.present(KindVLLM, KindTriton)
	if len(servers) == 0 {
		r.Status, r.Note = CoverageNotApplicable, "no vLLM or Triton servers"
		return r
	}
	r.Sources, r.Versions = refs(servers), versions(servers)
	scrapers := c.present(KindPrometheusOperator, KindGMP, KindAlloy, KindDatadogAgent)
	for _, comp := range c.present(KindOTelCollector) {
		if comp.Config == nil || contains(comp.Config.Receivers, "prometheus") || comp.Config.DeterminedBy == DeterminedByPresence {
			scrapers = append(scrapers, comp)
		}
	}
	if len(scrapers) == 0 {
		r.Status = CoverageMissing
		r.Note = "nothing in the cluster scrapes Prometheus metrics"
		return r
	}
	r.Status = CoverageUnknown
	r.Note = "a scraper runs (" + strings.Join(refs(scrapers), ", ") + "); whether a scrape config or PodMonitor selects the servers is not read"
	return r
}
