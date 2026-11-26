package cli

// `forge ml`: ML training and inference in your own clusters. Today one verb,
// `forge ml discover` (docs/proposals/ml-workloads.md, §1 and §3.2): read a
// cluster read-only through the user's own kube context, say what ML
// components it already runs and what is missing, list the recommendations,
// and propose the env's ml.k — adopt what is there, install only what is
// missing, never a duplicate.
//
// The pipeline is three pure steps over one read:
//
//	internal/mlcollect.Collect   the read set (mlcollect.ReadSet), nothing else
//	pkg/mldiscover.Discover      Snapshot → Inventory
//	pkg/mlrecs.Evaluate/Propose  Inventory → findings, and the ml.k
//
// so --from-snapshot runs the same code on a recorded read, and the control
// plane runs the same packages on its collector's inventory.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/mlcollect"
	"github.com/reliant-labs/forge/pkg/mldiscover"
	"github.com/reliant-labs/forge/pkg/mlrecs"
)

func newMLCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ml",
		Short: "ML training and inference in your own clusters: discover what a GPU cluster runs and propose its ml.k",
		Long: `ML training and inference in your own clusters.

` + "`forge ml discover`" + ` reads a cluster read-only through your own kube context and
proposes the env's ml.k: the accelerators, the ML components already running
(adopted, never reinstalled), the one or two that are missing, and the
recommendations, worst first. It needs no control plane and no forge project.`,
	}
	cmd.AddCommand(newMLDiscoverCmd())
	return cmdutil.StrictGroup(cmd)
}

// mlDiscoverOptions is one `forge ml discover` run.
type mlDiscoverOptions struct {
	kubeContext  string
	env          string
	jsonOut      bool
	write        bool
	out          string
	fromSnapshot string
	saveSnapshot string
	namespace    string
	rules        bool
	probe        bool
	// projectDir is the forge project, "" outside one.
	projectDir string
	stdout     io.Writer
	stderr     io.Writer

	// The pipeline's steps; nil is the real one. Tests substitute a fixed
	// read, render or proposal per run, so they run in parallel.
	collect  func(ctx context.Context, kubeContext string) (*mldiscover.Snapshot, error)
	render   func(ctx context.Context, projectDir, env string) ([]byte, error)
	evaluate func(*mldiscover.Snapshot, *mldiscover.Inventory, mlrecs.Options) []mlrecs.Finding
	propose  func(*mldiscover.Inventory, []mlrecs.Finding, mlrecs.ProposeOptions) string
	// probeTargets and runProbe are the opt-in probe's steps.
	probeTargets func(*mldiscover.Snapshot, *mldiscover.Inventory) []mldiscover.ProbeTarget
	runProbe     func(ctx context.Context, kubeContext string, targets []mldiscover.ProbeTarget) ([]mldiscover.MetricsProbe, error)
}

// The real steps.
var (
	mlCollect = func(ctx context.Context, kubeContext string) (*mldiscover.Snapshot, error) {
		cfg, name, err := mlcollect.RESTConfig(kubeContext)
		if err != nil {
			return nil, err
		}
		clients, err := mlcollect.NewClients(cfg)
		if err != nil {
			return nil, err
		}
		return mlcollect.Collect(ctx, clients, mlcollect.Options{Context: name})
	}
	mlProbe = func(ctx context.Context, kubeContext string, targets []mldiscover.ProbeTarget) ([]mldiscover.MetricsProbe, error) {
		if len(targets) == 0 {
			return nil, nil
		}
		cfg, _, err := mlcollect.RESTConfig(kubeContext)
		if err != nil {
			return nil, err
		}
		clients, err := mlcollect.NewClients(cfg)
		if err != nil {
			return nil, err
		}
		return mlcollect.Probe(ctx, mlcollect.ProxyFetch(clients.Kube), targets, mlcollect.ProbeOptions{}), nil
	}
	mlEnvRender = func(ctx context.Context, projectDir, env string) ([]byte, error) {
		return renderKCLRawHook(ctx, projectDir, env)
	}
)

func newMLDiscoverCmd() *cobra.Command {
	o := mlDiscoverOptions{}
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Read a cluster read-only and propose its ml.k: accelerators, what to adopt, what is missing, findings",
		Long: `Read a Kubernetes cluster read-only, through your own kube context, and report
what it runs for ML: its accelerators, the components already there (GPU
Operator, DCGM exporter, Kueue, KServe, ...), what is missing, and the
recommendations, worst first. Then propose the env's ml.k: one forge.MLStack
that ADOPTS every component found, suggests an install only for what is
missing (commented out), and lists what it found under ` + "`observed`" + `, so the
KCL refuses installing a second copy (two DCGM exporters on one node break each
other's profiling counters).

It needs no control plane and no forge project ("start anywhere"), so it works
on a private cluster your laptop can reach. It never writes to the cluster, and
never writes a file unless you pass --write.

  --context    the kube context to read (default: the kubeconfig's current-context)
  --env        the env the ml.k is for: proposes deploy/kcl/<env>/ml.k, and
               applies the env's forge.RecommendationDecisions (a dismissed
               finding is still listed, marked, and sorted last)
  --write      write the proposal (to --out, else deploy/kcl/<env>/ml.k). Over
               an existing ml.k it prints the diff, then writes: the file is in
               git. Without --write the proposal (or its diff) is only printed.
  --json       print the forge.dev/ml-discover/v1 document on stdout and nothing
               else (` + "`forge project schema ml-discover`" + `)
  --probe      also GET each exporter's /metrics through the API server's pod
               proxy (at most one pod per component and node pool, 10s and
               8 MiB each), keeping metric names and label keys, never values:
               the strongest evidence of which DCGM fields are exported. It
               WIDENS THE GRANT: it needs get on pods/proxy, which reaches any
               port of any pod. Off unless asked.

Inside a project, each live read is saved to
.forge/state/ml-inventory-<context>.json; --save-snapshot writes it anywhere, and
--from-snapshot runs discovery on a saved read instead of a cluster.

Coverage: one row per observability signal an ML cluster should have (GPU
utilisation, profiling counters, Xid, ECC, NVLink, node health and GPU rules,
kubelet and API server tracing, the telemetry pipeline, continuous profiling,
workload tracing), each collected, partial, missing or unknown, with how it was
determined (probe, config, args, version-default, presence, unreadable), its
sources and versions, and the parts missing.

What it reads (get/list only; managedFields, annotations and container env
values are stripped, credentials in container args and probe headers become
REDACTED, and ConfigMap data and custom object specs are redacted: values
under keys like token, password, secret, key, auth, header become REDACTED, an
Alloy config keeps only its component blocks, and data that is not JSON, YAML
or a DCGM counters CSV is dropped):

` + mlcollect.ReadSetTable() + `
It never reads: ` + strings.Join(mlcollect.Excluded, "; ") + `.

Exit codes: 0 the read and the proposal succeeded (findings are not failures);
1 bad input (a flag, a snapshot file, a kube context that does not exist);
2 could not look (the cluster is unreachable).

Examples:
  forge ml discover
  forge ml discover --context gke_acme_us-central1_gpu --env prod
  forge ml discover --env prod --write
  forge ml discover --json > inventory.json
  forge ml discover --probe
  forge ml discover --from-snapshot .forge/state/ml-inventory-k3d-dev.json
  forge ml discover --rules`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := cmdutil.FindProjectRoot()
			if err != nil {
				return err
			}
			o.projectDir = root
			o.stdout, o.stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			return runMLDiscover(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.kubeContext, "context", "", "kube context to read (default: the kubeconfig's current-context)")
	f.StringVar(&o.env, "env", "", "env the ml.k is for: deploy/kcl/<env>/ml.k, and the env's RecommendationDecisions")
	f.BoolVar(&o.jsonOut, "json", false, "print the forge.dev/ml-discover/v1 document on stdout and nothing else")
	f.BoolVar(&o.write, "write", false, "write the proposed ml.k (to --out, else deploy/kcl/<env>/ml.k); prints the diff over an existing file")
	f.StringVar(&o.out, "out", "", "path of the proposed ml.k (default deploy/kcl/<env>/ml.k; required with --write outside a project)")
	f.StringVar(&o.fromSnapshot, "from-snapshot", "", "run discovery on a saved snapshot (forge.dev/ml-snapshot/v1) instead of reading a cluster")
	f.StringVar(&o.saveSnapshot, "save-snapshot", "", "also write the snapshot read to this file")
	f.StringVar(&o.namespace, "namespace", "forge-ml", "namespace the proposal's forge.Install charts render into")
	f.BoolVar(&o.probe, "probe", false, "also read each exporter's /metrics through the API server pod proxy (needs get pods/proxy: widens the grant)")
	f.BoolVar(&o.rules, "rules", false, "list every recommendation rule (id, severity, what it detects, the fix) and exit")
	return cmd
}

// mlStateFile is where a live read is saved inside a project, for the lint
// that refuses an Install of a component the inventory shows present.
func mlStateFile(projectDir, kubeContext string) string {
	return filepath.Join(projectDir, ".forge", "state", "ml-inventory-"+sanitizeMLContext(kubeContext)+".json")
}

var mlContextUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeMLContext makes a kube context a file name: gke_acme_us-central1_gpu
// stays as it is; arn:aws:eks:us-east-1:1:cluster/gpu becomes
// arn_aws_eks_us-east-1_1_cluster_gpu.
func sanitizeMLContext(kubeContext string) string {
	s := strings.Trim(mlContextUnsafe.ReplaceAllString(kubeContext, "_"), "._")
	if s == "" {
		return "default"
	}
	return s
}

func runMLDiscover(ctx context.Context, o mlDiscoverOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	o = o.withDefaults()
	if o.rules {
		return writeMLRules(o.stdout, o.jsonOut)
	}
	path, err := o.validate()
	if err != nil {
		return err
	}
	doc := MLDiscoverDocument{Schema: MLDiscoverDocumentSchema, Findings: []mlrecs.Finding{}, Warnings: []string{}}

	snap, err := o.read(ctx)
	if err != nil {
		return err
	}
	inv := o.discover(ctx, snap, &doc)
	if err := o.saveSnapshots(snap, &doc); err != nil {
		return err
	}

	// The env's declared decisions, best effort.
	var decisions []mlrecs.Decision
	connected := ""
	if o.env != "" {
		d, cc, note := mlEnvDeclared(ctx, o.render, o.projectDir, o.env, snap.Context)
		decisions, connected = d, cc
		if note != "" {
			doc.Warnings = append(doc.Warnings, note)
		}
	}

	// One set of proposal options for both: the decisions are written back
	// into the ml.k (so --write keeps them), and each finding's kcl_diff is
	// taken against the very file Propose writes.
	popts := mlrecs.ProposeOptions{Namespace: o.namespace, ConnectedCluster: connected, Decisions: decisions}
	findings := o.evaluate(snap, inv, mlrecs.Options{Decisions: decisions, Propose: popts})
	inv.Coverage = mlrecs.LinkCoverage(inv.Coverage, findings)
	mlk := o.propose(inv, findings, popts)

	doc.Inventory = *inv
	if findings != nil {
		doc.Findings = findings
	}
	doc.Warnings = append(mlInventoryWarnings(snap, inv), doc.Warnings...)
	if doc.Proposal, err = o.placeProposal(path, mlk); err != nil {
		return err
	}

	if o.jsonOut {
		enc := json.NewEncoder(o.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	return writeMLDiscoverHuman(o.stdout, doc, o)
}

// withDefaults fills every unset pipeline step with the real one.
func (o mlDiscoverOptions) withDefaults() mlDiscoverOptions {
	if o.collect == nil {
		o.collect = mlCollect
	}
	if o.render == nil {
		o.render = mlEnvRender
	}
	if o.evaluate == nil {
		o.evaluate = mlrecs.Evaluate
	}
	if o.propose == nil {
		o.propose = mlrecs.Propose
	}
	if o.probeTargets == nil {
		o.probeTargets = mldiscover.ProbeTargets
	}
	if o.runProbe == nil {
		o.runProbe = mlProbe
	}
	if o.stderr == nil {
		o.stderr = io.Discard
	}
	return o
}

// validate refuses flag combinations that cannot work, before anything is
// read, and returns where the proposal goes ("" for printed only).
func (o mlDiscoverOptions) validate() (string, error) {
	if o.probe && o.fromSnapshot != "" {
		return "", errors.New("--probe reads a live cluster: it cannot run on --from-snapshot (a snapshot saved with --probe already holds its probes)")
	}
	if o.fromSnapshot != "" && o.kubeContext != "" {
		return "", errors.New("--from-snapshot and --context are exclusive: a snapshot records the context it was read through")
	}
	if o.env != "" && o.projectDir == "" {
		return "", fmt.Errorf("--env %s needs a forge project (no forge.yaml here or above); outside one, place the proposal with --out <file>", o.env)
	}
	if o.env != "" && !mlEnvName.MatchString(o.env) {
		return "", fmt.Errorf("--env %q is not an env name", o.env)
	}
	path := mlProposalPath(o)
	if o.write && path == "" {
		if o.projectDir == "" {
			return "", errors.New("--write outside a forge project needs --out <file>: there is no deploy/kcl/<env>/ to write into")
		}
		return "", errors.New("--write needs --env <env> (writes deploy/kcl/<env>/ml.k) or --out <file>")
	}
	return path, nil
}

// read is the snapshot: a saved one, or a live read of the cluster.
func (o mlDiscoverOptions) read(ctx context.Context) (*mldiscover.Snapshot, error) {
	if o.fromSnapshot != "" {
		return readMLSnapshot(o.fromSnapshot)
	}
	s, err := o.collect(ctx, o.kubeContext)
	if err != nil {
		if errors.Is(err, mlcollect.ErrUnreachable) {
			return nil, undeterminedf("forge ml discover: %v", err)
		}
		return nil, fmt.Errorf("forge ml discover: %w", err)
	}
	return s, nil
}

// discover runs discovery over the snapshot. With --probe the first pass
// names the probe targets, the probe's results go into the snapshot, and the
// second pass is refined by them.
func (o mlDiscoverOptions) discover(ctx context.Context, snap *mldiscover.Snapshot, doc *MLDiscoverDocument) *mldiscover.Inventory {
	inv := mldiscover.Discover(snap)
	if !o.probe {
		return inv
	}
	targets := o.probeTargets(snap, inv)
	probes, err := o.runProbe(ctx, snap.Context, targets)
	if err != nil {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf("--probe: %v", err))
	}
	snap.Probes = probes
	if len(targets) == 0 {
		doc.Warnings = append(doc.Warnings, "--probe: no metrics endpoint to probe (no exporter with a running pod and a metrics port was found)")
	}
	return mldiscover.Discover(snap)
}

// saveSnapshots writes a live read to .forge/state inside a project (a
// failure is a warning) and to --save-snapshot (a failure is an error).
func (o mlDiscoverOptions) saveSnapshots(snap *mldiscover.Snapshot, doc *MLDiscoverDocument) error {
	if o.fromSnapshot == "" && o.projectDir != "" {
		state := mlStateFile(o.projectDir, snap.Context)
		if err := writeMLSnapshot(state, snap); err != nil {
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("could not save the snapshot to %s: %v", state, err))
		} else {
			doc.Snapshot = displayMLPath(state)
		}
	}
	if o.saveSnapshot != "" {
		if err := writeMLSnapshot(o.saveSnapshot, snap); err != nil {
			return fmt.Errorf("--save-snapshot: %w", err)
		}
		if doc.Snapshot == "" {
			doc.Snapshot = displayMLPath(o.saveSnapshot)
		}
	}
	return nil
}

// placeProposal compares the proposal with the file at path and, with
// --write, writes it when it differs.
func (o mlDiscoverOptions) placeProposal(path, mlk string) (MLDiscoverProposal, error) {
	p := MLDiscoverProposal{MLK: mlk}
	if path == "" {
		return p, nil
	}
	p.Path = displayMLPath(path)
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		p.Exists = true
		p.Diff = mlUnifiedDiff(string(existing), mlk, path)
	case !errors.Is(err, os.ErrNotExist):
		return p, fmt.Errorf("read %s: %w", path, err)
	}
	if o.write && mlk != "" && (!p.Exists || string(existing) != mlk) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return p, err
		}
		if err := atomicfile.WriteFile(path, []byte(mlk), 0o644); err != nil {
			return p, fmt.Errorf("write %s: %w", path, err)
		}
		p.Written = true
	}
	return p, nil
}

var mlEnvName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// mlProposalPath is where the ml.k goes: --out, else the env's directory
// inside a project, else nowhere (printed only).
func mlProposalPath(o mlDiscoverOptions) string {
	if o.out != "" {
		return o.out
	}
	if o.env != "" && o.projectDir != "" {
		return filepath.Join(o.projectDir, "deploy", "kcl", o.env, "ml.k")
	}
	return ""
}

// displayMLPath shows a path relative to the working directory when it is
// under it.
func displayMLPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return abs
}

func readMLSnapshot(path string) (*mldiscover.Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--from-snapshot: %w", err)
	}
	var s mldiscover.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("--from-snapshot %s: not a snapshot: %w", path, err)
	}
	if s.Schema != mldiscover.SnapshotSchema {
		return nil, fmt.Errorf("--from-snapshot %s: schema %q, want %q", path, s.Schema, mldiscover.SnapshotSchema)
	}
	return &s, nil
}

func writeMLSnapshot(path string, s *mldiscover.Snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Owner-only: a snapshot is redacted, but it still maps the cluster.
	// atomicfile keeps an existing file's mode, so narrow one an older
	// release wrote 0644.
	if err := atomicfile.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// mlInventoryWarnings is the inventory's warnings plus every read the
// snapshot records as refused that they do not already name.
func mlInventoryWarnings(s *mldiscover.Snapshot, inv *mldiscover.Inventory) []string {
	out := append([]string{}, inv.Warnings...)
	for _, re := range s.ReadErrors {
		named := false
		for _, w := range inv.Warnings {
			named = named || strings.Contains(w, re.Resource)
		}
		if !named {
			out = append(out, fmt.Sprintf("could not read %s: %s", re.Resource, re.Error))
		}
	}
	return out
}

// mlRenderedStack is the part of a rendered forge.MLStack discover reads:
// its cluster (a ClusterTarget, whose `cluster` is the kube context) and its
// RecommendationDecisions.
type mlRenderedStack struct {
	Cluster   json.RawMessage   `json:"cluster"`
	Decisions []mlrecs.Decision `json:"decisions"`
}

func (s mlRenderedStack) context() string {
	var target struct {
		Cluster string `json:"cluster"`
	}
	if json.Unmarshal(s.Cluster, &target) == nil && target.Cluster != "" {
		return target.Cluster
	}
	var name string
	_ = json.Unmarshal(s.Cluster, &name)
	return name
}

// mlEnvDeclared reads what the env already declares for this cluster: the
// decisions of the ml_stack whose cluster is kubeContext, and the connected
// cluster bound to that context. Best effort: an env that does not render,
// or a forge whose KCL has no ml_stacks yet, yields nothing and a note.
func mlEnvDeclared(ctx context.Context, render func(context.Context, string, string) ([]byte, error), projectDir, env, kubeContext string) ([]mlrecs.Decision, string, string) {
	mainK := filepath.Join(projectDir, "deploy", "kcl", env, "main.k")
	if !envHasDeclaration(projectDir, env, mainK) {
		return nil, "", fmt.Sprintf("env %s has no declaration (%s): no RecommendationDecisions applied", env, displayMLPath(mainK))
	}
	raw, err := render(ctx, projectDir, env)
	if err != nil {
		return nil, "", fmt.Sprintf("env %s did not render, so its RecommendationDecisions are not applied: %v", env, err)
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, "", fmt.Sprintf("env %s's render is not JSON, so its RecommendationDecisions are not applied: %v", env, err)
	}
	if inner, ok := wrapper["output"]; ok && len(inner) > 0 {
		wrapper = nil
		if err := json.Unmarshal(inner, &wrapper); err != nil {
			return nil, "", fmt.Sprintf("env %s's render output is not an object: %v", env, err)
		}
	}
	var connected map[string]string
	_ = json.Unmarshal(wrapper["connected_clusters"], &connected)
	cc := connected[kubeContext]
	stacksRaw, ok := wrapper["ml_stacks"]
	if !ok {
		return nil, cc, ""
	}
	var stacks []mlRenderedStack
	if err := json.Unmarshal(stacksRaw, &stacks); err != nil {
		return nil, cc, fmt.Sprintf("env %s's ml_stacks did not parse, so its RecommendationDecisions are not applied: %v", env, err)
	}
	for _, s := range stacks {
		if s.context() == kubeContext {
			return s.Decisions, cc, ""
		}
	}
	if len(stacks) > 0 {
		return nil, cc, fmt.Sprintf("env %s declares no ml_stack for kube context %s: no RecommendationDecisions applied", env, kubeContext)
	}
	return nil, cc, ""
}

// mlUnifiedDiff is the change from the file on disk to the proposal, "" when
// they are the same.
func mlUnifiedDiff(from, to, path string) string {
	if from == to {
		return ""
	}
	name := filepath.ToSlash(displayMLPath(path))
	d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(from), B: difflib.SplitLines(to),
		FromFile: "a/" + name, ToFile: "b/" + name, Context: 3,
	})
	if err != nil {
		return ""
	}
	return d
}

// writeMLRules prints the rule catalogue.
func writeMLRules(w io.Writer, asJSON bool) error {
	rules := mlrecs.Rules()
	if rules == nil {
		rules = []mlrecs.Rule{}
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Rules []mlrecs.Rule `json:"rules"`
		}{rules})
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSEVERITY\tVENDOR\tDETECTS\tFIX")
	for _, r := range rules {
		vendor := r.Vendor
		if vendor == "" {
			vendor = "any"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Severity, vendor, r.Detects, r.Fix)
	}
	return tw.Flush()
}
