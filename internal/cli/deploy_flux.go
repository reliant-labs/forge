package cli

// `forge env deploy <env>` for an env that RECONCILES ITSELF: write the
// desired-state pointer, then watch.
//
// WHAT THIS REPLACES, AND WHAT IT DOES NOT. For an env matching
// [reconcilesThroughFlux], this runs INSTEAD of the client-side apply — not
// beside it. That is the point of the whole path: forge writes two objects per
// cluster and Flux applies the env, so the thing that records the release and
// the thing that converges the cluster are different processes. Running both
// would make two actors authorities over the same objects, and the one that
// lost would spend every interval reverting the other.
//
// IT RUNS AFTER THE PROMOTION IS RECORDED, and that order is the ledger's
// contract rather than a convenience. The promotion is the record that this
// release is the env's desired state; the pointer is a cluster-side projection
// of that record. Writing the pointer first would mean a cluster converging to
// a release the ledger never recorded — unexplainable to every later reader,
// and unrecoverable, since nothing would know to re-point it. This is why it
// hangs off followPromote, after applyPromotePlan, in the slot the
// client-side apply occupies for every other self-managed env.
//
// THE THREE EXIT CODES ARE THE CONTRACT (and they are the table every other
// deploy verb already uses — exitcodes.go):
//
//	0  every Kustomization reports lastAppliedRevision == the digest AND
//	   Ready=True. The release is live.
//	1  a Kustomization reports Ready=False for a real reason, and the reason
//	   is FLUX'S OWN, relayed verbatim. We looked, and it is wrong.
//	8  the budget expired while it was still progressing. Retrying the WAIT
//	   is correct; re-deploying is not, and that distinction is why this is
//	   not folded into 1.
//
// There is deliberately no fourth code for "the pointer wrote but the cluster
// never acknowledged it": that is 8. An unreachable cluster fails the WRITE,
// which is an ordinary error, because a pointer that did not land is not a
// convergence question at all.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/flux"
	"github.com/reliant-labs/forge/pkg/release"
)

// fluxPointerInput is one cluster's pointer, as the deploy assembles it.
type fluxPointerInput struct {
	env          string
	repository   string
	digest       string
	cluster      string
	clusterPaths []release.BundleClusterTree
	requestedAt  time.Time
}

// fluxDeployOptions is what the reconciled deploy needs that the command owns.
type fluxDeployOptions struct {
	// Digest is the bundle this deploy pins. Resolved by the caller from
	// the ledger, so the pointer and the promotion name the same bytes.
	Digest string
	// NoWait writes the pointer and returns. The env still converges —
	// that is the nature of a reconciler — so this is "do not gate on it"
	// rather than "do not deploy", and the notice says so.
	NoWait bool
	// Timeout is the whole convergence budget. Zero means
	// [fluxWaitDefaultTimeout].
	Timeout time.Duration
	// DryRun prints the pointer and writes nothing.
	DryRun bool
	// Out and ErrOut are where the human progress and the notices go. Nil
	// means stdout / stderr.
	Out    io.Writer
	ErrOut io.Writer
	// jsonOut mirrors the command's --json: progress goes to stderr so the
	// single document on stdout stays decodable.
	jsonOut bool
}

// The wait's cadences. Separate from envWaitDefaultTimeout because the thing
// being waited on is different: there is no server-side stability window here,
// and the poll is a kubectl against a local apiserver rather than an RPC.
const (
	// fluxWaitDefaultTimeout is the whole budget. 10m covers a cold
	// cluster pulling fresh images for several workloads, plus Flux's own
	// apply-and-health-check, and is short enough that a wedged
	// reconciliation fails the command rather than hanging a CI job to its
	// own timeout with no diagnosis.
	fluxWaitDefaultTimeout = 10 * time.Minute
	// fluxWaitInterval is the poll cadence. One kubectl per cluster per
	// tick against an apiserver forge is already talking to, so 3s is
	// cheap and it is the resolution of the progress line.
	fluxWaitInterval = 3 * time.Second
)

// runFluxDeploy is the whole reconciled deploy: publish, write, wait.
func runFluxDeploy(ctx context.Context, env string, entities *KCLEntities, opts fluxDeployOptions) error {
	out, errOut := opts.writers()
	if !release.ValidDigest(opts.Digest) {
		// NO BUNDLE, NO POINTER, AND NO FALLBACK TO APPLYING DIRECTLY.
		// A pointer needs immutable bytes to name; without a recorded
		// bundle there is nothing for a reconciler to converge to, and
		// quietly applying from this machine instead would be the one
		// behaviour this path exists to remove — the env would ship,
		// look fine, and have no reconciler.
		return fmt.Errorf(
			"env %s reconciles from its bundle, but no bundle is recorded for this release.\n"+
				"  Build one first: forge env build %s\n"+
				"  (a bundle is the artifact the in-cluster reconciler fetches; forge writes a pointer at it\n"+
				"  rather than applying from this machine)", env, env)
	}

	doc, err := fluxBundleDoc(ctx, projectDirForKCL(), env, opts.Digest)
	if err != nil {
		return err
	}
	clusters := fluxTargetClusters(entities, doc.ClusterPaths)
	if len(clusters) == 0 {
		return fmt.Errorf(
			"env %s reconciles from its bundle, but its bundle routes no documents to any cluster it declares.\n"+
				"  The bundle holds %s. A reconciler over a path the artifact does not carry reports Ready\n"+
				"  having applied nothing, so forge writes no pointer rather than converging green over an\n"+
				"  env that was never applied", env, describeClusterPaths(doc.ClusterPaths))
	}

	// SECRETS ARE SYNCED BY FORGE, NOT CARRIED BY THE BUNDLE. Collected (and
	// refused if a named one cannot be supplied) before anything is
	// published or written, so a missing value never leaves a half-deployed
	// env behind.
	var syncSet fluxSecretSet
	if len(doc.Secrets) > 0 {
		if opts.DryRun {
			printFluxSecretPlan(out, env, doc.Secrets, clusters)
		} else if syncSet, err = collectFluxSecrets(ctx, env, entities, doc.Secrets, clusters); err != nil {
			return err
		}
	}

	// CROSS-CLUSTER KUBECONFIG SECRETS are minted by forge the same way: they
	// are Secrets (so not in the bundle) that only `env up` used to create.
	kubeconfigMints := fluxKubeconfigSecrets(entities)
	if opts.DryRun {
		printFluxKubeconfigPlan(out, env, kubeconfigMints)
	}

	repository := ""
	if !opts.DryRun {
		if repository, err = publishBundleForFlux(ctx, projectDirForKCL(), env, entities, opts.Digest); err != nil {
			return err
		}
	} else {
		// A dry run must still SHOW a plausible pointer, and the
		// address is most of what a reader is checking. It is derived
		// from the declaration (never pushed), so it is the same string
		// a real run would write.
		repository = clusterReachableRegistry(bundle.Repository(fluxRegistryBase(entities), env))
	}

	requestedAt := time.Now()
	pointers := make([]flux.Pointer, 0, len(clusters))
	for _, kctx := range clusters {
		p, perr := fluxPointerFor(fluxPointerInput{
			env: env, repository: repository, digest: opts.Digest,
			cluster: kctx, clusterPaths: doc.ClusterPaths, requestedAt: requestedAt,
		})
		if perr != nil {
			return perr
		}
		if p.Empty() {
			// The env declares this cluster and the render routed
			// nothing to it. Said out loud rather than skipped
			// silently: somebody expected objects here, and a
			// source with no Kustomization consuming it is a
			// pointer that fetches bytes nothing applies.
			fmt.Fprintf(errOut, "Note: env %s declares cluster %s, but its bundle routes no documents there; "+
				"no pointer was written for it.\n", env, kctx)
			continue
		}
		pointers = append(pointers, p)
	}
	if len(pointers) == 0 {
		return fmt.Errorf("env %s: no cluster it declares has any documents in its bundle", env)
	}

	if opts.DryRun {
		return printFluxPointers(out, env, opts.Digest, pointers)
	}

	if len(syncSet) > 0 {
		fmt.Fprintf(out, "\nSyncing %s's Secrets to its clusters (values never enter the bundle)\n", env)
		if err := syncFluxSecrets(ctx, env, syncSet, out); err != nil {
			return err
		}
	}

	if len(kubeconfigMints) > 0 {
		namespace := fluxDeployNamespace(ctx, env)
		fmt.Fprintf(out, "\nMinting %s's cross-cluster kubeconfig Secrets (contents never printed)\n", env)
		if err := mintKubeconfigSecretsAs(ctx, kubeconfigMints, ownerNetworkFromClusters(entities.Clusters), namespace, fluxSecretFieldManager, false); err != nil {
			return fmt.Errorf("kubeconfig secrets: %w", err)
		}
	}

	fmt.Fprintf(out, "\nRecording %s's desired state for its in-cluster reconciler (bundle %s)\n",
		env, shortDigest(opts.Digest))
	for _, p := range pointers {
		if err := fluxApply(ctx, p); err != nil {
			return err
		}
		fmt.Fprintf(out, "  %s: %s → %s\n", p.Source.Cluster, p.Source.Name, describeKustomizations(p))
	}

	if opts.NoWait {
		// Flux converges on its own. Saying so is the difference
		// between "forge is done" and "the release is live", and the
		// caller who opted out of the gate is the one who needs to know
		// which they got.
		fmt.Fprintf(out, "\nRecorded. %s is converged by the Flux in its cluster; gate on it with: forge env status %s --wait\n", env, env)
		return nil
	}
	return waitFluxConverged(ctx, env, opts.Digest, pointers, opts, out)
}

// waitFluxConverged polls every Kustomization until all have applied the
// digest and report Ready, or the budget expires.
func waitFluxConverged(ctx context.Context, env, digest string, pointers []flux.Pointer, opts fluxDeployOptions, out io.Writer) error {
	budget := opts.Timeout
	if budget <= 0 {
		budget = fluxWaitDefaultTimeout
	}
	started := time.Now()
	deadline := started.Add(budget)
	fmt.Fprintf(out, "\nWaiting for %s to converge to %s (budget %s)\n", env, shortDigest(digest), budget)

	var last string
	for {
		obs, err := fluxObserve(ctx, pointers, time.Now())
		if err != nil {
			return err
		}
		if obs.Converged(digest) {
			fmt.Fprintf(out, "  converged: %s in %s\n", obs.Summary(digest), time.Since(started).Round(time.Second))
			return nil
		}
		// A FAILURE IS REPORTED IN FLUX'S OWN WORDS, and it exits 1
		// immediately rather than waiting out the budget. This is the
		// opposite default from the hosted rollout gate (env_wait.go,
		// Q4), and deliberately so: there, DEGRADED is an observation
		// of pods that may settle, and exiting on the first one makes
		// the gate flaky. Here the reason comes from Flux having
		// already applied and health-checked, and its failure reasons
		// are terminal for this revision — `build failed`,
		// `health check failed`, `apply failed`. It will retry on its
		// own interval, but nothing about the revision changes, so
		// spending ten more minutes to report the same reason buys
		// nothing. Status.Failing() is what keeps `Progressing` out of
		// this branch.
		if failures := obs.Failures(); len(failures) > 0 {
			return exitCodeError{code: exitWrong, msg: describeFluxFailures(env, failures)}
		}
		if line := obs.Summary(digest); line != last {
			fmt.Fprintf(out, "  %s\n", line)
			last = line
		}
		if !time.Now().Before(deadline) {
			return exitCodeError{code: exitTimedOut, msg: fmt.Sprintf(
				"env %s did not converge to %s within %s: %s.\n"+
					"  Flux keeps reconciling; re-run `forge env status %s --wait` to keep waiting, "+
					"and `kubectl --context %s -n %s describe kustomization %s` for its own view.",
				env, shortDigest(digest), budget, obs.Summary(digest),
				env, pointers[0].Source.Cluster, flux.Namespace, pointers[0].Kustomizations[0].Name)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(fluxWaitInterval):
		}
	}
}

// describeFluxFailures is the exit-1 message: Flux's own reason, per failing
// Kustomization, plus the command that shows more.
//
// Flux's reason is RELAYED, never remapped. forge is reporting another
// system's verdict, and a translation layer would eventually disagree with what
// `flux get kustomization` shows the operator for the same object — leaving
// them to debug the difference rather than the deploy.
func describeFluxFailures(env string, failures []flux.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "env %s failed to converge: %d of its reconcilers report an error\n", env, len(failures))
	for _, f := range failures {
		fmt.Fprintf(&b, "  %s (%s): %s", f.Name, f.Cluster, f.Reason)
		if f.Message != "" {
			fmt.Fprintf(&b, " — %s", strings.TrimSpace(f.Message))
		}
		b.WriteString("\n")
	}
	f := failures[0]
	fmt.Fprintf(&b, "  kubectl --context %s -n %s describe kustomization %s", f.Cluster, flux.Namespace, f.Name)
	return b.String()
}

// fluxBundleDoc reads the recorded bundle's DOCUMENT, for its ClusterPaths.
//
// THE LAYOUT COMES FROM THE ARTIFACT, never from the env's declared cluster
// list. BundleDoc.ClusterPaths lists the paths the layer ACTUALLY HOLDS, and
// the two differ exactly when a declared cluster ends up with no documents
// routed to it — in which case a Kustomization built for it points at a path
// that does not exist, reconciles as "nothing to apply", and reports Ready.
// That is the one failure mode that makes an env look converged while running
// the previous release forever, which is why pkg/release makes the bundle
// describe its own layout and why this reads it rather than recomputing it.
func fluxBundleDoc(ctx context.Context, projectDir, env, digest string) (release.BundleDoc, error) {
	store, err := recordStoreFor(projectDir)
	if err != nil {
		return release.BundleDoc{}, err
	}
	bundles, err := store.store.Bundles(env)
	if err != nil {
		return release.BundleDoc{}, err
	}
	for i := len(bundles) - 1; i >= 0; i-- {
		if bundles[i].Digest != digest {
			continue
		}
		// A ledger RECORD carries the shape but not the layout, so the
		// paths come from the artifact itself. Read from the local
		// layout the build wrote it to, by digest, verified on the way
		// through — see bundle.Fetch.
		return fluxBundleDocFromLayout(ctx, projectDir, digest)
	}
	return release.BundleDoc{}, fmt.Errorf(
		"env %s has no recorded bundle %s.\n  Build one: forge env build %s", env, shortDigest(digest), env)
}

// fluxTargetClusters is every cluster this env has a pointer written into:
// the intersection of what the env DECLARES and what the bundle CARRIES.
//
// The intersection, not either side. A path the bundle holds for a cluster the
// env no longer declares is stale — applying it would write a departed
// cluster's objects somewhere — and a cluster the env declares with no path is
// the empty-Kustomization case the caller reports. Taking the bundle's side
// alone would mean a removed cluster kept being deployed to; taking the
// declaration's alone would mean building Kustomizations over paths that do
// not exist.
func fluxTargetClusters(entities *KCLEntities, trees []release.BundleClusterTree) []string {
	declared := map[string]bool{}
	for _, c := range declaredFluxClusters(entities) {
		declared[c] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range trees {
		c := strings.TrimSpace(t.Cluster)
		// The unclustered tree is skipped: it holds documents the
		// deploy layer attributes to no cluster, which nothing applies.
		if c == "" || t.Documents <= 0 || seen[c] || !declared[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// declaredFluxClusters is every kubectl context the env declares.
//
// It reads the same three places the deploy's own cluster resolution does — the
// env-wide target, each cluster workload's runtime, and each declared
// forge.Cluster — so the pointer is written into exactly the contexts a direct
// apply would have applied to.
func declaredFluxClusters(entities *KCLEntities) []string {
	if entities == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	add(entities.ClusterTarget.field("cluster"))
	for _, w := range entities.WorkloadsOn(RuntimeCluster) {
		add(w.Runtime.Cluster.Cluster)
	}
	for _, c := range entities.Clusters {
		add(c.Context)
	}
	for _, c := range entities.ManifestClusters {
		add(c.Cluster)
	}
	return out
}

func describeClusterPaths(trees []release.BundleClusterTree) string {
	if len(trees) == 0 {
		return "no paths at all"
	}
	parts := make([]string, 0, len(trees))
	for _, t := range trees {
		name := t.Cluster
		if name == "" {
			name = "(no cluster)"
		}
		parts = append(parts, fmt.Sprintf("%s: %d document(s)", name, t.Documents))
	}
	return strings.Join(parts, ", ")
}

func describeKustomizations(p flux.Pointer) string {
	parts := make([]string, 0, len(p.Kustomizations))
	for _, k := range p.Kustomizations {
		parts = append(parts, k.Name)
	}
	return strings.Join(parts, ", ")
}

// printFluxPointers is `--dry-run`: the exact bytes, not a description of
// them.
//
// A preview that summarised would be a second renderer, and a preview that
// disagrees with the write is worse than none — it is what an operator
// trusts.
func printFluxPointers(out io.Writer, env, digest string, pointers []flux.Pointer) error {
	fmt.Fprintf(out, "\n# env %s would record this desired state for its in-cluster reconciler\n", env)
	fmt.Fprintf(out, "# bundle %s\n", digest)
	for _, p := range pointers {
		stream, err := flux.Encode(p.All())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "---\n# cluster: %s\n%s", p.Source.Cluster, stream)
	}
	return nil
}

func (o fluxDeployOptions) writers() (out, errOut io.Writer) {
	out, errOut = o.Out, o.ErrOut
	if out == nil {
		out = os.Stdout
		// Under --json the single document owns stdout, so the human
		// progress goes to stderr — the same rule
		// promoteFollowOptions.notice follows, for the same reason: a
		// notice beside the document makes it undecodable.
		if o.jsonOut {
			out = os.Stderr
		}
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	return out, errOut
}

// The cluster-touching halves, as seams.
//
// Vars rather than an injected interface because what a test of this path has
// to STATE is what the cluster reported — a Kustomization on the previous
// revision, one failing with Flux's own reason, one absent — and those are
// observable exactly here. An interface would push the same two functions
// through a parameter every production caller passes identically.
var (
	fluxApply   = flux.Apply
	fluxObserve = func(ctx context.Context, pointers []flux.Pointer, now time.Time) (flux.Observation, error) {
		out := flux.Observation{At: now}
		for _, p := range pointers {
			got, err := flux.Observe(ctx, p, now)
			if err != nil {
				return flux.Observation{}, err
			}
			out.Statuses = append(out.Statuses, got.Statuses...)
		}
		return out, nil
	}
)

// fluxBundleDocFromLayout reads a recorded bundle's document out of the
// machine ledger's OCI layout, verified by digest.
func fluxBundleDocFromLayout(ctx context.Context, projectDir, digest string) (release.BundleDoc, error) {
	store, err := openMachineLedger(projectDir)
	if err != nil {
		return release.BundleDoc{}, err
	}
	layout, err := bundle.NewLocalLayout(store.OCIDir())
	if err != nil {
		return release.BundleDoc{}, err
	}
	fetched, err := bundle.Fetch(ctx, layout, digest)
	if err != nil {
		return release.BundleDoc{}, fmt.Errorf("read bundle %s from this machine's ledger: %w", shortDigest(digest), err)
	}
	return fetched.Doc, nil
}

// republishLedgerBundle copies the recorded bundle from the machine ledger's
// layout into `base`'s bundle repository, and returns that repository.
//
// See internal/bundle's republish.go for why this copies rather than
// re-renders: the digest is an input, every byte is verified against it, so the
// artifact Flux applies is the one the ledger named or the operation fails.
func republishLedgerBundle(ctx context.Context, projectDir, env, base, digest string) (string, error) {
	store, err := openMachineLedger(projectDir)
	if err != nil {
		return "", err
	}
	layout, err := bundle.NewLocalLayout(store.OCIDir())
	if err != nil {
		return "", err
	}
	repo := bundle.Repository(base, env)
	target, err := bundlePushTarget(repo)
	if err != nil {
		return "", err
	}
	if _, err := bundle.Republish(ctx, layout, target, digest); err != nil {
		return "", fmt.Errorf("publish env %s's bundle %s to %s so its cluster can fetch it: %w",
			env, shortDigest(digest), repo, err)
	}
	return repo, nil
}
