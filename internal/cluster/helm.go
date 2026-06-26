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
// CRDs that must exist + be Established first. The chart's OWN CRDs are
// excluded from the render (`--skip-crds`) because a chart's bundled
// (often older / experimental-channel) Gateway-API CRDs trip the
// `safe-upgrades` ValidatingAdmissionPolicy and make the install
// self-deny. forge instead SUPPLIES the correct CRDs itself (the caller
// fetches the pinned standard Gateway API CRDs / cert-manager's CRDs at
// the matching chart version and hands them in as HelmChartSpec.CRDs);
// Apply applies those CRDs FIRST and waits until Established before the
// chart's controller manifests. See applyCRDsThenRest.
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
	return stampAppLabel(rendered, spec.Name), nil
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
	streamCRDs, rest := partitionCRDs(manifests)

	crds := joinNonEmpty(extraCRDs, streamCRDs)
	if strings.TrimSpace(crds) != "" {
		if err := KubectlApply(ctx, kctx, crds); err != nil {
			return fmt.Errorf("apply CRDs: %w", err)
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

// partitionCRDs splits a `---`-separated stream into (crds, rest): crds
// holds every `kind: CustomResourceDefinition` document (in order), rest
// holds everything else. An unparseable doc goes to rest (it can't be
// confirmed a CRD, and rest is the pass that always runs).
func partitionCRDs(manifests string) (crds, rest string) {
	var crdDocs, restDocs []string
	for _, doc := range splitDocs(manifests) {
		m, ok := parseDoc(doc)
		if ok && m.Kind == "CustomResourceDefinition" {
			crdDocs = append(crdDocs, doc)
		} else {
			restDocs = append(restDocs, doc)
		}
	}
	return strings.Join(crdDocs, docDelimiter), strings.Join(restDocs, docDelimiter)
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
