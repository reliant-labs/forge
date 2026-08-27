// File: internal/cli/env_render.go
//
// `forge env render <env>` — print the Kubernetes objects an environment
// renders, and the cluster each one lands on, without touching a cluster.
//
// The question this answers is "what does this environment actually own",
// and until now forge had no way to ask it. `kcl run` on a real project
// fails, because a forge project's KCL calls kcl_plugin.forge.* and only
// forge registers that namespace. `forge doctor` renders every environment
// internally and then reports a COUNT ("5 env(s), 341 manifest(s)") — the
// objects it read never leave the check. `forge env deploy --dry-run` does
// print manifests, but it is the deploy command: it resolves a kubectl
// context, runs the declared-cluster guard, may stand up a k3d cluster and
// push images, and refuses outright when the recorded build is behind HEAD.
// None of that is available to someone who only wants to LOOK.
//
// So the answer got reconstructed by hand, and the reconstruction is where
// the damage happens. An audit of which objects an environment owns read
// 1,886 lines of KCL and inferred each workload's target lexically — HOST
// from the presence of a `host = forge.HostOverrides` block, cluster from a
// per-service `k8s = K8sOverrides{cluster = ...}` override a thousand lines
// further down. One misread there deletes the wrong object out of the wrong
// cluster.
//
// CLUSTER ATTRIBUTION is therefore not decoration, it is the point. An
// environment renders ONE manifest stream but may deploy it to SEVERAL
// clusters (control-plane's dev puts most workloads on k3d-control-plane and
// workspace-proxy on k3d-cp-daemon), and which document goes where is
// decided by internal/cluster.ScopeManifestsToGroup at apply time. This
// command calls THAT function, one document at a time, rather than modelling
// its rules again — a printer that disagrees with the router would describe
// a deploy that never happens, which is the failure mode it exists to
// prevent.
//
// SIDE EFFECTS: forge cannot promise this is a pure read, and does not.
// KCL's `file.write` runs during evaluation, so a project whose deploy KCL
// generates a file (control-plane's dev main.k calls `nats.write_conf`,
// which materialises deploy/nats/nats.conf) writes it every time anything
// renders — `forge doctor` and this command included. forge has no hook to
// suppress a project's own writes: the write happens inside the embedded KCL
// runtime, which offers no read-only mode, and the alternatives (rendering
// from a copy of the tree, sandboxing the process) are neither portable nor
// cheap. What forge CAN do it does: it reverts its own render-state writes
// (the resolve_port store), and it watches the tree so the report is
// evidence rather than a promise. See renderWriteScan.
package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// renderedObject is one document of the env's rendered manifest stream,
// plus the clusters it lands on.
//
// Clusters is a LIST because routing genuinely replicates: an unattributed
// env-level resource (a Namespace, a ConfigMap left on the bundle rather
// than on an infra service) is applied to EVERY cluster the env deploys to —
// ScopeManifestsToGroup keeps it for every group rather than guessing a
// primary. Printing such a document once, with both clusters named, keeps
// the object count equal to the render's own count (what `forge doctor`
// reports) while still saying where it goes. `--cluster` then narrows to
// exactly the stream one cluster receives.
type renderedObject struct {
	// Doc is the document's exact rendered YAML, trimmed of surrounding
	// whitespace — the bytes `kubectl apply` would read.
	Doc       string
	Kind      string
	Name      string
	Namespace string
	// App is the `app.kubernetes.io/name` label: the KCL-declared deploy
	// GROUP (`forge env deploy --target`), empty for env-shared resources
	// that belong to no service.
	App      string
	Clusters []string
}

// renderedObjectMeta is the slice of a manifest this command reads. Anything
// else is passed through untouched — the printer's job is to hand back the
// render, not to reformat it.
type renderedObjectMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

func newEnvRenderCmd() *cobra.Command {
	var (
		tag          string
		namespace    string
		clusterName  string
		kinds        []string
		nameFilter   string
		targets      []string
		list         bool
		noDigest     bool
		failOnWrite  bool
		noWriteCheck bool
	)

	cmd := &cobra.Command{
		Use:   "render <environment>",
		Short: "Print the Kubernetes objects deploy/kcl/<env>/ renders, with the cluster each lands on",
		Args:  cobra.ExactArgs(1),
		Long: `Print the manifests ` + Name() + ` env deploy would apply, as a ` + "`---`" + `-separated
YAML stream, without contacting a cluster.

Every document is preceded by a ` + "`# cluster:`" + ` comment naming the cluster(s) it
lands on. An environment renders one stream but may deploy it to several
clusters, and the routing is decided per document by the same function the
deploy performs it with (internal/cluster.ScopeManifestsToGroup): a document
carrying the first-class ` + "`forge.dev/cluster`" + ` label goes to that cluster; otherwise
one labelled ` + "`app.kubernetes.io/name`" + ` goes to the cluster its owning workload
declares; otherwise it is replicated to EVERY cluster the environment deploys
to. A replicated document is printed ONCE with every cluster named, so the
object count matches the render's own — pass --cluster to see exactly the
stream one cluster receives.

The render is READ-ONLY as far as forge is concerned: no kubectl context is
resolved, no cluster is created, no image is built or pushed, and none of the
deploy-time refusals (stale build state, declared-context guard) apply. It is
NOT guaranteed pure, because KCL evaluates ` + "`file.write`" + `: a project whose deploy
KCL generates a file writes it on every render, forge's included. Rather than
promise otherwise, ` + Name() + ` watches the tree and reports on stderr every file that
changed while rendering; --fail-on-write turns that report into a non-zero
exit for callers that need the guarantee.

Exits non-zero with the KCL error when the environment does not render, so it
is usable as a CI gate.

Examples:
  ` + Name() + ` env render dev                              # every object, cluster-annotated
  ` + Name() + ` env render dev --list                       # one line per object (kind/name/cluster)
  ` + Name() + ` env render dev --cluster k3d-cp-daemon      # only what that cluster receives
  ` + Name() + ` env render prod --kind Deployment,Job       # only those kinds
  ` + Name() + ` env render prod --target workspace-proxy    # only that app's objects
  ` + Name() + ` env render prod | kubectl diff -f -         # diff the render against a live cluster
  ` + Name() + ` env render dev --fail-on-write >/dev/null   # assert the render touched nothing`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvRender(cmd, args[0], envRenderOptions{
				imageTag:     tag,
				namespace:    namespace,
				cluster:      clusterName,
				kinds:        kinds,
				name:         nameFilter,
				targets:      targets,
				list:         list,
				noDigest:     noDigest,
				failOnWrite:  failOnWrite,
				noWriteCheck: noWriteCheck,
			})
		},
	}

	cmd.Flags().StringVar(&tag, "tag", "", "Image tag to render with (default: the same source forge env deploy reads — .forge/state/build-<env>.json, then git describe)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override the namespace the environment declares")
	cmd.Flags().StringVar(&clusterName, "cluster", "", "Print ONLY the objects that land on this cluster (the kubectl context named by forge.K8sCluster.cluster)")
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "Print only these kinds (comma-separated or repeated; case-insensitive)")
	cmd.Flags().StringVar(&nameFilter, "name", "", "Print only objects with this metadata.name")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "Print only the named application's objects (the app.kubernetes.io/name group forge env deploy --target selects; repeatable)")
	cmd.Flags().BoolVar(&list, "list", false, "Print one line per object (cluster, kind, namespace, name) instead of the YAML stream")
	cmd.Flags().BoolVar(&noDigest, "no-digest", false, "Render image references as the mutable :tag even when a build state captured an immutable digest (matches forge env deploy --no-digest)")
	cmd.Flags().BoolVar(&failOnWrite, "fail-on-write", false, "Exit non-zero if any file in the project changed while rendering (the render is not guaranteed side-effect-free — see the command description)")
	cmd.Flags().BoolVar(&noWriteCheck, "no-write-check", false, "Skip the before/after scan that detects files the render wrote")

	return cmd
}

// envRenderOptions is the command's resolved input. Grouped so the flag
// wiring and the implementation don't drift apart argument by argument.
type envRenderOptions struct {
	imageTag     string
	namespace    string
	cluster      string
	kinds        []string
	name         string
	targets      []string
	list         bool
	noDigest     bool
	failOnWrite  bool
	noWriteCheck bool
}

func runEnvRender(cmd *cobra.Command, envName string, opts envRenderOptions) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()

	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	projectDir := projectDirForKCL()

	kclDir := store.K8s().KCLDir
	if kclDir == "" {
		kclDir = "deploy/kcl"
	}
	mainK := filepath.Join(kclDir, envName, "main.k")
	if _, serr := os.Stat(mainK); serr != nil {
		envs, _ := ListEnvs(projectDir)
		return fmt.Errorf("environment %q not found: %s does not exist (declared environments: %s)",
			envName, mainK, strings.Join(envs, ", "))
	}

	// Arm the SAME render context `forge env deploy` arms before its first
	// render: option("worktree")/option("branch") and the port primitives.
	// Without it a keyed allocate_port / resolve_port resolves differently
	// here than at deploy time, and the printed manifests would be a stack
	// nobody deploys. The returned restore is deliberately unused —
	// pinFile below is the narrower version of it, reverting the store only
	// when the render actually moved it so an untouched file keeps even its
	// mtime (and stays out of the write report as a phantom forge wrote).
	activateDevStack(projectDir, envName)
	unpin := pinFile(filepath.Join(projectDir, ".forge", "ports-"+envName+".json"))

	scan := newRenderWriteScan(projectDir, opts.noWriteCheck)

	// Report what the render touched no matter how it ends: a render that
	// fails half-way has still run whatever file.write it reached, and
	// that is exactly when a caller most needs to know.
	defer func() {
		unpin()
		scan.report(errOut, envName)
	}()

	entities, kerr := RenderKCL(ctx, projectDir, envName)
	if kerr != nil {
		return fmt.Errorf("render deploy/kcl/%s/: %w", envName, kerr)
	}

	namespace := opts.namespace
	if namespace == "" {
		if ns := k8sClusterFieldFromEntities(entities, "namespace"); ns != "" {
			namespace = ns
		} else {
			namespace = store.Meta().Name + "-" + envName
		}
	}

	imageTag, tagSource := renderImageTag(ctx, projectDir, envName, opts.imageTag)
	digests, boundRelease, derr := resolveDeployDigests(projectDir, envName, opts.noDigest)
	if derr != nil {
		// Digest pinning is an optimisation of WHICH bytes deploy ships, not
		// of what the environment declares. A caller reading the object graph
		// is better served by a render on the mutable tag than by no render.
		fmt.Fprintf(errOut, "warning: image digests unresolved (%v) — rendering on tag %q instead\n", derr, imageTag)
		digests = nil
	}

	manifests, rerr := cluster.RenderManifests(ctx, mainK, imageTag, namespace, envName, loadDeployEnvConfigKV(projectDir, envName), digests)
	if rerr != nil {
		return fmt.Errorf("render %s: %w", mainK, rerr)
	}
	// Counted BEFORE any filter, so the summary's "N of M rendered" measures
	// the environment rather than the slice of it being printed. A --target
	// view that reported its own 8 objects as the whole env would understate
	// exactly what an audit is trying to size.
	totalRendered := len(cluster.SplitManifestDocs(manifests))

	if len(opts.targets) > 0 {
		// Reuse the deploy path's own validation and filter: a typo'd
		// --target that silently prints nothing is the worst possible
		// answer to "does this app own anything".
		if verr := validateDeployTargets(entities, opts.targets); verr != nil {
			return verr
		}
		manifests = cluster.SelectManifestsByGroup(manifests, opts.targets)
	}

	groups, gerr := buildDeployGroupsForEnv(envName, entities, namespace, imageTag, false)
	if gerr != nil {
		return gerr
	}

	objects, clusters := attributeRenderedObjects(manifests, groups, entities)

	if opts.cluster != "" && !containsString(clusters, opts.cluster) {
		return fmt.Errorf("environment %q deploys to no cluster named %q (it deploys to: %s)",
			envName, opts.cluster, describeClusters(clusters))
	}
	objects = filterRenderedObjects(objects, opts)
	if len(objects) == 0 {
		fmt.Fprintf(errOut, "no objects matched (environment %q rendered %d)\n", envName, totalRendered)
	}

	out := cmd.OutOrStdout()
	if opts.list {
		if werr := writeRenderedTable(out, objects); werr != nil {
			return werr
		}
	} else if werr := writeRenderedStream(out, objects); werr != nil {
		return werr
	}
	writeRenderSummary(errOut, renderProvenance{
		envName:      envName,
		mainK:        mainK,
		tagSource:    tagSource,
		imageTag:     imageTag,
		boundRelease: boundRelease,
		namespace:    namespace,
	}, clusters, objects, totalRendered)

	// The write report runs in the deferred close, so the fail-on-write
	// verdict has to be taken after it — hence the sentinel here and the
	// check in the caller-visible error below.
	if opts.failOnWrite {
		unpin()
		if changes := scan.changes(); len(changes) > 0 {
			return fmt.Errorf("--fail-on-write: %d file(s) changed while rendering %q (see the report on stderr)", len(changes), envName)
		}
	}
	return nil
}

// renderImageTag resolves the tag the manifests are rendered with.
//
// It reads the SAME sources `forge env deploy` does, in the same order —
// --tag, the recorded build state for this env, the env-agnostic `default`
// record a plain `forge build` writes, then `git describe` — but deliberately
// skips the GUARDS layered on top of them there. The stale-build refusal and
// the dirty/untagged warnings exist to stop you SHIPPING an image you have
// moved past; neither has any bearing on reading what an environment
// declares, and a printer that refused because a build record is behind HEAD
// would be unavailable exactly when it is needed — mid-incident, or on a
// machine that has never built this env at all.
//
// An unresolvable tag is not an error either: it is passed as the empty
// string, which leaves the KCL `option("image_tag") or "<default>"` idiom on
// its own default. The objects still render; only their image refs are
// generic.
func renderImageTag(ctx context.Context, projectDir, envName, flagTag string) (tag, source string) {
	if flagTag != "" {
		return flagTag, "--tag"
	}
	for _, key := range buildStateLookupEnvs(envName) {
		st, err := ReadBuildState(projectDir, key)
		if err != nil || st == nil || st.Tag == "" {
			continue
		}
		return st.Tag, ".forge/state/build-" + key + ".json"
	}
	if t, err := resolveImageTag(ctx, envName); err == nil && t != "" {
		return t, "git describe"
	}
	return "", "unresolved (the KCL default applies)"
}

// attributeRenderedObjects splits the env's rendered stream into documents
// and answers, for each, which cluster(s) a deploy would apply it to.
//
// The routing is NOT re-implemented here. Each document is handed to
// internal/cluster.ScopeManifestsToGroup — the function the deploy path uses
// to partition the stream — once per cluster, with that cluster's real
// GroupScope built by the deploy path's own clusterScopeForGroups. A
// document survives for the clusters it belongs to and is dropped for the
// rest, so "which clusters does this land on" is answered by the router
// itself rather than by a second model of its rules that can drift out of
// agreement with it.
//
// Single-cluster environments never scope at all at deploy time
// (ApplyOpts.ClusterScope stays nil, the whole stream applies), so every
// object is attributed to the one cluster. An environment that declares no
// cluster — host-only, compose-only — attributes to none, which is reported
// as such rather than guessed.
func attributeRenderedObjects(manifests string, groups []deploytarget.ServiceGroup, entities *KCLEntities) ([]renderedObject, []string) {
	clusters := envClusterOrder(groups, entities)
	scopes := envClusterScopes(groups, entities)

	docs := cluster.SplitManifestDocs(manifests)
	objects := make([]renderedObject, 0, len(docs))
	for _, doc := range docs {
		obj := renderedObject{Doc: doc}
		var meta renderedObjectMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err == nil {
			obj.Kind = meta.Kind
			obj.Name = meta.Metadata.Name
			obj.Namespace = meta.Metadata.Namespace
			obj.App = meta.Metadata.Labels[cluster.AppNameLabel]
		}
		obj.Clusters = clustersForDoc(doc, clusters, scopes)
		objects = append(objects, obj)
	}
	return objects, clusters
}

// envClusterOrder is every cluster the environment deploys to, sorted.
//
// Deterministic order is not cosmetic: this command is meant to be diffed
// (against a previous render, against a live cluster, in CI), and a set that
// came out of a map would reorder the per-cluster summary and every
// multi-cluster annotation run to run.
//
// Sorted rather than "main cluster first" on purpose. forge does have a
// notion of a main cluster — mainClusterForEntities, the FIRST cluster-shaped
// service in render order, which is what an operator or cronjob attributes to
// — but it is a resolution rule, not a claim about importance, and it does
// not survive contact with a host-mode environment: in control-plane's dev
// every big service runs on the host, so the first CLUSTER-shaped service is
// the lone cross-cluster exception (workspace-proxy on k3d-cp-daemon) and
// "main" names the cluster holding a third of the objects. Printing that
// first would editorialise, wrongly. Alphabetical says nothing it cannot
// back up. The fallback below still uses the rule, because for an
// environment with no deploy groups at all it is the only cluster fact
// there is.
func envClusterOrder(groups []deploytarget.ServiceGroup, entities *KCLEntities) []string {
	seen := map[string]bool{}
	var clusters []string
	for _, g := range groups {
		if g.ProviderID != "k8s-cluster" || g.Cluster == "" || seen[g.Cluster] {
			continue
		}
		seen[g.Cluster] = true
		clusters = append(clusters, g.Cluster)
	}
	if len(clusters) == 0 {
		if main := mainClusterForEntities(entities, groups); main != "" {
			return []string{main}
		}
		return nil
	}
	sort.Strings(clusters)
	return clusters
}

// envClusterScopes builds each cluster's GroupScope through the deploy
// path's clusterScopeForGroups, so the filter this command applies per
// document is byte-for-byte the filter the deploy applies to the stream.
// A nil entry means "no scoping" — the single-cluster case, where the whole
// stream applies.
func envClusterScopes(groups []deploytarget.ServiceGroup, entities *KCLEntities) map[string]*cluster.GroupScope {
	scopeFor := clusterScopeForGroups(groups, entities)
	scopes := map[string]*cluster.GroupScope{}
	for _, g := range groups {
		if g.ProviderID != "k8s-cluster" || g.Cluster == "" {
			continue
		}
		if _, done := scopes[g.Cluster]; done {
			continue
		}
		scopes[g.Cluster] = scopeFor(g)
	}
	return scopes
}

// clustersForDoc answers which of the env's clusters keep this document.
// One cluster (or none) needs no filtering — nothing is partitioned at
// deploy time either.
func clustersForDoc(doc string, clusters []string, scopes map[string]*cluster.GroupScope) []string {
	if len(clusters) <= 1 {
		return clusters
	}
	var lands []string
	for _, c := range clusters {
		scope := scopes[c]
		if scope == nil {
			// No scope for a declared cluster should not happen once the env
			// declares two; keeping the document is the same conservative
			// choice ScopeManifestsToGroup makes for anything it cannot
			// confirm belongs elsewhere.
			lands = append(lands, c)
			continue
		}
		if strings.TrimSpace(cluster.ScopeManifestsToGroup(doc, *scope)) != "" {
			lands = append(lands, c)
		}
	}
	return lands
}

// filterRenderedObjects narrows the printed set. Filters compose (AND), and
// each is exact rather than fuzzy: a substring match on --name would quietly
// widen an audit that is about to delete something.
func filterRenderedObjects(objects []renderedObject, opts envRenderOptions) []renderedObject {
	kinds := map[string]bool{}
	for _, k := range opts.kinds {
		if k = strings.TrimSpace(k); k != "" {
			kinds[strings.ToLower(k)] = true
		}
	}
	out := make([]renderedObject, 0, len(objects))
	for _, o := range objects {
		if opts.cluster != "" && !containsString(o.Clusters, opts.cluster) {
			continue
		}
		if len(kinds) > 0 && !kinds[strings.ToLower(o.Kind)] {
			continue
		}
		if opts.name != "" && o.Name != opts.name {
			continue
		}
		out = append(out, o)
	}
	return out
}

// writeRenderedStream prints the documents as a `---`-separated YAML stream,
// each preceded by a comment naming the cluster(s) it lands on.
//
// A comment is the right carrier for the attribution: it is legal YAML that
// every consumer ignores, so the stream still pipes into `kubectl apply -f -`
// or `kubectl diff -f -` unchanged, and the fact a reader most needs is
// attached to the object rather than to a section header they have to scroll
// back to.
func writeRenderedStream(w io.Writer, objects []renderedObject) error {
	for i, o := range objects {
		if i > 0 {
			if _, err := fmt.Fprintln(w, "---"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "# cluster: %s\n", describeClusters(o.Clusters)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, o.Doc); err != nil {
			return err
		}
	}
	return nil
}

// writeRenderedTable prints one line per object, in render order (which is
// apply order). The inventory view: what an audit reads before it decides
// anything.
//
// APP is the object's `app.kubernetes.io/name` label — the deploy GROUP, and
// the same key `forge env deploy --target` selects on. It earns a column
// because it is the OWNERSHIP fact routing is decided by, and because it is
// the one an audit otherwise guesses: an object with no app is env-shared
// and belongs to no service (shown as a dash), which is exactly the class
// that gets replicated to every cluster.
func writeRenderedTable(w io.Writer, objects []renderedObject) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLUSTER\tKIND\tNAMESPACE\tNAME\tAPP")
	for _, o := range objects {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			describeClusters(o.Clusters), orDash(o.Kind), orDash(o.Namespace), orDash(o.Name), orDash(o.App))
	}
	return tw.Flush()
}

// writeRenderSummary states, on stderr, what was rendered and from what —
// the provenance a reader needs to trust the stream on stdout, kept off
// stdout so the stream stays pipeable.
//
// Per-cluster counts can sum to MORE than the total: an unattributed
// env-level resource is applied to every cluster, and is counted for each.
// The line says so rather than leaving a reader to find the discrepancy.
// renderProvenance is where a render came from: the env and its main.k, the
// image tag and what resolved it, the namespace, and the release the env is
// promoted to (empty when it is not). Grouped into one value because they are
// one fact — the answer to "what produced this stream" — and because passing
// six strings positionally invites a silent swap of two of them.
type renderProvenance struct {
	envName      string
	mainK        string
	tagSource    string
	imageTag     string
	boundRelease string
	namespace    string
}

func writeRenderSummary(w io.Writer, p renderProvenance, clusters []string, objects []renderedObject, total int) {
	envName, mainK := p.envName, p.mainK
	tagSource, imageTag := p.tagSource, p.imageTag
	boundRelease, namespace := p.boundRelease, p.namespace

	fmt.Fprintf(w, "\n[render] %s — %d object(s)", envName, len(objects))
	if len(objects) != total {
		fmt.Fprintf(w, " of %d rendered", total)
	}
	fmt.Fprintf(w, "\n[render]   source:    %s\n", mainK)
	fmt.Fprintf(w, "[render]   namespace: %s\n", namespace)
	if imageTag != "" {
		fmt.Fprintf(w, "[render]   image tag: %s  (source: %s)\n", imageTag, tagSource)
	} else {
		fmt.Fprintf(w, "[render]   image tag: %s\n", tagSource)
	}
	if boundRelease != "" {
		fmt.Fprintf(w, "[render]   release:   %s  (env is promoted to it — image digests pinned)\n", boundRelease)
	}
	if len(clusters) == 0 {
		fmt.Fprintf(w, "[render]   clusters:  none declared (host-only / compose environment)\n")
		return
	}
	counts := map[string]int{}
	replicated := 0
	for _, o := range objects {
		if len(o.Clusters) > 1 {
			replicated++
		}
		for _, c := range o.Clusters {
			counts[c]++
		}
	}
	for _, c := range clusters {
		fmt.Fprintf(w, "[render]   cluster:   %-24s %d object(s)\n", c, counts[c])
	}
	if replicated > 0 {
		fmt.Fprintf(w, "[render]   (%d object(s) land on more than one cluster and are counted for each)\n", replicated)
	}
}

// describeClusters renders a document's landing sites for humans. The
// no-cluster case is spelled out rather than blank: an empty column in an
// audit reads as "nothing here", and the truth is "this environment declares
// no cluster at all".
func describeClusters(clusters []string) string {
	if len(clusters) == 0 {
		return "(none declared)"
	}
	return strings.Join(clusters, ", ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pinFile snapshots path's bytes, mode and mtime, and returns a revert that
// puts all three back — but only if something actually changed them.
//
// Two departures from the unconditional restore forge's `env up` path uses,
// both for the same reason: this command REPORTS every file that changed on
// disk while rendering, so forge's own restore must not appear in that
// report. A restore that rewrote the file either way would list it as
// touched; one that rewrote it without replacing the mtime would list it as
// rewritten. Both train a reader to ignore exactly the signal the report
// exists to carry.
//
// Returned funcs are idempotent — the first call reverts, later ones observe
// the reverted content and do nothing — so a caller may both defer it and
// call it early.
func pinFile(path string) func() {
	before, rerr := os.ReadFile(path)
	info, serr := os.Stat(path)
	existed := rerr == nil && serr == nil
	return func() {
		after, aerr := os.ReadFile(path)
		switch {
		case existed && aerr == nil && !bytes.Equal(before, after):
			if werr := os.WriteFile(path, before, info.Mode().Perm()); werr == nil {
				_ = os.Chtimes(path, time.Now(), info.ModTime())
			}
		case !existed && aerr == nil:
			_ = os.Remove(path)
		}
	}
}

// ── write detection ─────────────────────────────────────────────────────
//
// forge cannot make a project's KCL stop writing files. `file.write` is a
// KCL builtin evaluated inside the embedded runtime, which exposes no
// read-only mode, and the ways to impose one from outside — render from a
// copy of the tree, sandbox the process — are either expensive (a project
// tree is arbitrarily large) or unportable (per-OS filesystem sandboxing).
// A symlink farm does not help: a write follows the symlink and truncates
// the original.
//
// So this is evidence instead of a promise. Stat every file under the
// project before and after the render and report what moved. It is honest
// about its own limits: it cannot attribute a change to the render (a
// concurrent process writing in the same tree also shows up), and it reads
// size and modification time rather than content, because hashing a real
// project tree costs an order of magnitude more than the render it is
// watching (measured on a 47k-file repo: 0.3s to stat, 7s to hash).

// renderFileStamp is the per-file evidence: enough to see a write, cheap
// enough to collect twice around every render.
type renderFileStamp struct {
	size  int64
	mtime time.Time
}

// renderTreeChange is one file that moved during the render.
type renderTreeChange struct {
	path string
	how  string // "created" | "removed" | "rewritten" | "modified"
	from int64
	to   int64
}

// renderWriteScan holds the before-picture and answers what changed.
type renderWriteScan struct {
	root    string
	enabled bool
	before  map[string]renderFileStamp
	scanned int
	err     error
	// after is memoised so report() and the --fail-on-write verdict cannot
	// disagree about what happened (and so a second walk is not paid for).
	after   []renderTreeChange
	settled bool
}

// renderScanSkipDirs are directories excluded from the walk, and both
// entries are forge or git talking to themselves rather than anything a
// render could produce:
//
//   - .git — object churn from any concurrent git command, never a render
//     target.
//   - .forge/logs — the per-process log files forge's own dev loop appends
//     to continuously; on a machine with a stack up they change during ANY
//     command, and listing them would bury the one line that matters.
//
// Everything else is scanned, including generated code, vendored trees and
// node_modules: a `file.write` can name any path, and deciding in advance
// that some are uninteresting is how a write goes unreported.
var renderScanSkipDirs = map[string]bool{
	".git":        true,
	".forge/logs": true,
}

func newRenderWriteScan(root string, disabled bool) *renderWriteScan {
	s := &renderWriteScan{root: root, enabled: !disabled}
	if !s.enabled {
		return s
	}
	s.before, s.scanned, s.err = scanRenderTree(root)
	return s
}

// changes walks the tree a second time and diffs it against the before
// picture. Memoised: the first call settles the verdict.
func (s *renderWriteScan) changes() []renderTreeChange {
	if !s.enabled || s.err != nil || s.settled {
		return s.after
	}
	s.settled = true
	after, _, err := scanRenderTree(s.root)
	if err != nil {
		s.err = err
		return nil
	}
	s.after = diffRenderTrees(s.before, after)
	return s.after
}

// report states the outcome on stderr — always, including the clean case.
// "No files changed" is the claim a read-only auditor came for, and a check
// that only speaks when it fails cannot be distinguished from one that was
// never run.
func (s *renderWriteScan) report(w io.Writer, envName string) {
	if !s.enabled {
		fmt.Fprintln(w, "[render] write check skipped (--no-write-check): forge cannot say whether this render wrote files")
		return
	}
	if s.err != nil {
		fmt.Fprintf(w, "[render] write check unavailable (%v): forge cannot say whether this render wrote files\n", s.err)
		return
	}
	changes := s.changes()
	if len(changes) == 0 {
		fmt.Fprintf(w, "[render] wrote nothing: %d path(s) unchanged across the render\n", s.scanned)
		return
	}
	fmt.Fprintf(w, "[render] NOT side-effect-free — %d file(s) changed while rendering %q:\n", len(changes), envName)
	for _, c := range changes {
		switch c.how {
		case "rewritten":
			fmt.Fprintf(w, "[render]   %s  rewritten (%d bytes, size unchanged)\n", c.path, c.to)
		case "modified":
			fmt.Fprintf(w, "[render]   %s  modified (%d -> %d bytes)\n", c.path, c.from, c.to)
		case "created":
			fmt.Fprintf(w, "[render]   %s  created (%d bytes)\n", c.path, c.to)
		case "removed":
			fmt.Fprintf(w, "[render]   %s  removed (was %d bytes)\n", c.path, c.from)
		}
	}
	fmt.Fprintln(w, "[render] KCL evaluates file.write during rendering and forge cannot suppress a project's own writes.")
	fmt.Fprintln(w, "[render] Caveats: size and mtime are compared, not content, so a rewrite may be byte-identical;")
	fmt.Fprintln(w, "[render] .git/ and .forge/logs/ are not scanned; a concurrent process writing here also appears above.")
}

// scanRenderTree stats every regular file under root, keyed by root-relative
// path. Symlinks are recorded by their own metadata and never followed —
// following would make a link into a repo-external tree look like part of
// this project.
func scanRenderTree(root string) (map[string]renderFileStamp, int, error) {
	stamps := map[string]renderFileStamp{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not a reason to abandon the scan; the
			// rest of the tree is still evidence.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if rel != "." && renderScanSkipDirs[filepath.ToSlash(rel)] {
				return fs.SkipDir
			}
			if renderScanSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		stamps[filepath.ToSlash(rel)] = renderFileStamp{size: info.Size(), mtime: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return stamps, len(stamps), nil
}

// diffRenderTrees reports the files that differ between two stat snapshots,
// sorted by path so the report is stable and diffable.
//
// "rewritten" (same size, later mtime) is kept distinct from "modified"
// (size changed) because it is the shape a deterministic generator produces
// — control-plane's nats.conf is rewritten byte-identically on every render
// — and collapsing the two would make a harmless regeneration look like a
// content change.
func diffRenderTrees(before, after map[string]renderFileStamp) []renderTreeChange {
	var changes []renderTreeChange
	for path, a := range after {
		b, existed := before[path]
		switch {
		case !existed:
			changes = append(changes, renderTreeChange{path: path, how: "created", to: a.size})
		case a.size != b.size:
			changes = append(changes, renderTreeChange{path: path, how: "modified", from: b.size, to: a.size})
		case !a.mtime.Equal(b.mtime):
			changes = append(changes, renderTreeChange{path: path, how: "rewritten", from: b.size, to: a.size})
		}
	}
	for path, b := range before {
		if _, still := after[path]; !still {
			changes = append(changes, renderTreeChange{path: path, how: "removed", from: b.size})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	return changes
}
