package mlrecs

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// How an env's main.k uses the ml.k Propose writes. Every proposal's header
// states it; it is kept here, once, so a change of idiom is one edit.
const (
	MainKImport      = "import .ml"
	MainKBundleField = "ml_stacks = ml.ml_stacks"
)

// DefaultNamespace is where forge.Install charts render by default.
const DefaultNamespace = "forge-ml"

// metricAllow is the allow list Propose writes for each exporter: the
// metric families the Machines view and the recommendation rules read. It
// is data in the user's ml.k, not a preset in forge's KCL library. DCGM
// names are from dcgm-exporter's default and profiling counters (both
// clock-reason spellings, DCGM 3.3 renamed them); the other vendors' names
// are from their exporters' documentation and unverified on a live node.
var metricAllow = map[string][]string{
	mldiscover.KindDCGMExporter: {
		"DCGM_FI_DEV_GPU_UTIL",
		"DCGM_FI_PROF_GR_ENGINE_ACTIVE",
		"DCGM_FI_PROF_SM_ACTIVE",
		"DCGM_FI_PROF_SM_OCCUPANCY",
		"DCGM_FI_PROF_PIPE_TENSOR_ACTIVE",
		"DCGM_FI_PROF_DRAM_ACTIVE",
		"DCGM_FI_DEV_FB_USED",
		"DCGM_FI_DEV_FB_FREE",
		"DCGM_FI_DEV_GPU_TEMP",
		"DCGM_FI_DEV_POWER_USAGE",
		"DCGM_FI_DEV_XID_ERRORS",
		"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL",
		"DCGM_FI_DEV_ROW_REMAP_FAILURE",
		"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS",
		"DCGM_FI_DEV_PCIE_REPLAY_COUNTER",
		"DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL",
		"DCGM_FI_DEV_CLOCK_THROTTLE_REASONS",
		"DCGM_FI_DEV_CLOCKS_EVENT_REASONS",
	},
	mldiscover.KindAMDMetricsExporter: {
		"gpu_gfx_activity", "gpu_used_vram", "gpu_total_vram", "gpu_power_usage",
		"gpu_junction_temperature", "gpu_ecc_uncorrect_total",
	},
	mldiscover.KindNeuronMonitor: {
		"neuroncore_utilization_ratio", "neuron_runtime_memory_used_bytes",
		"neuron_hardware_ecc_events_total", "execution_errors_total",
	},
	mldiscover.KindGaudiExporter: {
		"habanalabs_utilization", "habanalabs_memory_used_bytes",
		"habanalabs_temperature_onchip", "habanalabs_power_mW",
	},
}

// unverifiedAllow marks the allow lists whose names were not checked
// against a live exporter.
var unverifiedAllow = map[string]bool{
	mldiscover.KindAMDMetricsExporter: true, mldiscover.KindNeuronMonitor: true, mldiscover.KindGaudiExporter: true,
}

// Stack slots.
const (
	slotMetrics    = "metrics"
	slotNodeHealth = "node_health"
	slotQueue      = "queue"
	slotTraining   = "training"
	slotServing    = "serving"
	slotMonitoring = "monitoring"
	slotPullCache  = "pull_cache"
)

// slotKinds says which component kinds fill each slot, in preference
// order. Kinds in no slot (operators, device plugins, autoscalers, model
// servers) are only listed in `observed`.
var slotKinds = map[string][]string{
	slotNodeHealth: {mldiscover.KindNodeProblemDetector, mldiscover.KindEKSNodeMonitoring},
	slotQueue:      {mldiscover.KindKueue, mldiscover.KindVolcano},
	slotTraining:   {mldiscover.KindJobSet, mldiscover.KindKubeflowTrainer, mldiscover.KindKubeflowTraining, mldiscover.KindLWS, mldiscover.KindKubeRay},
	slotServing:    {mldiscover.KindKServe, mldiscover.KindInferenceExtension},
	slotMonitoring: {mldiscover.KindPrometheusOperator, mldiscover.KindGMP, mldiscover.KindKubeStateMetrics, mldiscover.KindOTelCollector},
	slotPullCache:  {mldiscover.KindSpegel, mldiscover.KindDragonfly},
}

// suggestion is an install a FixKCL finding proposes. One install may be
// suggested by several findings (three DCGM field-set findings all
// reconfigure the one exporter); findingIDs keeps every one of them, so
// applying any of them activates it.
type suggestion struct {
	findingIDs []string
	slot       string
	vendor     string // for slotMetrics
	kind       string
	// reconfigure: the finding changes the values of a component forge
	// already installed instead of adding one.
	reconfigure bool
}

// suggestionsFor derives a finding's installs from its id, vendor and fix
// kind alone, so findings read back from JSON propose the same file.
func suggestionsFor(f Finding, inv *mldiscover.Inventory) []suggestion {
	if f.Fix.Kind != FixKCL || f.Dismissed != nil {
		return nil
	}
	switch f.ID {
	case "accel-telemetry-missing":
		if f.Vendor == mldiscover.VendorNVIDIA {
			return []suggestion{{[]string{f.ID}, slotMetrics, mldiscover.VendorNVIDIA, mldiscover.KindDCGMExporter, false}}
		}
	case "no-node-health":
		return []suggestion{{[]string{f.ID}, slotNodeHealth, "", mldiscover.KindNodeProblemDetector, false}}
	case "gpu-jobs-bypass-queue":
		out := []suggestion{{[]string{f.ID}, slotQueue, "", mldiscover.KindKueue, false}}
		if !inv.Has(mldiscover.KindJobSet) {
			out = append(out, suggestion{[]string{f.ID}, slotTraining, "", mldiscover.KindJobSet, false})
		}
		return out
	case "no-p2p-cache":
		return []suggestion{{[]string{f.ID}, slotPullCache, "", mldiscover.KindSpegel, false}}
	case "dcgm-profiling-off", "accel-error-counters-off", "dcgm-nvlink-off", "dcgm-pod-attribution-off":
		return []suggestion{{[]string{f.ID}, slotMetrics, mldiscover.VendorNVIDIA, mldiscover.KindDCGMExporter, true}}
	case "npd-no-gpu-rules":
		return []suggestion{{[]string{f.ID}, slotNodeHealth, "", mldiscover.KindNodeProblemDetector, true}}
	}
	return nil
}

// Propose writes the env's ml.k for an inventory: one forge.MLStack for the
// cluster, every found component adopted, each suggested install written but
// commented out, and `observed` listing what discovery found running so the
// KCL refuses installing a duplicate. The output is deterministic, valid KCL
// that `import forge` resolves.
func Propose(inv *mldiscover.Inventory, findings []Finding, opts ProposeOptions) string {
	return propose(inv, findings, opts, nil)
}

// kclDiff is the unified diff that applies finding id's suggested installs
// to the ml.k Propose writes.
func kclDiff(inv *mldiscover.Inventory, findings []Finding, opts ProposeOptions, id string) string {
	base := propose(inv, findings, opts, nil)
	applied := propose(inv, findings, opts, map[string]bool{id: true})
	if base == applied {
		return ""
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(base), B: difflib.SplitLines(applied),
		FromFile: "a/ml.k", ToFile: "b/ml.k", Context: 3,
	})
	if err != nil {
		return ""
	}
	return diff
}

// proposer renders one ml.k.
type proposer struct {
	inv    *mldiscover.Inventory
	opts   ProposeOptions
	active map[string]bool // finding ids whose suggestions are written uncommented
	sugg   []suggestion
	b      strings.Builder
}

func propose(inv *mldiscover.Inventory, findings []Finding, opts ProposeOptions, active map[string]bool) string {
	if opts.Namespace == "" {
		opts.Namespace = DefaultNamespace
	}
	p := &proposer{inv: inv, opts: opts, active: active}
	seen := map[string]int{} // dedup key -> index in p.sugg
	for _, f := range findings {
		for _, sg := range suggestionsFor(f, inv) {
			k := sg.slot + "/" + sg.vendor + "/" + sg.kind + "/" + strconv.FormatBool(sg.reconfigure)
			i, ok := seen[k]
			if !ok {
				seen[k] = len(p.sugg)
				p.sugg = append(p.sugg, sg)
				continue
			}
			if !contains(p.sugg[i].findingIDs, f.ID) {
				p.sugg[i].findingIDs = append(p.sugg[i].findingIDs, f.ID)
			}
		}
	}
	for i := range p.sugg {
		sort.Strings(p.sugg[i].findingIDs)
	}
	sort.SliceStable(p.sugg, func(i, j int) bool { return p.sugg[i].findingIDs[0] < p.sugg[j].findingIDs[0] })
	p.render()
	return p.b.String()
}

func (p *proposer) date() string {
	d := p.opts.Date
	if d.IsZero() {
		d = p.inv.CollectedAt
	}
	return d.UTC().Format(dateLayout)
}

// line writes s at indent (in 4-space steps), commented when comment is set.
func (p *proposer) line(indent int, comment bool, s string) {
	pad := strings.Repeat("    ", indent)
	switch {
	case comment && s == "":
		p.b.WriteString(pad + "#\n")
	case comment:
		p.b.WriteString(pad + "# " + s + "\n")
	case s == "":
		p.b.WriteString("\n")
	default:
		p.b.WriteString(pad + s + "\n")
	}
}

func (p *proposer) lines(indent int, comment bool, ls []string) {
	for _, l := range ls {
		p.line(indent, comment, l)
	}
}

func (p *proposer) render() {
	date := p.date()
	p.line(0, true, "ml.k: the ML stack of this env's cluster.")
	p.line(0, true, "")
	p.line(0, true, fmt.Sprintf("Generated by `forge ml discover` on %s from kube context %s.", date, strconv.Quote(p.inv.Context)))
	p.line(0, true, "This file is yours to edit. It adopts what discovery found running (forge never")
	p.line(0, true, "upgrades, reconfigures or removes what it did not install) and writes each")
	p.line(0, true, "suggested install commented out, under the id of the finding that suggests it.")
	p.line(0, true, "")
	p.line(0, true, "Wire it into this env's main.k:")
	p.line(0, true, "")
	p.line(0, true, "    "+MainKImport)
	p.line(0, true, "")
	p.line(0, true, "    forge.Bundle {")
	p.line(0, true, "        "+MainKBundleField)
	p.line(0, true, "    }")
	p.line(0, false, "import forge")
	p.line(0, false, "")
	p.line(0, false, "ml_stacks = [forge.MLStack {")
	target := fmt.Sprintf("cluster = %s, namespace = %s", strconv.Quote(p.inv.Context), strconv.Quote(p.opts.Namespace))
	if p.opts.ConnectedCluster != "" {
		target += ", connected_cluster = " + strconv.Quote(p.opts.ConnectedCluster)
	}
	p.line(1, false, "cluster = forge.ClusterTarget {"+target+"}")
	p.renderObserved(date)
	p.renderAccelerators()
	p.renderSingle(slotNodeHealth)
	p.renderSingle(slotQueue)
	p.renderList(slotTraining)
	p.renderList(slotServing)
	p.renderList(slotMonitoring)
	p.renderSingle(slotPullCache)
	p.renderNotes()
	p.renderDecisions()
	p.line(0, false, "}]")
}

func (p *proposer) renderObserved(date string) {
	set := map[string]bool{}
	for _, c := range p.inv.Components {
		if c.Status == mldiscover.StatusPresent && c.Owner == mldiscover.OwnerTheirs {
			set[c.Kind] = true
		}
	}
	p.line(1, true, "What discovery found running that forge did not install: the KCL refuses")
	p.line(1, true, "a forge.Install of any of these kinds.")
	if len(set) == 0 {
		p.line(1, false, "observed = []")
	} else {
		p.line(1, false, "observed = [")
		for _, k := range sortedSet(set) {
			p.line(2, false, strconv.Quote(k))
		}
		p.line(1, false, "]")
	}
	p.line(1, false, "observed_at = "+strconv.Quote(date))
}

// pick returns the component a slot adopts or keeps for kinds: theirs first
// (never install beside theirs), a managed one (Via) before a standalone
// one, then forge's own install.
func (p *proposer) pick(kinds []string, vendor string) (mldiscover.Component, bool) {
	var cands []mldiscover.Component
	for _, c := range p.inv.Components {
		if c.Status != mldiscover.StatusPresent || !contains(kinds, c.Kind) || (vendor != "" && c.Vendor != vendor) ||
			c.Via == mldiscover.ViaForgePlatform {
			continue
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return mldiscover.Component{}, false
	}
	kindRank := func(k string) int {
		for i, kk := range kinds {
			if kk == k {
				return i
			}
		}
		return len(kinds)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.Owner == mldiscover.OwnerTheirs) != (b.Owner == mldiscover.OwnerTheirs) {
			return a.Owner == mldiscover.OwnerTheirs
		}
		if (a.Via != "") != (b.Via != "") {
			return a.Via != ""
		}
		if kindRank(a.Kind) != kindRank(b.Kind) {
			return kindRank(a.Kind) < kindRank(b.Kind)
		}
		return a.Namespace+"/"+a.Workload < b.Namespace+"/"+b.Workload
	})
	return cands[0], true
}

// component renders a found component: forge.Adopt for theirs, forge.Install
// again for forge's own (so re-running discover keeps it).
func (p *proposer) component(c mldiscover.Component, scrape bool) []string {
	if c.Owner == mldiscover.OwnerForge {
		if chart, ok := ChartFor(c.Kind); ok {
			if c.ChartVersion != "" {
				chart.Version = c.ChartVersion
			}
			ns := c.Namespace
			if ns == "" {
				ns = p.opts.Namespace
			}
			return p.install(chart, ns, scrape, p.fullValues(c.Kind))
		}
	}
	return p.adopt(c, scrape)
}

func (p *proposer) adopt(c mldiscover.Component, scrape bool) []string {
	fields := [][2]string{{"kind", strconv.Quote(c.Kind)}}
	if c.Namespace != "" {
		fields = append(fields, [2]string{"namespace", strconv.Quote(c.Namespace)})
	}
	if c.Version != "" {
		fields = append(fields, [2]string{"version", strconv.Quote(c.Version)})
	}
	note := ""
	switch {
	case c.Owner == mldiscover.OwnerForge:
		note = "installed by forge; no pinned chart for this kind"
	case c.Via == mldiscover.ViaGPUOperator:
		note = "managed by the GPU Operator"
	case c.Via == mldiscover.ViaGKEManaged:
		note = "managed by GKE"
	case c.Via != "":
		note = "managed by " + c.Via
	}
	if note != "" {
		fields = append(fields, [2]string{"note", strconv.Quote(note)})
	}
	if !scrape || len(c.Selector) == 0 || c.MetricsPort == 0 {
		parts := make([]string, len(fields))
		for i, f := range fields {
			parts[i] = f[0] + " = " + f[1]
		}
		return []string{"forge.Adopt {" + strings.Join(parts, ", ") + "}"}
	}
	out := []string{"forge.Adopt {"}
	for _, f := range fields {
		out = append(out, "    "+f[0]+" = "+f[1])
	}
	out = append(out, "    selector = "+kclStringMap(c.Selector), "    port = "+strconv.Itoa(int(c.MetricsPort)))
	allow, verified := metricAllow[c.Kind], !unverifiedAllow[c.Kind]
	if c.Config != nil && c.Config.DeterminedBy != mldiscover.DeterminedByPresence && len(c.Config.Fields) > 0 {
		allow, verified = c.Config.Fields, true
		out = append(out, "    # The fields discovery found it exporting ("+c.Config.DeterminedBy+": "+c.Config.Source+").")
	}
	out = append(out, indentLines(allowLines(allow, verified), "    ")...)
	return append(out, "}")
}

func allowLines(allow []string, verified bool) []string {
	if len(allow) == 0 {
		return nil
	}
	var out []string
	if !verified {
		out = append(out, "# Metric names from the exporter's docs, unverified: check its /metrics.")
	}
	out = append(out, "allow = [")
	for _, m := range allow {
		out = append(out, "    "+strconv.Quote(m))
	}
	return append(out, "]")
}

// install renders a forge.Install of a pinned chart. full writes the
// chart values forge installs with; a found forge-owned component that a
// reconfigure finding says lacks them is written without until the
// finding's diff applies.
func (p *proposer) install(chart Chart, namespace string, scrape, full bool) []string {
	out := []string{"forge.Install {", "    kind = " + strconv.Quote(chart.Kind)}
	if chart.Delivery == DeliveryBootstrap {
		out = append(out, fmt.Sprintf("    # It ships CRDs, so it is a bootstrap chart: apply it with `forge env deploy <env> --target %s`.", chart.Name))
	}
	out = append(out, "    chart = forge.HelmChart {")
	field := func(k, v string) { out = append(out, "        "+k+" = "+v) }
	field("name", strconv.Quote(chart.Name))
	if chart.OCI != "" {
		field("oci", strconv.Quote(chart.OCI))
	} else {
		field("repo", strconv.Quote(chart.Repo))
		field("chart", strconv.Quote(chart.Chart))
	}
	field("version", strconv.Quote(chart.Version))
	field("namespace", strconv.Quote(namespace))
	field("delivery", strconv.Quote(chart.Delivery))
	if values := p.values(chart.Kind, full); len(values) > 0 {
		v := kclValue(values, "        ")
		out = append(out, "        values = "+v[0])
		out = append(out, v[1:]...)
	}
	out = append(out, "    }")
	if scrape && chart.Kind == mldiscover.KindDCGMExporter {
		out = append(out, "    selector = "+kclStringMap(map[string]string{"app.kubernetes.io/name": "dcgm-exporter"}),
			"    port = "+strconv.Itoa(int(mldiscover.DefaultMetricsPort(mldiscover.KindDCGMExporter))))
		var allow []string
		for _, f := range mldiscover.ForgeDCGMFields {
			allow = append(allow, f.Name)
		}
		out = append(out, indentLines(allowLines(allow, true), "    ")...)
	}
	return append(out, "}")
}

// isActive reports whether any finding that suggests s is being applied.
func (p *proposer) isActive(s suggestion) bool {
	for _, id := range s.findingIDs {
		if p.active[id] {
			return true
		}
	}
	return false
}

// reconfigureFor returns the reconfigure suggestions for a kind.
func (p *proposer) reconfigureFor(kind string) []suggestion {
	var out []suggestion
	for _, s := range p.sugg {
		if s.reconfigure && s.kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// fullValues reports whether a found forge-owned component is written with
// forge's full values: yes unless a reconfigure finding says it lacks them
// and that finding is not the one being applied.
func (p *proposer) fullValues(kind string) bool {
	rs := p.reconfigureFor(kind)
	if len(rs) == 0 {
		return true
	}
	for _, s := range rs {
		if p.isActive(s) {
			return true
		}
	}
	return false
}

// values is a chart's helm values for this cluster.
func (p *proposer) values(kind string, full bool) map[string]any {
	v := chartValues(kind, full)
	if kind != mldiscover.KindDCGMExporter {
		return v
	}
	// Keep the exporter off nodes without NVIDIA GPUs.
	if expr := p.nvidiaNodeExpr(); expr != nil {
		v["affinity"] = map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
			"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{expr}}},
		}}}
	}
	return v
}

func (p *proposer) nvidiaNodeExpr() map[string]any {
	if p.inv.Cloud == mldiscover.CloudGKE {
		return map[string]any{"key": mldiscover.LabelGKEAccelerator, "operator": "Exists"}
	}
	types := map[string]bool{}
	for _, m := range p.inv.Machines {
		if m.Vendor != mldiscover.VendorNVIDIA {
			continue
		}
		if m.InstanceType == "" {
			return map[string]any{"key": mldiscover.LabelGFDPresent, "operator": "In", "values": []any{"true"}}
		}
		types[m.InstanceType] = true
	}
	if len(types) == 0 {
		return nil
	}
	vals := []any{}
	for _, t := range sortedSet(types) {
		vals = append(vals, t)
	}
	return map[string]any{"key": mldiscover.LabelInstanceType, "operator": "In", "values": vals}
}

func (p *proposer) suggestionsIn(slot, vendor string) []suggestion {
	var out []suggestion
	for _, s := range p.sugg {
		if s.slot == slot && s.vendor == vendor {
			out = append(out, s)
		}
	}
	return out
}

// emitSuggestion writes a suggested install: active when its finding is
// the one being applied, commented out otherwise.
func (p *proposer) emitSuggestion(indent int, s suggestion, prefix string) {
	chart, ok := ChartFor(s.kind)
	if !ok {
		return
	}
	on := p.isActive(s)
	if !on {
		what := "finding " + s.findingIDs[0]
		if len(s.findingIDs) > 1 {
			what = "findings " + strings.Join(s.findingIDs, ", ")
		}
		p.line(indent, true, fmt.Sprintf("Suggested for %s: uncomment to install.", what))
	}
	ls := p.install(chart, p.opts.Namespace, true, true)
	ls[0] = prefix + ls[0]
	p.lines(indent, !on, ls)
}

func (p *proposer) renderSingle(slot string) {
	if c, ok := p.pick(slotKinds[slot], ""); ok {
		ls := p.component(c, false)
		ls[0] = slot + " = " + ls[0]
		p.lines(1, false, ls)
		return
	}
	for _, s := range p.suggestionsIn(slot, "") {
		if s.reconfigure {
			continue
		}
		p.emitSuggestion(1, s, slot+" = ")
		return // one component per slot
	}
}

func (p *proposer) renderList(slot string) {
	var entries [][]string
	for _, kind := range slotKinds[slot] {
		if c, ok := p.pick([]string{kind}, ""); ok {
			entries = append(entries, p.component(c, false))
		}
	}
	var sugg []suggestion
	for _, s := range p.suggestionsIn(slot, "") {
		if !s.reconfigure {
			sugg = append(sugg, s)
		}
	}
	if len(entries) == 0 && len(sugg) == 0 {
		return
	}
	if len(sugg) == 0 && len(entries) == 1 && len(entries[0]) == 1 {
		p.line(1, false, slot+" = ["+entries[0][0]+"]")
		return
	}
	p.line(1, false, slot+" = [")
	for _, e := range entries {
		p.lines(2, false, e)
	}
	for _, s := range sugg {
		if !s.reconfigure {
			p.emitSuggestion(2, s, "")
		}
	}
	p.line(1, false, "]")
}

func (p *proposer) renderAccelerators() {
	byVendor := map[string][]mldiscover.AcceleratorGroup{}
	var vendors []string
	for _, g := range p.inv.Accelerators {
		if _, ok := byVendor[g.Vendor]; !ok {
			vendors = append(vendors, g.Vendor)
		}
		byVendor[g.Vendor] = append(byVendor[g.Vendor], g)
	}
	sort.SliceStable(vendors, func(i, j int) bool { return mldiscover.VendorRank(vendors[i]) < mldiscover.VendorRank(vendors[j]) })
	if len(vendors) == 0 {
		p.line(1, true, "No accelerators found: no node has accelerator capacity or labels.")
		p.line(1, false, "accelerators = []")
		return
	}
	p.line(1, false, "accelerators = [")
	for _, v := range vendors {
		groups := byVendor[v]
		for _, g := range groups {
			p.line(2, true, groupComment(g))
		}
		p.line(2, false, "forge.Accelerators {")
		p.line(3, false, "vendor = "+strconv.Quote(v))
		resource := groups[0].Resource
		if v == mldiscover.VendorGeneric {
			var others []string
			for _, g := range groups[1:] {
				if g.Resource != resource && !contains(others, g.Resource) {
					others = append(others, g.Resource)
				}
			}
			if len(others) > 0 {
				p.line(3, true, "One generic resource per stack: also found "+strings.Join(others, ", ")+".")
			}
		}
		if resource != mldiscover.DefaultResource(v) && !strings.HasPrefix(resource, mldiscover.ResourceNVIDIAMIGPrefix) {
			p.line(3, false, "resource = "+strconv.Quote(resource))
		}
		if v == mldiscover.VendorTPU {
			p.line(3, true, "TPU metrics are in Cloud Monitoring, not an in-cluster exporter.")
		}
		if c, ok := p.pick(metricsKinds(v), v); ok {
			for _, other := range p.inv.Components {
				if other.Status == mldiscover.StatusPresent && contains(metricsKinds(v), other.Kind) &&
					(other.Namespace != c.Namespace || other.Workload != c.Workload) {
					p.line(3, true, "Also running: "+componentRef(other)+" (see finding accel-exporter-duplicate).")
				}
			}
			ls := p.component(c, true)
			ls[0] = "metrics = " + ls[0]
			p.lines(3, false, ls)
		} else {
			for _, s := range p.suggestionsIn(slotMetrics, v) {
				if s.reconfigure {
					continue
				}
				p.emitSuggestion(3, s, "metrics = ")
				break
			}
		}
		p.line(2, false, "}")
	}
	p.line(1, false, "]")
}

func metricsKinds(vendor string) []string {
	switch vendor {
	case mldiscover.VendorNVIDIA:
		return []string{mldiscover.KindDCGMExporter}
	case mldiscover.VendorAMD:
		return []string{mldiscover.KindAMDMetricsExporter}
	case mldiscover.VendorNeuron:
		return []string{mldiscover.KindNeuronMonitor}
	case mldiscover.VendorGaudi:
		return []string{mldiscover.KindGaudiExporter}
	}
	return nil
}

func groupComment(g mldiscover.AcceleratorGroup) string {
	what := g.Product
	if what == "" {
		what = "device"
	}
	s := fmt.Sprintf("%d × %s (%s) on %s", g.Capacity, what, g.Resource, plural(g.Nodes, "node", "nodes"))
	if g.Pool != "" {
		s += ", pool " + g.Pool
	}
	if g.Sharing != "" {
		s += ", sharing " + g.Sharing
	}
	if g.DetectedBy == mldiscover.DetectedByLabel {
		s += ": labelled, but no capacity yet (device plugin missing or not ready)"
	}
	return s
}

func (p *proposer) renderNotes() {
	var crdOnly, servers []string
	for _, c := range p.inv.Components {
		switch {
		case c.Status == mldiscover.StatusCRDOnly:
			crdOnly = append(crdOnly, c.Kind)
		case c.Kind == mldiscover.KindVLLM || c.Kind == mldiscover.KindTriton:
			kind, name, _ := strings.Cut(c.Workload, "/")
			servers = append(servers, fmt.Sprintf("%s %s/%s/%s", c.Kind, kind, c.Namespace, name))
		}
	}
	if len(crdOnly) > 0 {
		sort.Strings(crdOnly)
		p.line(1, true, "CRDs with no controller, not adopted: "+strings.Join(crdOnly, ", ")+".")
	}
	if len(servers) > 0 {
		sort.Strings(servers)
		p.line(1, true, "Model servers found (workloads, not stack components):")
		for _, s := range servers {
			p.line(1, true, "  "+s)
		}
	}
}

func (p *proposer) renderDecisions() {
	ds := append([]Decision(nil), p.opts.Decisions...)
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
	if len(ds) == 0 {
		p.line(1, true, "Dismiss a finding until a date:")
		p.line(1, true, `decisions = [forge.RecommendationDecision {id = "<finding id>", reason = "<why>", expires = "YYYY-MM-DD"}]`)
		return
	}
	p.line(1, false, "decisions = [")
	for _, d := range ds {
		decision := d.Decision
		if decision == "" {
			decision = "dismiss"
		}
		p.line(2, false, "forge.RecommendationDecision {")
		p.line(3, false, "id = "+strconv.Quote(d.ID))
		p.line(3, false, "decision = "+strconv.Quote(decision))
		p.line(3, false, "reason = "+strconv.Quote(d.Reason))
		p.line(3, false, "expires = "+strconv.Quote(d.Expires))
		p.line(2, false, "}")
	}
	p.line(1, false, "]")
}

// --- KCL literals ------------------------------------------------------

func kclStringMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strconv.Quote(k) + ": " + strconv.Quote(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// kclValue renders a helm values tree as KCL lines; the first line follows
// `values = ` and the rest are indented from indent.
func kclValue(v any, indent string) []string {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return []string{"{}"}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []string{"{"}
		for _, k := range keys {
			sub := kclValue(t[k], indent+"    ")
			out = append(out, indent+"    "+strconv.Quote(k)+": "+sub[0])
			out = append(out, sub[1:]...)
		}
		return append(out, indent+"}")
	case []any:
		if len(t) == 0 {
			return []string{"[]"}
		}
		out := []string{"["}
		for _, e := range t {
			sub := kclValue(e, indent+"    ")
			out = append(out, indent+"    "+sub[0])
			out = append(out, sub[1:]...)
		}
		return append(out, indent+"]")
	case bool:
		if t {
			return []string{"True"}
		}
		return []string{"False"}
	case int:
		return []string{strconv.Itoa(t)}
	case string:
		if strings.Contains(t, "\n") {
			// A triple-quoted string; the content has no backslash, no
			// `"""` and no `${`, so it needs no escaping.
			ls := strings.Split(strings.TrimSuffix(t, "\n"), "\n")
			out := []string{`"""` + ls[0]}
			out = append(out, ls[1:]...)
			return append(out, `"""`)
		}
		return []string{strconv.Quote(t)}
	}
	return []string{fmt.Sprint(v)}
}

func indentLines(ls []string, pad string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = pad + l
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
