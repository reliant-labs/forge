package mlrecs

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// LinkCoverage returns a copy of cov with each row's Findings set to the ids
// of the findings about its signal (a finding's Signals, else its rule's).
// Pure: neither argument is modified.
func LinkCoverage(cov []mldiscover.SignalCoverage, findings []Finding) []mldiscover.SignalCoverage {
	ruleSignals := map[string][]string{}
	for _, r := range rules {
		ruleSignals[r.ID] = r.Signals
	}
	bySignal := map[string]map[string]bool{}
	for _, f := range findings {
		sigs := f.Signals
		if len(sigs) == 0 {
			sigs = ruleSignals[f.ID]
		}
		for _, s := range sigs {
			if bySignal[s] == nil {
				bySignal[s] = map[string]bool{}
			}
			bySignal[s][f.ID] = true
		}
	}
	out := make([]mldiscover.SignalCoverage, len(cov))
	for i, row := range cov {
		row.Sources = append([]string(nil), row.Sources...)
		row.Expected = append([]string(nil), row.Expected...)
		row.Present = append([]string(nil), row.Present...)
		row.Missing = append([]string(nil), row.Missing...)
		row.Versions = append([]string(nil), row.Versions...)
		row.Findings = nil
		if ids := bySignal[row.Signal]; len(ids) > 0 {
			row.Findings = sortedKeys(ids)
		}
		out[i] = row
	}
	return out
}

// confidenceFrom maps how a coverage row was determined to a finding's
// confidence.
func confidenceFrom(determinedBy string) string {
	switch determinedBy {
	case mldiscover.DeterminedByProbe, mldiscover.DeterminedByConfig:
		return ConfidenceCertain
	case mldiscover.DeterminedByArgs, mldiscover.DeterminedByVersionDefault:
		return ConfidenceLikely
	}
	return ConfidenceUnverified
}

// coverage returns the inventory's row for a signal.
func (c *evalCtx) coverage(signal string) (mldiscover.SignalCoverage, bool) {
	for _, r := range c.inv.Coverage {
		if r.Signal == signal {
			return r, true
		}
	}
	return mldiscover.SignalCoverage{}, false
}

func gap(r mldiscover.SignalCoverage) bool {
	return r.Status == mldiscover.CoverageMissing || r.Status == mldiscover.CoveragePartial
}

// --- DCGM field sets ---------------------------------------------------

// csvLine is one counters-CSV row to recommend.
type csvLine struct{ name, typ, help string }

func (l csvLine) String() string { return l.name + ", " + l.typ + ", " + l.help }

// classicLines are the CSV rows to add for each signal part on a
// dcgm-exporter before 4.9, with the help text of etc/default-counters.csv
// at 4.8.4 (checked 2026-11-26). NVLink error counters were renamed in 4.8;
// classicNVLinkPre48 is 3.3.9-3.6.1's name.
var classicLines = map[string][]csvLine{
	"PROF_GR_ENGINE_ACTIVE":       {{"DCGM_FI_PROF_GR_ENGINE_ACTIVE", "gauge", "Ratio of time the graphics engine is active."}},
	"PROF_SM_ACTIVE":              {{"DCGM_FI_PROF_SM_ACTIVE", "gauge", "The ratio of cycles an SM has at least 1 warp assigned."}},
	"PROF_SM_OCCUPANCY":           {{"DCGM_FI_PROF_SM_OCCUPANCY", "gauge", "The ratio of number of warps resident on an SM."}},
	"PROF_PIPE_TENSOR_ACTIVE":     {{"DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", "gauge", "Ratio of cycles the tensor (HMMA) pipe is active."}},
	"PROF_DRAM_ACTIVE":            {{"DCGM_FI_PROF_DRAM_ACTIVE", "gauge", "Ratio of cycles the device memory interface is active sending or receiving data."}},
	"XID_ERRORS":                  {{"DCGM_FI_DEV_XID_ERRORS", "gauge", "Value of the last XID error encountered."}},
	"ECC_SBE":                     {{"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", "counter", "Total number of single-bit volatile ECC errors."}},
	"ECC_DBE":                     {{"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "counter", "Total number of double-bit volatile ECC errors."}},
	"UNCORRECTABLE_REMAPPED_ROWS": {{"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", "counter", "Number of remapped rows for uncorrectable errors"}},
	"ROW_REMAP_FAILURE":           {{"DCGM_FI_DEV_ROW_REMAP_FAILURE", "gauge", "Whether remapping of rows has failed"}},
	"NVLINK_BANDWIDTH":            {{"DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", "gauge", "Aggregate NVLink throughput across all lanes (in MB/s)."}},
	"NVLINK_ERRORS": {
		{"DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_TOTAL", "counter", "Total number of NVLink flow-control CRC errors across all lanes."},
		{"DCGM_FI_DEV_NVLINK_CRC_ERROR_TOTAL", "counter", "Total number of NVLink data-link CRC errors."},
	},
}

var classicNVLinkPre48 = csvLine{"DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL", "counter", "Total number of NVLink flow-control CRC errors."}

// modernParts maps each part to dcgm-exporter 4.9's names (help text from
// mldiscover.ForgeDCGMFields).
var modernParts = map[string][]string{
	"PROF_GR_ENGINE_ACTIVE":       {"DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO"},
	"PROF_SM_ACTIVE":              {"DCGM_FI_PROF_SM_UTIL_RATIO"},
	"PROF_SM_OCCUPANCY":           {"DCGM_FI_PROF_SM_OCCUPANCY_RATIO"},
	"PROF_PIPE_TENSOR_ACTIVE":     {"DCGM_FI_PROF_TENSOR_UTIL_RATIO"},
	"PROF_DRAM_ACTIVE":            {"DCGM_FI_PROF_DRAM_UTIL_RATIO"},
	"XID_ERRORS":                  {"DCGM_FI_DEV_XID_ERROR", "DCGM_EXP_XID_ERRORS_TOTAL"},
	"ECC_SBE":                     {"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL"},
	"ECC_DBE":                     {"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL"},
	"UNCORRECTABLE_REMAPPED_ROWS": {"DCGM_FI_DEV_ROW_REMAP_UNCORRECTABLE_TOTAL"},
	"ROW_REMAP_FAILURE":           {"DCGM_FI_DEV_ROW_REMAP_FAILED"},
	"NVLINK_BANDWIDTH":            {"DCGM_FI_DEV_NVLINK_THROUGHPUT_BYTES_PER_SECOND"},
	"NVLINK_ERRORS":               {"DCGM_FI_DEV_NVLINK_CRC_ERROR_TOTAL", "DCGM_FI_DEV_NVLINK_RECOVERY_TOTAL", "DCGM_FI_DEV_NVLINK_REPLAY_TOTAL"},
}

// linesFor returns the CSV rows that add parts to an exporter, in the
// naming of its release.
func linesFor(parts []string, src mldiscover.Component) []csvLine {
	modern := src.Config != nil && mldiscover.UsesModernDCGMNames(src.Config.Fields)
	pre48 := false
	if v := exporterRelease(src.Version); v != "" && !modern {
		pre48 = v < "4.8"
	}
	help := map[string]mldiscover.DCGMField{}
	for _, f := range mldiscover.ForgeDCGMFields {
		help[f.Name] = f
	}
	var out []csvLine
	for _, p := range parts {
		if modern {
			for _, n := range modernParts[p] {
				f := help[n]
				out = append(out, csvLine{f.Name, f.Type, f.Help})
			}
			continue
		}
		if p == "NVLINK_ERRORS" && pre48 {
			out = append(out, classicNVLinkPre48)
			continue
		}
		out = append(out, classicLines[p]...)
	}
	return out
}

// exporterRelease is "major.minor" of a dcgm-exporter tag's exporter part,
// zero-padded for string comparison of single-digit majors ("4.8").
func exporterRelease(tag string) string {
	parts := strings.Split(tag, "-")
	cand := parts[0]
	if len(parts) >= 2 && strings.Count(parts[0], ".") == 2 && strings.Count(parts[1], ".") == 2 {
		cand = parts[1]
	}
	maj, rest, ok := strings.Cut(cand, ".")
	if !ok {
		return ""
	}
	min, _, _ := strings.Cut(rest, ".")
	if len(min) == 1 {
		min = "0" + min
	}
	return maj + "." + min
}

// dcgmGap is what one exporter lacks for a set of signals.
type dcgmGap struct {
	src   mldiscover.Component
	parts []string // missing parts across the signals, in order
	sigs  []string
}

// dcgmGaps lists, per running DCGM exporter whose field set is known, the
// parts of signals it does not export. GKE's managed exporter is skipped
// when skipManaged is set (its set is fixed).
func (c *evalCtx) dcgmGaps(signals []string, skipManaged bool) []dcgmGap {
	var out []dcgmGap
	for _, comp := range c.present(mldiscover.KindDCGMExporter) {
		if comp.Config == nil || comp.Config.DeterminedBy == mldiscover.DeterminedByPresence {
			continue
		}
		if skipManaged && comp.Via == mldiscover.ViaGKEManaged {
			continue
		}
		g := dcgmGap{src: comp}
		for _, sig := range signals {
			have := setOfStr(mldiscover.DCGMPartsPresent(sig, comp.Config.Fields))
			missing := false
			for _, p := range mldiscover.DCGMPartNames(sig) {
				if !have[p] {
					g.parts = append(g.parts, p)
					missing = true
				}
			}
			if missing {
				g.sigs = append(g.sigs, sig)
			}
		}
		if len(g.parts) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func setOfStr(list []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range list {
		out[s] = true
	}
	return out
}

// dcgmFix writes the fix for exporters that lack parts: a KCL change when
// forge installed the exporter, else exactly what to add to their counters
// file.
func (c *evalCtx) dcgmFix(gaps []dcgmGap, what string) Fix {
	var manual []string
	kcl := false
	for _, g := range gaps {
		ref := componentRef(g.src)
		lines := linesFor(g.parts, g.src)
		var csv []string
		for _, l := range lines {
			csv = append(csv, l.String())
		}
		block := strings.Join(csv, "\n")
		cfg := g.src.Config
		switch {
		case g.src.Owner == mldiscover.OwnerForge:
			kcl = true
		case g.src.Via == mldiscover.ViaGKEManaged:
			manual = append(manual, fmt.Sprintf("%s: GKE's managed DCGM metric set is fixed and cannot export %s. GKE node auto-repair acts on GPU faults; to collect these fields, run NVIDIA's dcgm-exporter instead of (never beside) the managed collection.", ref, what))
		case g.src.Via == mldiscover.ViaGPUOperator && cfg.DeterminedBy == mldiscover.DeterminedByConfig && strings.HasPrefix(cfg.Source, "ConfigMap/"):
			manual = append(manual, fmt.Sprintf("%s reads %s. Add these lines to it, then restart the exporter pods:\n%s", ref, cfg.Source, block))
		case g.src.Via == mldiscover.ViaGPUOperator:
			manual = append(manual, fmt.Sprintf("%s uses the GPU Operator's built-in counters file (%s). Copy that file, add these lines, store it in a ConfigMap in namespace %s under key dcgm-metrics.csv, and set ClusterPolicy spec.dcgmExporter.config.name to the ConfigMap's name:\n%s", ref, cfg.Source, g.src.Namespace, block))
		case cfg.DeterminedBy == mldiscover.DeterminedByConfig && strings.HasPrefix(cfg.Source, "ConfigMap/"):
			manual = append(manual, fmt.Sprintf("%s reads %s. Add these lines to it, then restart the exporter pods:\n%s", ref, cfg.Source, block))
		default:
			manual = append(manual, fmt.Sprintf("%s uses its image's built-in counters file (%s). Mount a counters CSV with these lines added and point the exporter at it (-f <path>, or DCGM_EXPORTER_COLLECTORS):\n%s", ref, cfg.Source, block))
		}
	}
	if kcl && len(manual) == 0 {
		return Fix{Kind: FixKCL, Summary: "forge installed this exporter: the diff adds the missing fields to its config.metrics list in ml.k.",
			Docs: "https://github.com/NVIDIA/dcgm-exporter"}
	}
	summary := strings.Join(manual, "\n\n")
	if kcl {
		summary += "\n\nThe exporter forge installed gets the same fields from the KCL diff."
	}
	return Fix{Kind: FixManual, Summary: summary + "\nforge never edits an exporter it did not install.", Docs: "https://github.com/NVIDIA/dcgm-exporter"}
}

func gapEvidence(g dcgmGap, row mldiscover.SignalCoverage) string {
	src := g.src.Config.Source
	return fmt.Sprintf("%s (%s): no %s; field set from %s", componentRef(g.src), g.src.Version, strings.Join(g.parts, ", "), src)
}

// dcgmGapFinding is a finding for a DCGM field-set gap.
func (c *evalCtx) dcgmGapFinding(signals []string, title, what string, skipManaged bool, filter func(dcgmGap) bool) []Finding {
	var rows []mldiscover.SignalCoverage
	anyGap := false
	for _, sig := range signals {
		if r, ok := c.coverage(sig); ok {
			rows = append(rows, r)
			anyGap = anyGap || gap(r)
		}
	}
	if !anyGap {
		return nil
	}
	gaps := c.dcgmGaps(signals, skipManaged)
	var kept []dcgmGap
	for _, g := range gaps {
		if filter == nil || filter(g) {
			kept = append(kept, g)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	var ev, by []string
	sigs := map[string]bool{}
	for _, g := range kept {
		ev = append(ev, gapEvidence(g, rows[0]))
		by = append(by, g.src.Config.DeterminedBy)
		for _, s := range g.sigs {
			sigs[s] = true
		}
	}
	return []Finding{{
		Confidence: confidenceFrom(weakestBy(by)),
		Title:      title,
		Evidence:   capEvidence(ev),
		Fix:        c.dcgmFix(kept, what),
		Signals:    sortedSet(sigs),
	}}
}

func weakestBy(vals []string) string {
	rank := map[string]int{mldiscover.DeterminedByProbe: 0, mldiscover.DeterminedByConfig: 1, mldiscover.DeterminedByArgs: 2,
		mldiscover.DeterminedByVersionDefault: 3, mldiscover.DeterminedByPresence: 4, mldiscover.DeterminedByUnreadable: 5}
	out := ""
	for _, v := range vals {
		if out == "" || rank[v] > rank[out] {
			out = v
		}
	}
	return out
}

func ruleDCGMProfilingOff(c *evalCtx) []Finding {
	return c.dcgmGapFinding([]string{mldiscover.SignalAccelProfiling},
		"The DCGM exporter exports only coarse utilisation: profiling counters (SM, tensor, DRAM activity) are off",
		"profiling counters", true, nil)
}

func ruleErrorCountersOff(c *evalCtx) []Finding {
	return c.dcgmGapFinding([]string{mldiscover.SignalAccelXid, mldiscover.SignalAccelECC, mldiscover.SignalAccelRowRemap},
		"Xid, ECC or row-remap counters are not collected: failing GPUs go unnoticed",
		"Xid, ECC or row-remap counters", false, nil)
}

func ruleNVLinkOff(c *evalCtx) []Finding {
	multi := map[string]bool{}
	for _, m := range c.inv.Machines {
		if m.Vendor == mldiscover.VendorNVIDIA && m.Capacity >= 2 && m.Sharing == "" {
			multi[m.Name] = true
		}
	}
	if len(multi) == 0 {
		return nil
	}
	return c.dcgmGapFinding([]string{mldiscover.SignalAccelNVLink},
		fmt.Sprintf("NVLink counters are not collected on %s", plural(len(multi), "multi-GPU node", "multi-GPU nodes")),
		"NVLink counters", true, func(g dcgmGap) bool {
			for _, n := range mldiscover.NodeCoverage(c.s, g.src) {
				if multi[n] {
					return true
				}
			}
			return false
		})
}

func rulePodAttributionOff(c *evalCtx) []Finding {
	r, ok := c.coverage(mldiscover.SignalAccelPodAttribution)
	if !ok || !gap(r) {
		return nil
	}
	var ev, manual []string
	kcl := false
	by := ""
	for _, comp := range c.present(mldiscover.KindDCGMExporter) {
		cfg := comp.Config
		if cfg == nil {
			continue
		}
		attr, attrBy := "", ""
		for _, s := range cfg.Settings {
			k, v, _ := strings.Cut(s, "=")
			switch k {
			case mldiscover.SettingKubernetesAttribution:
				attr = v
			case mldiscover.SettingKubernetesAttribution + ".determined-by":
				attrBy = v
			}
		}
		if attr != "false" {
			continue
		}
		by = weakestBy([]string{by, attrBy})
		ref := componentRef(comp)
		ev = append(ev, fmt.Sprintf("%s: no pod/namespace/container labels (determined by %s)", ref, attrBy))
		switch {
		case comp.Owner == mldiscover.OwnerForge:
			kcl = true
		case comp.Via == mldiscover.ViaGKEManaged:
			manual = append(manual, ref+": GKE's managed DCGM collection sets its own labels; this is not configurable.")
		default:
			manual = append(manual, ref+": set DCGM_EXPORTER_KUBERNETES=true (or pass --kubernetes) and mount the kubelet pod-resources socket (/var/lib/kubelet/pod-resources), so utilisation joins to pods.")
		}
	}
	if len(ev) == 0 {
		return nil
	}
	if by == "" {
		by = r.DeterminedBy
	}
	fix := Fix{Kind: FixManual, Summary: strings.Join(manual, "\n") + "\nforge never edits an exporter it did not install.", Docs: "https://github.com/NVIDIA/dcgm-exporter"}
	if kcl && len(manual) == 0 {
		fix = Fix{Kind: FixKCL, Summary: "forge installed this exporter: the diff turns on its Kubernetes enrichment in ml.k.", Docs: "https://github.com/NVIDIA/dcgm-exporter"}
	}
	return []Finding{{
		Confidence: confidenceFrom(by),
		Title:      "GPU metrics carry no pod labels: utilisation cannot be joined to workloads",
		Evidence:   ev,
		Fix:        fix,
		Signals:    []string{mldiscover.SignalAccelPodAttribution},
	}}
}

// --- node health -------------------------------------------------------

func ruleNPDNoGPURules(c *evalCtx) []Finding {
	r, ok := c.coverage(mldiscover.SignalNodeHealthGPURules)
	if !ok || r.Status != mldiscover.CoverageMissing {
		return nil
	}
	var ev, manual []string
	kcl := false
	var by []string
	for _, comp := range c.present(mldiscover.KindNodeProblemDetector) {
		cfg := comp.Config
		if cfg == nil || comp.Workload == "" {
			continue
		}
		gpu := ""
		for _, s := range cfg.Settings {
			if k, v, _ := strings.Cut(s, "="); k == mldiscover.SettingGPURules {
				gpu = v
			}
		}
		if gpu != "false" {
			continue
		}
		ref := componentRef(comp)
		by = append(by, cfg.DeterminedBy)
		src := cfg.Source
		if src == "" {
			src = "args"
		}
		ev = append(ev, fmt.Sprintf("%s: monitors %s (from %s), none matches NVIDIA Xid / NVRM kernel lines", ref, strings.Join(cfg.Monitors, ", "), src))
		if comp.Owner == mldiscover.OwnerForge {
			kcl = true
			continue
		}
		manual = append(manual, fmt.Sprintf("%s: add this monitor to its custom config ConfigMap as gpu-xid-monitor.json and append its path to --config.system-log-monitor (deliveryhero chart: settings.custom_monitor_definitions and settings.log_monitors):\n%s", ref, strings.TrimSpace(gpuXidMonitor)))
	}
	if len(ev) == 0 {
		return nil
	}
	fix := Fix{Kind: FixManual, Summary: strings.Join(manual, "\n\n") + "\nforge never edits a node-problem-detector it did not install.", Docs: "https://github.com/kubernetes/node-problem-detector"}
	if kcl && len(manual) == 0 {
		fix = Fix{Kind: FixKCL, Summary: "forge installed this node-problem-detector: the diff adds the GPU Xid monitor to its values in ml.k.", Docs: "https://github.com/kubernetes/node-problem-detector"}
	}
	return []Finding{{
		Confidence: confidenceFrom(weakestBy(by)),
		Title:      "node-problem-detector has no GPU rule: Xid errors never mark a node unhealthy",
		Evidence:   ev,
		Fix:        fix,
		Signals:    []string{mldiscover.SignalNodeHealthGPURules},
	}}
}

// --- control-plane tracing ---------------------------------------------

func ruleKubeletTracing(c *evalCtx) []Finding {
	if c.inv.Cloud != mldiscover.CloudOther || len(c.inv.Machines) == 0 {
		return nil
	}
	r, ok := c.coverage(mldiscover.SignalKubeletTracing)
	if ok && r.Status == mldiscover.CoverageCollected {
		return nil
	}
	docs := "https://kubernetes.io/docs/concepts/cluster-administration/system-traces/"
	if ok && r.Status == mldiscover.CoverageMissing {
		return []Finding{{
			Confidence: confidenceFrom(r.DeterminedBy),
			Title:      "Kubelet tracing is off: image pull and container start phases are opaque",
			Evidence:   append(append([]string{}, r.Sources...), "KubeletConfiguration has no tracing block"),
			Fix: Fix{Kind: FixManual,
				Summary: "Add a tracing block to the KubeletConfiguration in kube-system/kubelet-config (and each node's /var/lib/kubelet/config.yaml, then restart the kubelets): `tracing: {endpoint: <your OTLP collector>:4317, samplingRatePerMillion: 10000}`. GA in Kubernetes 1.34.",
				Docs:    docs},
			Signals: []string{mldiscover.SignalKubeletTracing},
		}}
	}
	var ev []string
	for _, m := range c.inv.Machines {
		ev = append(ev, fmt.Sprintf("Node/%s: kubelet %s", m.Name, m.KubeletVersion))
	}
	if ok && r.Note != "" {
		ev = append(ev, r.Note)
	}
	return []Finding{{
		Confidence: ConfidenceUnverified,
		Title:      fmt.Sprintf("Kubelet tracing may be off on %s (unverified)", plural(len(c.inv.Machines), "self-managed accelerator node", "self-managed accelerator nodes")),
		Evidence:   capEvidence(ev),
		Fix: Fix{Kind: FixManual,
			Summary: "Discovery cannot read the kubelet's config (it never requests nodes/proxy), so check it: if KubeletConfiguration has no `tracing` block, add one (GA in Kubernetes 1.34) pointing at your OTLP collector, to see image pull and container start spans. Dismiss this finding once it is on.",
			Docs:    docs},
		Signals: []string{mldiscover.SignalKubeletTracing},
	}}
}

func ruleAPIServerTracing(c *evalCtx) []Finding {
	if c.inv.Cloud != mldiscover.CloudOther {
		return nil
	}
	r, ok := c.coverage(mldiscover.SignalAPIServerTracing)
	if !ok || r.Status != mldiscover.CoverageMissing {
		return nil
	}
	return []Finding{{
		Confidence: confidenceFrom(r.DeterminedBy),
		Title:      "API server tracing is off",
		Evidence:   append(append([]string{}, r.Sources...), "no --"+mldiscover.APIServerTracingFlag),
		Fix: Fix{Kind: FixManual,
			Summary: "Write a TracingConfiguration file (apiVersion: apiserver.config.k8s.io/v1, kind: TracingConfiguration, endpoint: <your OTLP collector>:4317, samplingRatePerMillion: 10000) on each control-plane node and pass it with --tracing-config-file: kubeadm ClusterConfiguration apiServer.extraArgs plus an apiServer.extraVolumes entry for the file. GA in Kubernetes 1.34.",
			Docs:    "https://kubernetes.io/docs/concepts/cluster-administration/system-traces/"},
		Signals: []string{mldiscover.SignalAPIServerTracing},
	}}
}

// --- telemetry ---------------------------------------------------------

func ruleNoContinuousProfiling(c *evalCtx) []Finding {
	if len(c.inv.Machines) == 0 {
		return nil
	}
	r, ok := c.coverage(mldiscover.SignalContinuousProfiling)
	if !ok || !gap(r) {
		return nil
	}
	ev := []string{fmt.Sprintf("profilers reach %d of %d accelerator nodes", r.NodesCovered, r.NodesTotal)}
	if len(r.Sources) > 0 {
		ev = append(ev, "profilers: "+strings.Join(r.Sources, ", "))
	}
	summary := "Run one continuous eBPF profiler as a DaemonSet on the accelerator nodes: Parca agent, Grafana Alloy with pyroscope.ebpf, or the OpenTelemetry eBPF profiler (otel/opentelemetry-collector-ebpf-profiler). It shows where CPU time goes in data loaders and serving code with no code change."
	if r.Status == mldiscover.CoveragePartial {
		summary = "Extend your profiler's node selector and tolerations to every accelerator node."
	}
	return []Finding{{
		Confidence: confidenceFrom(r.DeterminedBy),
		Severity:   SeverityLow,
		Title:      "No continuous profiling on the accelerator nodes",
		Evidence:   ev,
		Fix:        Fix{Kind: FixManual, Summary: summary, Docs: "https://www.parca.dev/docs/parca-agent"},
		Signals:    []string{mldiscover.SignalContinuousProfiling},
	}}
}

func collectorFixTarget(r mldiscover.SignalCoverage) string {
	if len(r.Sources) > 0 {
		return strings.Join(r.Sources, ", ")
	}
	return "an OpenTelemetry Collector"
}

func ruleNoOTLPReceiver(c *evalCtx) []Finding {
	r, ok := c.coverage(mldiscover.SignalOTLPReceiver)
	if !ok || r.Status != mldiscover.CoverageMissing || len(c.inv.Machines) == 0 {
		return nil
	}
	summary := "Install an OpenTelemetry Collector with an otlp receiver (grpc :4317, http :4318) so vLLM, Triton and training jobs can export traces and metrics."
	if len(r.Sources) > 0 {
		summary = "Add an otlp receiver (protocols: grpc :4317, http :4318) to " + collectorFixTarget(r) + " and list it in a traces pipeline's receivers."
	}
	return []Finding{{
		Confidence: confidenceFrom(r.DeterminedBy),
		Title:      "Nothing accepts OTLP from workloads",
		Evidence:   []string{noteOr(r.Note, "no otlp receiver")},
		Fix:        Fix{Kind: FixManual, Summary: summary, Docs: "https://github.com/open-telemetry/opentelemetry-collector/tree/main/receiver/otlpreceiver"},
		Signals:    []string{mldiscover.SignalOTLPReceiver},
	}}
}

func noteOr(note, def string) string {
	if note == "" {
		return def
	}
	return note
}

func ruleCollectorNoK8sEvents(c *evalCtx) []Finding {
	r, ok := c.coverage(mldiscover.SignalK8sEvents)
	if !ok || r.Status != mldiscover.CoverageMissing || len(r.Sources) == 0 {
		return nil
	}
	return []Finding{{
		Confidence: confidenceFrom(r.DeterminedBy),
		Title:      "The collector does not keep Kubernetes events: scheduling, pull and eviction history is lost",
		Evidence:   []string{"collectors: " + strings.Join(r.Sources, ", ")},
		Fix: Fix{Kind: FixManual,
			Summary: "Add a k8s_events receiver (or k8sobjects with `objects: [{name: events, mode: watch}]`) to " + collectorFixTarget(r) + " in a logs pipeline; run it in one replica so events are not duplicated.",
			Docs:    "https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/receiver/k8seventsreceiver"},
		Signals: []string{mldiscover.SignalK8sEvents},
	}}
}

// --- workload tracing --------------------------------------------------

func ruleInferenceTracingOff(c *evalCtx) []Finding {
	r, ok := c.coverage(mldiscover.SignalInferenceTracing)
	if !ok || !gap(r) {
		return nil
	}
	missing := r.Missing
	return []Finding{{
		Confidence: confidenceFrom(r.DeterminedBy),
		Title:      fmt.Sprintf("%s no traces", plural(len(missing), "inference server exports", "inference servers export")),
		Evidence:   capEvidence(append([]string(nil), missing...)),
		Fix: Fix{Kind: FixManual,
			Summary: "vLLM: pass --otlp-traces-endpoint <collector>:4317 (and OTEL_SERVICE_NAME). Triton: --trace-config mode=opentelemetry --trace-config opentelemetry,url=<collector>:4318/v1/traces. Per-request spans show queueing, prefill and decode time.",
			Docs:    "https://docs.vllm.ai/en/latest/examples/online_serving/opentelemetry.html"},
		Signals: []string{mldiscover.SignalInferenceTracing},
	}}
}

func ruleTrainingTracingOff(c *evalCtx) []Finding {
	r, ok := c.coverage(mldiscover.SignalTrainingTracing)
	if !ok || !gap(r) {
		return nil
	}
	return []Finding{{
		Confidence: confidenceFrom(r.DeterminedBy),
		Title:      fmt.Sprintf("%s no OTLP traces", plural(len(r.Missing), "accelerator Job exports", "accelerator Jobs export")),
		Evidence:   capEvidence(append([]string(nil), r.Missing...)),
		Fix: Fix{Kind: FixManual,
			Summary: "Set OTEL_EXPORTER_OTLP_ENDPOINT (and OTEL_SERVICE_NAME) on the Job's containers, or inject an SDK with an OpenTelemetry Instrumentation resource, so steps and data-loading spans reach your collector."},
		Signals: []string{mldiscover.SignalTrainingTracing},
	}}
}
