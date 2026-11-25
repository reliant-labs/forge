package mlrecs

import (
	"time"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// Severity values, worst first.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"
	SeverityInfo   = "info"
)

// Confidence values.
const (
	// ConfidenceCertain: the snapshot shows the condition directly.
	ConfidenceCertain = "certain"
	// ConfidenceLikely: inferred from signatures that usually mean it.
	ConfidenceLikely = "likely"
	// ConfidenceUnverified: discovery cannot read the setting (kubelet
	// tracing lives in the kubelet's config file); the finding says what to
	// check.
	ConfidenceUnverified = "unverified"
)

// Fix kinds.
const (
	// FixKCL: a change to the env's ml.k. Fix.KCLDiff carries it.
	FixKCL = "kcl"
	// FixCommand: a setting outside forge's declarations (a GKE node-pool
	// flag). Fix.Command carries it.
	FixCommand = "command"
	// FixManual: something only the owner of the component can do (their
	// GPU Operator's values, cordoning a node). Fix.Summary says what.
	FixManual = "manual"
)

// Finding is one recommendation: a rule that matched, with its evidence and
// the adopt-first fix.
//
// Every field is part of the published `forge ml discover --json` document
// (forge.dev/ml-discover/v1). Fields are only ever added; an ID is never
// renamed (a forge.RecommendationDecision names it).
type Finding struct {
	// ID is the rule's stable id (accel-telemetry-missing).
	ID string `json:"id"`
	// Severity is one of the Severity constants.
	Severity string `json:"severity"`
	// Confidence is one of the Confidence constants.
	Confidence string `json:"confidence"`
	// Title is the one-line finding ("3 nodes with nvidia.com/gpu have no
	// utilisation source").
	Title string `json:"title"`
	// Vendor and Cloud scope the finding when it is vendor- or
	// cloud-specific.
	Vendor string `json:"vendor,omitempty"`
	Cloud  string `json:"cloud,omitempty"`
	// Evidence lists the facts the rule matched on, one per line, naming
	// objects as kind/namespace/name.
	Evidence []string `json:"evidence"`
	// Fix is what to do about it.
	Fix Fix `json:"fix"`
	// Dismissed is set when a forge.RecommendationDecision in the env
	// dismisses this id and has not expired. A dismissed finding is still
	// reported, marked, and sorted last.
	Dismissed *Dismissal `json:"dismissed,omitempty"`
}

// Fix is a finding's remedy.
type Fix struct {
	// Kind is FixKCL, FixCommand or FixManual.
	Kind string `json:"kind"`
	// Summary is the fix in one sentence.
	Summary string `json:"summary"`
	// KCLDiff is a unified diff against the env's ml.k as Propose writes
	// it (`--- a/ml.k` / `+++ b/ml.k`), for FixKCL.
	KCLDiff string `json:"kcl_diff,omitempty"`
	// Command is the exact command, for FixCommand.
	Command string `json:"command,omitempty"`
	// Docs is a URL to the upstream documentation of the fix.
	Docs string `json:"docs,omitempty"`
}

// Decision is a forge.RecommendationDecision read from the env.
type Decision struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	// Expires is YYYY-MM-DD; the decision stops applying the day after.
	Expires string `json:"expires"`
}

// Dismissal is the decision that dismissed a finding.
type Dismissal struct {
	Reason  string `json:"reason"`
	Expires string `json:"expires"`
}

// Options tunes Evaluate.
type Options struct {
	// Decisions are the env's forge.RecommendationDecisions.
	Decisions []Decision
	// Now decides whether a decision has expired. Zero means the
	// snapshot's CollectedAt.
	Now time.Time
}

// Rule describes one rule Evaluate runs, for `forge ml discover --rules` and
// the skill's catalogue.
type Rule struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Vendor   string `json:"vendor,omitempty"`
	// Detects is the detection rule in one sentence.
	Detects string `json:"detects"`
	// Fix is the adopt-first fix in one sentence.
	Fix string `json:"fix"`
}

// Rules lists every rule Evaluate runs, in catalogue order.
func Rules() []Rule {
	// TODO(contract stub): implemented by the pkg/mlrecs task.
	return nil
}

// Evaluate runs every rule over a snapshot and its inventory. It is pure and
// deterministic: findings are sorted by severity, then ID, then evidence, and
// dismissed findings last.
func Evaluate(s *mldiscover.Snapshot, inv *mldiscover.Inventory, opts Options) []Finding {
	// TODO(contract stub): implemented by the pkg/mlrecs task.
	return []Finding{}
}

// ProposeOptions tunes Propose.
type ProposeOptions struct {
	// Namespace is where forge.Install charts render (default "forge-ml").
	Namespace string
	// ConnectedCluster is the control-plane cluster name to put on the
	// ClusterTarget, when the env knows it.
	ConnectedCluster string
	// Date stamps the header and observed_at (YYYY-MM-DD). Zero means the
	// inventory's CollectedAt.
	Date time.Time
}

// Propose writes the env's ml.k for an inventory: one forge.MLStack for the
// cluster, every found component adopted, each suggested install written but
// commented out, and `observed` listing what discovery found running so the
// KCL refuses installing a duplicate. The output is deterministic, valid KCL
// that `import forge` resolves.
func Propose(inv *mldiscover.Inventory, findings []Finding, opts ProposeOptions) string {
	// TODO(contract stub): implemented by the pkg/mlrecs task.
	return ""
}
