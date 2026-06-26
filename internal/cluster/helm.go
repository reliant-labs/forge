// Package cluster — helm-as-a-RENDERER for declarative platform deps.
//
// THE MODEL: helm is a RENDERER, not an installer. A forge.HelmChart is a
// platform dependency declared in the SAME env Bundle as the app. forge
// runs `helm template <chart> --version <v> --values <vals> -n <ns>
// --skip-crds` to expand the chart to a manifest list, stamps every
// rendered manifest with `app.kubernetes.io/name = <name>`, and folds
// those manifests into the SAME render → kubectl-apply → wait pipeline
// every other forge manifest flows through (Apply, below). There is NO
// `helm install`, NO helm-managed release, NO imperative installer.
//
// SELECTION IS `--target`. Because each chart's manifests carry the
// chart's `name` as their `app.kubernetes.io/name` GROUP, the SAME
// exclusive `--target` axis (SelectManifestsByGroup) selects them with no
// new tag — a chart is just another manifest group:
//
//	forge deploy <env> --target <name>   # render+apply ONLY this group
//	forge deploy <env>                   # apply EVERYTHING (every group +
//	                                     # every declared platform dep)
//
// APPLY ORDERING (the one real problem). A chart's controllers reference
// CRDs that must exist + be Established first. The chart's bundled
// STANDARD Gateway-API CRDs (group gateway.networking.k8s.io) are excluded
// from the render (`--skip-crds`) because the chart's copy is often older /
// experimental-channel and trips the `safe-upgrades`
// ValidatingAdmissionPolicy, making the install self-deny. forge SUPPLIES
// that group itself at a pinned version (the caller fetches the pinned
// standard Gateway API CRDs / cert-manager's CRDs at the matching chart
// version and hands them in as HelmChartSpec.CRDs).
//
// But a chart ALSO ships its OWN, non-Gateway-API CRDs the controller
// needs — envoy-gateway's eight `gateway.envoyproxy.io` CRDs the controller
// starts informers on; `--skip-crds` would drop those too and the controller
// crashloops on cache-sync. So forge ADDITIONALLY renders the chart
// `--include-crds` (chartOwnCRDs) and supplies the chart's NON-standard-
// Gateway-API CRDs alongside the pinned bundle. Net CRD set = pinned standard
// Gateway-API + the chart's own CRDs.
//
// Apply applies that combined CRD set (+ the chart's synthesized Namespace,
// which `helm template` never emits) FIRST and waits until the CRDs are
// Established before the chart's controller manifests; then waits for the
// chart's Deployments to be Available before the chart's riding manifests
// (the cert-manager webhook would reject ClusterIssuers until then). See
// applyCRDsThenRest, RenderHelmChart, waitChartDeploymentsAvailable.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// HelmChartSpec is one declared platform dependency, resolved from the
// KCL `output.helm_charts` projection plus the caller-fetched CRD bundle.
// The CLI (internal/cli) builds these from KCLEntities and fetches CRDs
// via the existing pinned-CRD machinery (internal/cli/dev_cluster_ingress.go),
// keeping this package free of the templates/cache dependency.
type HelmChartSpec struct {
	// Name is the platform dep's NAME — the `--target` selector and the
	// `app.kubernetes.io/name` stamped on every rendered manifest.
	Name string
	// Chart is the chart name for a repo chart (e.g. "cert-manager").
	// Empty for an OCI chart (OCI carries the full ref).
	Chart string
	// Repo is the chart-repo URL (e.g. "https://charts.jetstack.io").
	// Mutually exclusive with OCI.
	Repo string
	// OCI is the OCI chart ref (e.g.
	// "oci://docker.io/envoyproxy/gateway-helm"). Mutually exclusive with Repo.
	OCI string
	// Version is the pinned chart version (e.g. "v1.20.1").
	Version string
	// Namespace is the namespace to render the chart into
	// (`helm template -n <namespace>`).
	Namespace string
	// Values is the helm values overlay (the `--values` file content),
	// passed through verbatim from the KCL `values` dict.
	Values map[string]any
	// CRDs is the forge-supplied CRD manifest YAML applied FIRST (before
	// the chart's controllers) and waited until Established. The caller
	// fetches it (pinned standard Gateway API CRDs / cert-manager CRDs at
	// the chart version). Empty when the chart needs no forge CRDs.
	CRDs string
	// Manifests is the consumer-declared raw manifest YAML (a `---`-joined
	// stream) that rides this chart's `--target`: the cluster-scoped
	// instances a chart's controller reconciles but the chart doesn't ship
	// (the `eg` GatewayClass, cert-manager ClusterIssuers). Applied AFTER
	// the chart's controllers (so the controller is up before its
	// instances), stamped with the chart's app-label like the chart's own
	// output, so they ride the chart's GROUP under the exclusive `--target`
	// filter (selected iff no targets, or the chart's Name ∈ targets — the
	// same rule as the chart itself). Empty when the chart carries none.
	Manifests string
}

// renderedChart pairs a chart spec with its rendered (stamped,
// --skip-crds) manifest stream so Apply can apply each chart's
// forge-supplied CRDs first and its controllers after.
type renderedChart struct {
	spec      HelmChartSpec
	manifests string
	// crds is the FULL CRD set applied (Established-gated) before this chart's
	// controllers: forge's pinned forge-supplied bundle (spec.CRDs — e.g. the
	// standard Gateway API v1.5.1) PLUS the chart's OWN CRDs that forge does
	// not own (the chart's `gateway.envoyproxy.io` / x-k8s.io CRDs, rendered
	// via `helm template --include-crds` and filtered to exclude the standard
	// Gateway API group forge pins). See RenderHelmChart / chartOwnCRDs.
	crds string
	// extra is the chart's consumer-declared raw manifests (HelmChartSpec.
	// Manifests), stamped with the chart's app-label, applied AFTER the
	// chart's controllers. Empty when the chart carries none.
	extra string
}

// selectHelmChartsByGroup applies the ONE uniform exclusive `--target`
// rule to the platform deps: a chart's NAME is its GROUP, so a chart is
// rendered iff (no targets) OR (its Name ∈ targets) — the IDENTICAL rule
// SelectManifestsByGroup applies to every other manifest. There is NO
// chart opt-in/opt-out asymmetry: a bare `forge deploy <env>` (empty
// targets) reconciles every declared platform dep, exactly as it
// reconciles every app manifest.
//
// So `--target=cert-manager` renders ONLY the cert-manager chart; an empty
// target renders ALL charts; a target naming no chart renders no chart
// (its manifests, if any, are selected by SelectManifestsByGroup instead).
func selectHelmChartsByGroup(charts []HelmChartSpec, targets []string) []HelmChartSpec {
	if len(targets) == 0 {
		return charts
	}
	want := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		want[t] = struct{}{}
	}
	var selected []HelmChartSpec
	for _, c := range charts {
		if _, ok := want[c.Name]; ok {
			selected = append(selected, c)
		}
	}
	return selected
}

// helmTemplate runs `helm template <release> <chart-ref> --version <v>
// --namespace <ns> --skip-crds [--repo <url>] [--values <file>]` and
// returns the rendered `---`-separated manifest stream.
//
// `--skip-crds` is LOAD-BEARING: the chart's bundled CRDs never enter the
// stream (see the package doc). forge supplies the correct CRDs out of
// band (HelmChartSpec.CRDs, applied first by Apply).
//
// Repo vs OCI: a repo chart passes `--repo <url> <chart-name>`; an OCI
// chart passes the OCI ref as the chart argument with no `--repo`. Both
// are fully version-pinned and offline-capable once helm has cached them.
func helmTemplate(ctx context.Context, spec HelmChartSpec) (string, error) {
	args := []string{
		"template", spec.Name,
		"--version", spec.Version,
		"--namespace", spec.Namespace,
		"--skip-crds",
		// include-crds is OFF by default; --skip-crds is explicit + future-proof.
	}

	chartRef := spec.OCI
	if chartRef == "" {
		// Repo chart: `helm template <name> <chart> --repo <url>`.
		chartRef = spec.Chart
		args = append(args, "--repo", spec.Repo)
	}

	var valuesFile string
	if len(spec.Values) > 0 {
		f, err := writeValuesFile(spec.Values)
		if err != nil {
			return "", fmt.Errorf("helm chart %q: write values: %w", spec.Name, err)
		}
		defer os.Remove(f)
		valuesFile = f
		args = append(args, "--values", valuesFile)
	}

	// The chart ref is the positional argument; append last.
	args = append(args, chartRef)

	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("helm template %s (%s@%s): %w\n%s",
			spec.Name, chartRef, spec.Version, err, stderr.String())
	}
	return stdout.String(), nil
}

// writeValuesFile marshals a helm values dict to a temp YAML file and
// returns its path. helm accepts JSON as a --values file (JSON is a YAML
// subset), so we marshal as JSON to avoid a YAML dependency on the
// arbitrary nested values shape.
func writeValuesFile(values map[string]any) (string, error) {
	b, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "forge-helm-values-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// RenderHelmChart expands one HelmChart to its manifest stream, with every
// rendered manifest stamped `app.kubernetes.io/name = spec.Name` so the
// SAME exclusive `--target` axis (SelectManifestsByGroup) selects it. This is the
// whole bridge from "a declared platform dep" to "manifests in the normal
// apply stream": the result joins render output and flows through the same
// filter → apply → wait pipeline as the app.
//
// The chart's CRDs (HelmChartSpec.CRDs) are NOT included here — they are
// applied first by Apply (CRDs → wait Established → rest); RenderHelmChart
// returns only the `--skip-crds` controller/RBAC/Service manifests.
func RenderHelmChart(ctx context.Context, spec HelmChartSpec) (string, error) {
	rendered, err := helmTemplate(ctx, spec)
	if err != nil {
		return "", err
	}
	// helm template does NOT emit the chart's target Namespace, and a chart's
	// rendered resources are namespaced into spec.Namespace (cert-manager →
	// cert-manager, envoy → envoy-gateway-system). On a cold cluster the
	// first config-pass apply then fails `namespaces "<ns>" not found`. So
	// forge EMITS the Namespace itself, stamped with the chart's group so it
	// rides the chart's --target, and applyCRDsThenRest promotes it into the
	// early (CRD) batch so it lands+waits BEFORE the namespaced resources.
	withNS := joinNonEmpty(namespaceManifest(spec.Namespace), rendered)
	// Drop helm POST-phase / test hooks (cert-manager-startupapicheck): a
	// `helm template` render carries `helm.sh/hook`-annotated docs that are
	// meaningless under a declarative reconcile and, being immutable Jobs,
	// force a delete/recreate on every warm redeploy. PRE-phase hooks
	// (envoy-gateway's certgen, which creates the controller's serving-cert
	// Secret) are KEPT — they produce prerequisite cluster state the
	// workloads mount, not a post-facto check. See dropPostHelmHooks.
	withNS = dropPostHelmHooks(withNS)
	return stampAppLabel(withNS, spec.Name), nil
}

// namespaceManifest renders a bare `kind: Namespace` document for ns, or ""
// when ns is empty (a chart with no namespace renders into whatever the
// apply default is — nothing to create). The Namespace carries no labels
// here; stampAppLabel adds the chart group + managed-by afterwards so it
// rides the chart's --target and applyCRDsThenRest promotes it ahead of the
// namespaced resources. Idempotent at apply time (already-exists is fine
// under server-side apply).
func namespaceManifest(ns string) string {
	if strings.TrimSpace(ns) == "" {
		return ""
	}
	return "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns
}

// dropPostHelmHooks removes docs whose `helm.sh/hook` annotation is
// EXCLUSIVELY a post-phase or test hook (post-install / post-upgrade /
// post-rollback / post-delete / test) from a `---`-separated stream. Those
// hooks (cert-manager's cert-manager-startupapicheck Job + its RBAC) are
// post-facto CHECKS: under `helm template` they're inert, and left in the
// render the startupapicheck Job's immutable spec.template forces the
// immutable-Job delete/recreate recovery on EVERY warm redeploy. Dropping
// them keeps the declarative reconcile a true no-op when nothing changed.
//
// PRE-phase hooks are KEPT. The distinction is correctness, not cosmetics: a
// pre-install/pre-upgrade hook PRODUCES prerequisite cluster state the
// workloads depend on — envoy-gateway's `certgen` Job (pre-install,
// pre-upgrade) generates the `envoy-gateway` serving-cert Secret the
// controller pod mounts; drop it and the controller wedges forever on
// `MountVolume.SetUp failed ... secret "envoy-gateway" not found`. Helm's own
// `upgrade --install` runs that certgen on every install/upgrade, so keeping
// it (it reconciles as a plain Job, healed by the immutable-Job recovery on a
// warm redeploy) matches the imperative install's behaviour exactly.
//
// A doc with no hook annotation, or a mixed pre+post hook, is KEPT (only a
// purely post/test hook is dropped). Docs that don't parse pass through.
func dropPostHelmHooks(manifests string) string {
	var kept []string
	for _, doc := range splitDocs(manifests) {
		if isPostOnlyHelmHook(doc) {
			continue
		}
		kept = append(kept, doc)
	}
	return strings.Join(kept, docDelimiter)
}

// postPhaseHelmHooks is the set of `helm.sh/hook` values that mark a doc as a
// post-facto / test hook (vs a pre-phase hook that sets up prerequisites).
var postPhaseHelmHooks = map[string]struct{}{
	"post-install":  {},
	"post-upgrade":  {},
	"post-rollback": {},
	"post-delete":   {},
	"test":          {},
	"test-success":  {},
}

// isPostOnlyHelmHook reports whether a doc carries a `helm.sh/hook` annotation
// whose phases are ALL post-phase/test (so it's safe to drop under a
// declarative reconcile). A doc with no hook annotation returns false (kept);
// a doc with ANY pre-phase hook returns false (kept — it may set up
// prerequisite state). The annotation is a comma-separated list
// (`post-install,post-upgrade`).
func isPostOnlyHelmHook(doc string) bool {
	var m struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		return false
	}
	raw, ok := m.Metadata.Annotations["helm.sh/hook"]
	if !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	for _, h := range strings.Split(raw, ",") {
		if _, post := postPhaseHelmHooks[strings.TrimSpace(h)]; !post {
			// A non-post (pre-phase) hook — keep the whole doc.
			return false
		}
	}
	return true
}

// standardGatewayAPIGroup is the CRD group forge OWNS at a pinned version
// (the standard-channel Gateway API CRDs, v1.5.1). A chart that bundles its
// OWN copy of this group (envoy gateway-helm ships an older,
// experimental-channel copy) must NOT have it supplied from the chart render
// — forge pins it separately so the chart's copy never trips the
// safe-upgrades ValidatingAdmissionPolicy. chartOwnCRDs filters this group
// out of the `--include-crds` render.
const standardGatewayAPIGroup = "gateway.networking.k8s.io"

// chartOwnCRDs renders a chart with `--include-crds` and returns the chart's
// OWN CRDs that forge does NOT already own — i.e. every CRD in the chart's
// crds/ EXCEPT the standard Gateway API group (standardGatewayAPIGroup),
// which forge supplies separately at its pinned version.
//
// THE BUG THIS FIXES (K3): the envoy gateway-helm chart's controller starts
// informers on its EIGHT `gateway.envoyproxy.io` CRDs (envoyproxies,
// clienttrafficpolicies, backendtrafficpolicies, securitypolicies,
// envoypatchpolicies, envoyextensionpolicies, httproutefilters, backends).
// With the main render `--skip-crds` those CRDs never land → the controller's
// cache-sync fails → it never goes Ready → ingress is dead. forge must supply
// the chart's OWN CRDs IN ADDITION to the standard Gateway API CRDs it pins.
//
// Why filter the standard Gateway API group OUT: the chart bundles an older,
// experimental-channel copy of `gateway.networking.k8s.io` that the
// safe-upgrades ValidatingAdmissionPolicy (activated by forge's pinned
// standard v1.5.1) DENIES. forge keeps owning that group at v1.5.1; only the
// chart's NON-standard-Gateway-API CRDs (its `gateway.envoyproxy.io` +
// x-k8s.io ones) are taken from the chart render. A chart that bundles no
// CRDs (cert-manager, rendered --skip-crds and supplied separately) yields ""
// here.
func chartOwnCRDs(ctx context.Context, spec HelmChartSpec) (string, error) {
	rendered, err := helmTemplateIncludeCRDs(ctx, spec)
	if err != nil {
		return "", err
	}
	var kept []string
	for _, doc := range splitDocs(rendered) {
		m, ok := parseDoc(doc)
		if !ok || m.Kind != "CustomResourceDefinition" {
			continue
		}
		if crdGroup(doc) == standardGatewayAPIGroup {
			// forge owns this group at its pinned version — never from the chart.
			continue
		}
		kept = append(kept, doc)
	}
	return strings.Join(kept, docDelimiter), nil
}

// crdGroup extracts spec.group from a CustomResourceDefinition document
// (e.g. `gateway.envoyproxy.io`, `gateway.networking.k8s.io`). Empty when
// the doc has no readable group.
func crdGroup(doc string) string {
	var m struct {
		Spec struct {
			Group string `yaml:"group"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		return ""
	}
	return m.Spec.Group
}

// helmTemplateIncludeCRDs runs the SAME `helm template` as helmTemplate but
// with `--include-crds` instead of `--skip-crds`, so the chart's bundled CRDs
// (its crds/ directory) appear in the output. chartOwnCRDs filters the result
// down to the chart's own non-standard-Gateway-API CRDs. Kept separate from
// helmTemplate (which stays --skip-crds for the controller render) so the two
// passes never share the CRD-channel decision.
func helmTemplateIncludeCRDs(ctx context.Context, spec HelmChartSpec) (string, error) {
	args := []string{
		"template", spec.Name,
		"--version", spec.Version,
		"--namespace", spec.Namespace,
		"--include-crds",
	}
	chartRef := spec.OCI
	if chartRef == "" {
		chartRef = spec.Chart
		args = append(args, "--repo", spec.Repo)
	}
	if len(spec.Values) > 0 {
		f, err := writeValuesFile(spec.Values)
		if err != nil {
			return "", fmt.Errorf("helm chart %q: write values: %w", spec.Name, err)
		}
		defer os.Remove(f)
		args = append(args, "--values", f)
	}
	args = append(args, chartRef)

	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("helm template --include-crds %s (%s@%s): %w\n%s",
			spec.Name, chartRef, spec.Version, err, stderr.String())
	}
	return stdout.String(), nil
}

// stampAppLabel FORCES `app.kubernetes.io/name = <name>` onto every
// document in a `---`-separated manifest stream so the deploy layer's
// `--target` selection (SelectManifestsByGroup) treats the WHOLE chart as
// ONE named app. Documents that don't parse are passed through unchanged.
//
// OVERRIDE, not defer-to-existing: unlike lib/services.k's
// `_stamp_owner_label` (which lets a user's explicit app label win on a
// raw Service.manifests entry), a helm chart sets its OWN per-component
// `app.kubernetes.io/name` (cert-manager's webhook / cainjector subcharts
// label themselves "webhook" / "cainjector"). If those survived,
// `--target=cert-manager` would DROP them (SelectManifestsByGroup keeps only
// docs whose app label equals the target), shipping a half-installed
// chart. The chart `name` is the single `--target` selector for the whole
// dependency, so it MUST overwrite any chart-set value.
func stampAppLabel(manifests, name string) string {
	var out []string
	for _, doc := range splitDocs(manifests) {
		out = append(out, stampDocAppLabel(doc, name))
	}
	return strings.Join(out, docDelimiter)
}

// stampDocAppLabel OVERWRITES metadata.labels["app.kubernetes.io/name"]
// with name on a single YAML document, preserving every other field and
// label. The override (see stampAppLabel) is what makes the chart's whole
// manifest set select as ONE `--target`. Unparseable docs pass through
// verbatim.
func stampDocAppLabel(doc, name string) string {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil || m == nil {
		return doc
	}
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	// FORCE the chart name — the whole chart is one --target unit.
	labels[appNameLabel] = name
	if _, ok := labels["app.kubernetes.io/managed-by"]; !ok {
		labels["app.kubernetes.io/managed-by"] = "forge"
	}
	meta["labels"] = labels
	m["metadata"] = meta
	b, err := yaml.Marshal(m)
	if err != nil {
		return doc
	}
	return strings.TrimRight(string(b), "\n")
}

// applyCRDsThenRest applies a manifest stream in CRD-FIRST order: every
// CustomResourceDefinition document is applied and WAITED to Established
// before the rest of the stream. This is the apply-ordering primitive the
// helm-as-renderer model needs — a `--target=<platform>` apply must leave
// the cluster with CRDs Established + controllers Deployed, so a later
// `forge deploy` (the app) finds the CRDs present.
//
// extraCRDs are forge-supplied CRDs (the pinned standard Gateway API CRDs /
// cert-manager CRDs at the chart version) that must also land + Establish
// before the rest — the chart was rendered `--skip-crds`, so its own CRDs
// are absent and forge owns them. They are applied in the SAME first pass
// as any CRD found in the stream.
//
// Ordering, in one apply:
//  1. CRDs (extraCRDs ++ every `kind: CustomResourceDefinition` in
//     manifests) → kubectl apply → wait --for=condition=Established.
//  2. Everything else → kubectl apply.
//
// When there are no CRDs in either source this degenerates to a single
// apply of `rest`, byte-identical to a plain apply.
func applyCRDsThenRest(ctx context.Context, kctx, extraCRDs, manifests string) error {
	streamCRDs, streamNS, rest := partitionEarlyBatch(manifests)

	// Early batch: CRDs + Namespaces. The chart's namespaced resources target
	// a Namespace `helm template` never emits (RenderHelmChart synthesizes
	// it), so the Namespace must land BEFORE the rest pass or the config-first
	// apply fails `namespaces "<ns>" not found`. CRDs + Namespaces are both
	// pre-requisites of the rest, applied together; only the CRDs gate on
	// Established (a Namespace is ready the moment it exists).
	crds := joinNonEmpty(extraCRDs, streamCRDs)
	early := joinNonEmpty(crds, streamNS)
	if strings.TrimSpace(early) != "" {
		if err := KubectlApply(ctx, kctx, early); err != nil {
			return fmt.Errorf("apply CRDs/Namespaces: %w", err)
		}
		names := crdNames(crds)
		if len(names) > 0 {
			fmt.Printf("Waiting for %d CRD(s) to be Established...\n", len(names))
			if err := waitCRDsEstablished(ctx, kctx, names, 120*time.Second); err != nil {
				return fmt.Errorf("wait CRDs Established: %w", err)
			}
		}
	}

	if strings.TrimSpace(rest) != "" {
		// Reuse the standard two-pass (config-then-rest) apply for the
		// non-CRD remainder so a chart's namespaced Secrets/ConfigMaps land
		// before the controller pods that reference them.
		config, workloads := PartitionConfigManifests(rest)
		if strings.TrimSpace(config) != "" {
			if err := KubectlApply(ctx, kctx, config); err != nil {
				return fmt.Errorf("apply config: %w", err)
			}
		}
		if err := KubectlApply(ctx, kctx, workloads); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	}
	return nil
}

// partitionEarlyBatch splits a `---`-separated stream into (crds, namespaces,
// rest): crds holds every `kind: CustomResourceDefinition` document (in
// order), namespaces holds every `kind: Namespace` document, rest holds
// everything else. CRDs and Namespaces are the early-batch prerequisites
// applyCRDsThenRest lands before the rest (CRDs additionally gate on
// Established). An unparseable doc goes to rest (it can't be confirmed a CRD
// or Namespace, and rest is the pass that always runs).
func partitionEarlyBatch(manifests string) (crds, namespaces, rest string) {
	var crdDocs, nsDocs, restDocs []string
	for _, doc := range splitDocs(manifests) {
		m, ok := parseDoc(doc)
		switch {
		case ok && m.Kind == "CustomResourceDefinition":
			crdDocs = append(crdDocs, doc)
		case ok && m.Kind == "Namespace":
			nsDocs = append(nsDocs, doc)
		default:
			restDocs = append(restDocs, doc)
		}
	}
	return strings.Join(crdDocs, docDelimiter),
		strings.Join(nsDocs, docDelimiter),
		strings.Join(restDocs, docDelimiter)
}

// crdNames extracts the metadata.name of every CustomResourceDefinition in
// a `---`-separated stream — the names `kubectl wait --for=Established
// crd/<name>` blocks on.
func crdNames(manifests string) []string {
	var names []string
	for _, doc := range splitDocs(manifests) {
		m, ok := parseDoc(doc)
		if ok && m.Kind == "CustomResourceDefinition" && m.Metadata.Name != "" {
			names = append(names, m.Metadata.Name)
		}
	}
	return names
}

// waitCRDsEstablished blocks until every named CRD reports
// Established=True (or the timeout elapses) — the happens-before that lets
// the chart's controllers (and any later `forge deploy` referencing the
// CRD's resources) apply against a CRD the apiserver already serves.
func waitCRDsEstablished(ctx context.Context, kctx string, names []string, timeout time.Duration) error {
	args := []string{"wait", "--for=condition=Established", "--timeout=" + timeout.String()}
	for _, n := range names {
		args = append(args, "crd/"+n)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitChartDeploymentsAvailable blocks until every Deployment in the chart's
// namespace reports Available=True (or the timeout elapses). This is the
// happens-before the chart's RIDING manifests need: a chart that ships an
// admission webhook (cert-manager's `webhook.cert-manager.io`
// ValidatingWebhookConfiguration, failurePolicy: Fail) REJECTS the
// cluster-scoped instances its controller reconciles (ClusterIssuers) with
// `failed calling webhook ... no endpoints available` until the webhook
// Deployment is Ready and cainjector has injected the caBundle. Waiting for
// the chart's Deployments to be Available closes that race before the riding
// manifests apply. A namespace with no Deployments (kubectl wait --all over
// an empty set) returns immediately.
func waitChartDeploymentsAvailable(ctx context.Context, kctx, namespace string, timeout time.Duration) error {
	cmd := kubectlCmd(ctx, kctx,
		"wait", "--for=condition=Available",
		"deploy", "--all",
		"-n", namespace,
		"--timeout="+timeout.String(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// applyRidingManifestsWithRetry applies a chart's riding manifests (the
// GatewayClass / ClusterIssuers stamped with the chart's group) with a
// bounded retry. Even after the chart's webhook Deployment reports Available,
// the webhook Service's endpoints can lag a few seconds (the readiness gate
// and Endpoints publication aren't perfectly simultaneous), so an apply that
// hits `no endpoints available` / `connection refused` for the webhook is
// retried rather than failing the deploy. A non-webhook error surfaces
// immediately (no point retrying a genuine manifest error).
func applyRidingManifestsWithRetry(ctx context.Context, kctx, manifests string) error {
	const attempts = 6
	const delay = 5 * time.Second
	var err error
	for i := 0; i < attempts; i++ {
		if err = applyCRDsThenRest(ctx, kctx, "", manifests); err == nil {
			return nil
		}
		if !isWebhookNotReadyError(err) {
			return err
		}
		if i < attempts-1 {
			fmt.Printf("Webhook endpoint not ready yet (attempt %d/%d); retrying in %s...\n",
				i+1, attempts, delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return err
}

// isWebhookNotReadyError reports whether an apply error is the transient
// "the admission webhook isn't serving yet" failure — the only error
// applyRidingManifestsWithRetry retries. The cert-manager validating webhook
// surfaces as `failed calling webhook ... no endpoints available for service`
// (or `connection refused` / `context deadline exceeded`) while its
// Deployment's endpoints are still publishing.
func isWebhookNotReadyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed calling webhook") &&
		(strings.Contains(msg, "no endpoints available") ||
			strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "context deadline exceeded") ||
			strings.Contains(msg, "EOF") ||
			strings.Contains(msg, "i/o timeout"))
}

// joinNonEmpty joins manifest streams with the doc delimiter, skipping
// empty ones so no spurious `---\n---` separators appear.
func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	return strings.Join(kept, docDelimiter)
}
