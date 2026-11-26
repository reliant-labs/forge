package cli

// The human form of `forge ml discover`: the inventory as a table of
// sections (FOUND / MISSING lines with what to adopt and what to install),
// then the findings worst first, then the proposal. The section labels and
// their order are the proposal's §1 sample.

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/mldiscover"
	"github.com/reliant-labs/forge/pkg/mlrecs"
)

// mlLabelWidth pads every section label to one column.
const mlLabelWidth = 15

type mlSection struct {
	label      string
	categories []string
	// missing is the line printed when nothing of these categories is
	// present; "" omits the section when it is empty.
	missing func(inv *mldiscover.Inventory) string
}

// mlSections is the inventory table, in print order. A MISSING line names
// forge's default for that slot, by the components policy: install only
// what is missing, never a duplicate, adopt-only for Ray and KServe.
var mlSections = []mlSection{
	{label: "accelerators", categories: []string{mldiscover.CategoryOperator, mldiscover.CategoryDevicePlugin}},
	{label: "gpu metrics", categories: []string{mldiscover.CategoryAccelMetrics}, missing: mlMissingMetrics},
	{label: "node health", categories: []string{mldiscover.CategoryNodeHealth, mldiscover.CategoryLifecycle}, missing: mlMissingNodeHealth},
	{label: "queueing", categories: []string{mldiscover.CategoryQueue}, missing: func(*mldiscover.Inventory) string {
		return "MISSING kueue — install (forge's default queue, with jobset)"
	}},
	{label: "training", categories: []string{mldiscover.CategoryTraining}, missing: func(*mldiscover.Inventory) string {
		return "MISSING kubeflow-trainer — install (forge's default); kuberay is adopt-only, never installed by default"
	}},
	{label: "serving", categories: []string{mldiscover.CategoryServing}, missing: func(*mldiscover.Inventory) string {
		return "none found (kserve is adopted when present, never installed by default)"
	}},
	{label: "autoscaling", categories: []string{mldiscover.CategoryAutoscaling}},
	{label: "monitoring", categories: []string{mldiscover.CategoryMonitoring}, missing: func(*mldiscover.Inventory) string {
		return "none found (no Prometheus Operator, GMP, kube-state-metrics or OTel collector)"
	}},
	{label: "image pulls", categories: []string{mldiscover.CategoryPullCache}, missing: func(*mldiscover.Inventory) string {
		return "none found (no spegel or dragonfly pull cache)"
	}},
}

// mlDefaultExporter is each vendor's metrics exporter.
var mlDefaultExporter = map[string]string{
	mldiscover.VendorNVIDIA: mldiscover.KindDCGMExporter,
	mldiscover.VendorAMD:    mldiscover.KindAMDMetricsExporter,
	mldiscover.VendorNeuron: mldiscover.KindNeuronMonitor,
	mldiscover.VendorGaudi:  mldiscover.KindGaudiExporter,
}

func mlMissingNodeHealth(inv *mldiscover.Inventory) string {
	line := "MISSING node-problem-detector — install (forge's default)"
	switch inv.Cloud {
	case mldiscover.CloudGKE:
		line += "; GKE auto-repair is a node-pool setting the API cannot see: check it"
	case mldiscover.CloudEKS:
		line += ", or adopt the EKS node monitoring agent if you enable it"
	case mldiscover.CloudAKS:
		line += "; AKS auto-repair is a cluster setting the API cannot see: check it"
	}
	return line
}

func mlMissingMetrics(inv *mldiscover.Inventory) string {
	if len(inv.Accelerators) == 0 {
		return "none found (and no accelerators to report on)"
	}
	var lines []string
	seen := map[string]bool{}
	for _, g := range inv.Accelerators {
		if seen[g.Vendor] {
			continue
		}
		seen[g.Vendor] = true
		kind := mlDefaultExporter[g.Vendor]
		switch {
		case kind == "":
			lines = append(lines, fmt.Sprintf("MISSING a %s utilisation source (no exporter forge knows for %s)", g.Vendor, g.Resource))
		case g.Vendor == mldiscover.VendorNVIDIA && inv.Has(mldiscover.KindGPUOperator):
			lines = append(lines, "MISSING dcgm-exporter — the GPU Operator runs: enable spec.dcgmExporter in its ClusterPolicy rather than installing a second")
		default:
			lines = append(lines, fmt.Sprintf("MISSING %s — install", kind))
		}
	}
	return strings.Join(lines, "\n")
}

func writeMLDiscoverHuman(w io.Writer, doc MLDiscoverDocument, o mlDiscoverOptions) error {
	p := &mlPrinter{w: w}
	inv := &doc.Inventory

	cluster := doc.Context
	if cluster == "" {
		cluster = "(no context recorded)"
	}
	var about []string
	if doc.ServerVersion != "" {
		about = append(about, doc.ServerVersion)
	}
	if doc.Cloud != "" && doc.Cloud != mldiscover.CloudOther {
		about = append(about, doc.Cloud)
	}
	if len(about) > 0 {
		cluster += " (" + strings.Join(about, ", ") + ")"
	}
	p.row("cluster", cluster)

	for _, sec := range mlSections {
		var lines []string
		if sec.label == "accelerators" {
			lines = append(lines, mlAcceleratorLines(inv)...)
		}
		present := false
		for _, c := range inv.Components {
			if !slices.Contains(sec.categories, c.Category) {
				continue
			}
			present = present || c.Status == mldiscover.StatusPresent
			lines = append(lines, mlComponentLine(c))
			if cfg := mlConfigLine(c.Config); cfg != "" {
				lines = append(lines, "    "+cfg)
			}
		}
		if !present && sec.missing != nil {
			lines = append(lines, strings.Split(sec.missing(inv), "\n")...)
		}
		if len(lines) == 0 {
			continue
		}
		p.row(sec.label, lines...)
	}
	mlCoverageLines(p, doc.Coverage)
	if len(doc.Warnings) > 0 {
		p.row("could not see", doc.Warnings...)
	}
	if doc.Snapshot != "" {
		p.row("snapshot", "saved to "+doc.Snapshot)
	}

	// Findings, worst first (Evaluate's order; dismissed last).
	p.blank()
	active := 0
	for _, f := range doc.Findings {
		if f.Dismissed == nil {
			active++
		}
	}
	if len(doc.Findings) == 0 {
		p.row("findings", "none")
	} else {
		head := fmt.Sprintf("%d, worst first", active)
		if n := len(doc.Findings) - active; n > 0 {
			head += fmt.Sprintf(" (and %d dismissed)", n)
		}
		p.row("findings", head)
		for _, f := range doc.Findings {
			mlFindingLines(p, f)
		}
	}

	// The proposal.
	p.blank()
	mlProposalLines(p, doc, o)
	return p.err
}

func mlAcceleratorLines(inv *mldiscover.Inventory) []string {
	if len(inv.Accelerators) == 0 {
		return []string{"none found (no node advertises an accelerator resource or label)"}
	}
	var out []string
	for _, g := range inv.Accelerators {
		what := g.Resource
		if g.Product != "" {
			what = g.Product + " (" + g.Resource + ")"
		}
		line := fmt.Sprintf("%d × %s on %s", g.Capacity, what, mlCount(g.Nodes, "node"))
		var detail []string
		if g.Pool != "" {
			detail = append(detail, "pool "+g.Pool)
		}
		if len(g.DriverVersions) > 0 {
			detail = append(detail, "driver "+strings.Join(g.DriverVersions, ", "))
		}
		if g.Sharing != "" {
			detail = append(detail, g.Sharing)
		}
		detail = append(detail, fmt.Sprintf("%d allocated", g.Allocated))
		if g.Allocatable < g.Capacity {
			detail = append(detail, fmt.Sprintf("only %d allocatable", g.Allocatable))
		}
		if g.DetectedBy == "label" {
			detail = append(detail, "labelled but no capacity: device plugin missing or not ready")
		}
		out = append(out, line+" ("+strings.Join(detail, "; ")+")")
	}
	return out
}

func mlComponentLine(c mldiscover.Component) string {
	var b strings.Builder
	if c.Status == mldiscover.StatusCRDOnly {
		fmt.Fprintf(&b, "CRD ONLY %s (its CRDs are installed but no controller runs", c.Kind)
		if c.Resources > 0 {
			b.WriteString("; " + mlCount(c.Resources, "object"))
		}
		b.WriteString(")")
		return b.String()
	}
	b.WriteString("FOUND " + c.Kind)
	if c.Version != "" {
		b.WriteString(" " + c.Version)
	}
	if c.Via != "" {
		b.WriteString(" via " + c.Via)
	}
	var where []string
	if c.Workload != "" {
		// Kind/namespace/name, the way findings name objects.
		loc := c.Workload
		if kind, name, ok := strings.Cut(c.Workload, "/"); ok && c.Namespace != "" {
			loc = kind + "/" + c.Namespace + "/" + name
		} else if c.Namespace != "" {
			loc = c.Namespace + "/" + c.Workload
		}
		if c.MetricsPort > 0 {
			loc += fmt.Sprintf(" :%d", c.MetricsPort)
		}
		where = append(where, loc)
	} else if c.Namespace != "" {
		where = append(where, c.Namespace)
	}
	if c.Pods > 0 {
		where = append(where, mlCount(c.Pods, "pod"))
	}
	if c.Resources > 0 {
		where = append(where, mlCount(c.Resources, "object"))
	}
	if len(where) > 0 {
		b.WriteString(" (" + strings.Join(where, ", ") + ")")
	}
	if c.Owner == mldiscover.OwnerForge {
		b.WriteString(" — installed by forge")
	} else {
		b.WriteString(" — adopt")
	}
	return b.String()
}

// mlConfigLine summarises what discovery read of a component's
// configuration, "" when it read nothing.
func mlConfigLine(cfg *mldiscover.ComponentConfig) string {
	if cfg == nil {
		return ""
	}
	var parts []string
	add := func(label string, items []string, max int) {
		if len(items) > 0 {
			parts = append(parts, label+" "+mlList(items, max))
		}
	}
	if n := len(cfg.Fields); n > 0 {
		parts = append(parts, mlCount(n, "field"))
	}
	add("monitors", cfg.Monitors, 4)
	add("conditions", cfg.Conditions, 4)
	add("receivers", cfg.Receivers, 5)
	add("exporters", cfg.Exporters, 4)
	add("pipelines", cfg.Pipelines, 4)
	add("settings", cfg.Settings, 4)
	if len(parts) == 0 {
		return ""
	}
	from := cfg.DeterminedBy
	switch {
	case strings.HasPrefix(cfg.Source, cfg.DeterminedBy):
		from = cfg.Source
	case cfg.Source != "":
		from += ": " + cfg.Source
	}
	return "config (" + from + "): " + strings.Join(parts, "; ")
}

// mlCoverageStatus is how each status prints; a gap is upper case.
var mlCoverageStatus = map[string]string{
	mldiscover.CoverageCollected: "collected",
	mldiscover.CoveragePartial:   "PARTIAL",
	mldiscover.CoverageMissing:   "MISSING",
	mldiscover.CoverageUnknown:   "unknown",
}

// mlCoverageLines prints one line per signal, grouped by category in the
// inventory's order, with what is missing, the nodes reached, the note and
// the findings the gap raised. Not-applicable signals are only counted.
func mlCoverageLines(p *mlPrinter, cov []mldiscover.SignalCoverage) {
	if len(cov) == 0 {
		return
	}
	counts := map[string]int{}
	for _, c := range cov {
		counts[c.Status]++
	}
	var head []string
	for _, st := range []string{mldiscover.CoverageCollected, mldiscover.CoveragePartial, mldiscover.CoverageMissing, mldiscover.CoverageUnknown, mldiscover.CoverageNotApplicable} {
		if counts[st] > 0 {
			head = append(head, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	p.blank()
	p.row("coverage", fmt.Sprintf("%d signals: %s", len(cov), strings.Join(head, ", ")))
	width := 0
	for _, c := range cov {
		width = max(width, len(c.Signal))
	}
	category, lastNote := "", ""
	for _, c := range cov {
		if c.Status == mldiscover.CoverageNotApplicable {
			continue
		}
		if c.Category != category {
			category, lastNote = c.Category, ""
			title := category
			if c.Vendor != "" {
				title += " (" + c.Vendor + ")"
			}
			p.line("  " + title)
		}
		status := mlCoverageStatus[c.Status]
		if status == "" {
			status = c.Status
		}
		var detail []string
		if len(c.Sources) > 0 {
			detail = append(detail, mlList(c.Sources, 2))
		}
		if c.NodesTotal > 0 {
			detail = append(detail, fmt.Sprintf("%d/%d nodes", c.NodesCovered, c.NodesTotal))
		}
		if len(c.Versions) > 0 {
			detail = append(detail, mlList(c.Versions, 2))
		}
		p.line(fmt.Sprintf("    %-9s %-*s  %-15s %s", status, width, c.Signal, c.DeterminedBy, strings.Join(detail, "; ")))
		pad := strings.Repeat(" ", 4+10+width+2)
		if len(c.Missing) > 0 {
			p.line(pad + "missing " + mlList(c.Missing, 4))
		}
		// A note shared by consecutive rows (every DCGM group read from the
		// same default counters file) prints once.
		if c.Note != "" && c.Note != lastNote {
			p.line(pad + c.Note)
		}
		lastNote = c.Note
		if len(c.Findings) > 0 {
			p.line(pad + "finding " + strings.Join(c.Findings, ", "))
		}
	}
}

// mlList joins at most n items, saying how many more there are.
func mlList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(", … (+%d)", len(items)-n)
}

func mlFindingLines(p *mlPrinter, f mlrecs.Finding) {
	sev := strings.ToUpper(f.Severity)
	if f.Dismissed != nil {
		sev = "dismissed"
	}
	p.line(fmt.Sprintf("  %-9s %s  %s", sev, f.ID, f.Title))
	if f.Dismissed != nil {
		p.line(fmt.Sprintf("            dismissed until %s: %s", f.Dismissed.Expires, f.Dismissed.Reason))
	}
	if f.Confidence != "" && f.Confidence != mlrecs.ConfidenceCertain {
		p.line("            confidence " + f.Confidence)
	}
	ev := append([]string(nil), f.Evidence...)
	sort.Strings(ev)
	for i, e := range ev {
		label := "evidence"
		if i > 0 {
			label = ""
		}
		p.line(fmt.Sprintf("            %-9s %s", label, e))
	}
	if f.Fix.Summary != "" {
		p.line(fmt.Sprintf("            %-9s %s", "fix", f.Fix.Summary))
	}
	if f.Fix.Command != "" {
		p.line(fmt.Sprintf("            %-9s $ %s", "", f.Fix.Command))
	}
	if f.Fix.Kind == mlrecs.FixKCL && f.Fix.KCLDiff != "" {
		p.line(fmt.Sprintf("            %-9s the KCL change is in --json (findings[].fix.kcl_diff)", ""))
	}
	if f.Fix.Docs != "" {
		p.line(fmt.Sprintf("            %-9s %s", "docs", f.Fix.Docs))
	}
}

func mlProposalLines(p *mlPrinter, doc MLDiscoverDocument, o mlDiscoverOptions) {
	pr := doc.Proposal
	if pr.MLK == "" {
		p.row("proposal", "nothing to propose")
		return
	}
	switch {
	case pr.Path == "":
		p.row("proposal", "the ml.k below is printed only: place it with --env <env> (deploy/kcl/<env>/ml.k) or --out <file>, then --write")
	case pr.Written && pr.Exists:
		p.row("proposal", "wrote "+pr.Path+" over the existing file (diff below; it is in git)")
	case pr.Written:
		p.row("proposal", "wrote "+pr.Path)
	case pr.Exists && pr.Diff == "":
		p.row("proposal", pr.Path+" is up to date")
	case pr.Exists:
		p.row("proposal", pr.Path+" differs (diff below): run with --write to update it")
	default:
		p.row("proposal", pr.Path+" (not written: run with --write to write it)")
	}
	mainK := "deploy/kcl/<env>/main.k"
	if o.env != "" {
		mainK = filepath.ToSlash(filepath.Join("deploy", "kcl", o.env, "main.k"))
	}
	p.row("", "wire it into "+mainK+":",
		"    import .ml",
		"    ml_stacks = ml.ml_stacks   # in its forge.Bundle")
	p.blank()
	switch {
	case pr.Exists && pr.Diff != "":
		p.raw(pr.Diff)
	case pr.Exists:
	default:
		p.raw(pr.MLK)
	}
}

func mlCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// mlPrinter writes label-column rows and remembers the first write error.
type mlPrinter struct {
	w   io.Writer
	err error
}

func (p *mlPrinter) line(s string) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, strings.TrimRight(s, " "))
	}
}

func (p *mlPrinter) row(label string, lines ...string) {
	for i, l := range lines {
		if i > 0 {
			label = ""
		}
		p.line(fmt.Sprintf("%-*s%s", mlLabelWidth, label, l))
	}
}

func (p *mlPrinter) blank() { p.line("") }

func (p *mlPrinter) raw(s string) {
	if p.err == nil {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		_, p.err = io.WriteString(p.w, s)
	}
}
