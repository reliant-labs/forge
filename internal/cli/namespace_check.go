package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// namespaceCheckResult is the structured outcome of walking a rendered
// manifest stream against an effective namespace. Carries the per-source
// list of mismatches so the error formatter can describe each
// independently — useful when KCL hardcodes one namespace in
// `metadata.namespace` AND a different one in an env-var DNS value.
type namespaceCheckResult struct {
	// EffectiveNS is the namespace forge will actually apply to (the
	// one passed to `kubectl apply -n <ns>` and bound into KCL's
	// `namespace` -D variable). All mismatches are reported relative
	// to this anchor.
	EffectiveNS string

	// EffectiveNSSource describes how EffectiveNS was determined, for
	// the actionable fix hint ("explicit forge.yaml declaration" vs.
	// "default <project>-<env>"). The "default" path is the one that
	// silently bites users — they had no idea forge was computing it.
	EffectiveNSSource string

	// MetadataMismatches lists every distinct `metadata.namespace`
	// value seen in the rendered stream that differs from EffectiveNS.
	// One entry per (namespace, [{kind, name}...]) so the error can
	// say "Service/foo + Deployment/bar are stamped with ns=foo".
	MetadataMismatches []metadataNamespaceMismatch

	// EnvVarMismatches lists every `<word>.svc.cluster.local`
	// occurrence in container env-var values where <word> differs
	// from EffectiveNS. One entry per (namespace, [{owner, env_name,
	// env_value}...]).
	EnvVarMismatches []envVarNamespaceMismatch
}

// metadataNamespaceMismatch is one (wrong-namespace, resources) pair
// from the rendered stream's `metadata.namespace` walk.
type metadataNamespaceMismatch struct {
	Namespace string // the wrong value found
	Resources []string // formatted as "Kind/name"
}

// envVarNamespaceMismatch is one (wrong-namespace, occurrences) pair
// from the env-var DNS walk. Owner is "Kind/name" of the workload
// carrying the env var; EnvName is the variable name (NATS_URL etc.);
// EnvValue is the literal value containing the wrong namespace.
type envVarNamespaceMismatch struct {
	Namespace   string
	Occurrences []envVarOccurrence
}

type envVarOccurrence struct {
	Owner    string // e.g. "Deployment/api-server"
	EnvName  string // e.g. "NATS_URL"
	EnvValue string // the full literal value
}

// hasFindings reports whether the result represents an actual mismatch
// (vs. a clean render). nil-tolerant for caller convenience.
func (r *namespaceCheckResult) hasFindings() bool {
	if r == nil {
		return false
	}
	return len(r.MetadataMismatches) > 0 || len(r.EnvVarMismatches) > 0
}

// resolveEffectiveNamespace returns (namespace, source-description)
// using the forge default-resolution rule: explicit namespace wins,
// otherwise fall back to `<project>-<env>`. The source-description is
// the human-readable explanation rendered in the error fix hint —
// "explicit in forge.yaml" vs. "default <project>-<env>".
//
// Lifted out so the check can be invoked from places (a future
// `forge lint --namespace`, a doctor check) that don't go through
// runDeploy. runDeploy itself uses namespaceResolutionSource because
// it has access to the --namespace flag plumbing.
func resolveEffectiveNamespace(projectName, envName, explicitNS string) (string, string) {
	if explicitNS != "" {
		return explicitNS, "explicit `environments[" + envName + "].namespace` in forge.yaml"
	}
	return projectName + "-" + envName, "default `<project>-<env>` (no `environments[" + envName + "].namespace` in forge.yaml)"
}

// dnsNamespacePattern matches the `<namespace>.svc.cluster.local` and
// `<namespace>.svc` suffixes that the in-cluster DNS service produces.
// We anchor on `.svc.cluster.local` because that's the unambiguous
// cluster-DNS fragment — a service like `nats.cp-forge-dev.svc.cluster.local`
// guarantees the second-to-last dotted segment is the namespace.
//
// The full match captures everything BEFORE `.svc.cluster.local` so the
// regex stays robust against DNS values that have a port suffix
// (`:4222`) or a path. The namespace is the dotted segment immediately
// preceding `.svc.cluster.local`.
var dnsNamespacePattern = regexp.MustCompile(`([a-z0-9][a-z0-9-]{0,61}[a-z0-9]|[a-z0-9])\.svc\.cluster\.local`)

// checkNamespaceConsistency walks a `---`-separated YAML document
// stream (the shape forge's KCL render produces) and returns a
// non-nil error when any `metadata.namespace` or env-var DNS
// reference disagrees with effectiveNS.
//
// The pure-function shape (string in, error out) keeps the check
// unit-testable without shelling kcl. Callers that need the structured
// findings (a future `forge lint --json` consumer) can use
// computeNamespaceCheck directly.
func checkNamespaceConsistency(manifests, projectName, envName, effectiveNS, effectiveNSSource string) error {
	result := computeNamespaceCheck(manifests, effectiveNS, effectiveNSSource)
	if !result.hasFindings() {
		return nil
	}
	return formatNamespaceMismatchError(result, projectName, envName)
}

// computeNamespaceCheck does the structural walk and returns the raw
// findings. Pure — no I/O, no fmt.Println side-effects. Tests assert
// against the structured result, the format function asserts against
// the rendered error text.
func computeNamespaceCheck(manifests, effectiveNS, effectiveNSSource string) *namespaceCheckResult {
	result := &namespaceCheckResult{
		EffectiveNS:       effectiveNS,
		EffectiveNSSource: effectiveNSSource,
	}

	// Group findings by the wrong namespace so the error reports
	// "Service/foo + Deployment/bar are all stamped with ns=foo" rather
	// than one finding per resource.
	metaByNS := map[string][]string{}
	envByNS := map[string][]envVarOccurrence{}

	for _, doc := range splitYAMLDocs(manifests) {
		var node map[string]any
		if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
			continue // best-effort: malformed manifest is the renderer's problem to surface
		}
		if node == nil {
			continue
		}

		kind, _ := node["kind"].(string)
		name := extractMetadataName(node)
		owner := fmt.Sprintf("%s/%s", emptyAs(kind, "?"), emptyAs(name, "?"))

		// metadata.namespace walk.
		if ns := extractMetadataNamespace(node); ns != "" && ns != effectiveNS {
			metaByNS[ns] = append(metaByNS[ns], owner)
		}

		// container env-var DNS walk. Both top-level Pod-shaped specs
		// (Pod, Job, CronJob) and Deployment/StatefulSet-style specs
		// (which nest a PodTemplateSpec) are covered by extractContainers.
		for _, c := range extractContainers(node) {
			for _, ev := range extractEnvVars(c) {
				val, _ := ev["value"].(string)
				if val == "" {
					continue
				}
				for _, matchedNS := range extractNamespacesFromValue(val) {
					if matchedNS == effectiveNS {
						continue
					}
					evName, _ := ev["name"].(string)
					envByNS[matchedNS] = append(envByNS[matchedNS], envVarOccurrence{
						Owner:    owner,
						EnvName:  evName,
						EnvValue: val,
					})
				}
			}
		}
	}

	for ns, owners := range metaByNS {
		// De-dupe owners — a Deployment can carry both itself and its
		// template metadata.namespace, but we only want each named
		// resource listed once.
		owners = dedupeSorted(owners)
		result.MetadataMismatches = append(result.MetadataMismatches, metadataNamespaceMismatch{
			Namespace: ns,
			Resources: owners,
		})
	}
	sort.Slice(result.MetadataMismatches, func(i, j int) bool {
		return result.MetadataMismatches[i].Namespace < result.MetadataMismatches[j].Namespace
	})

	for ns, occs := range envByNS {
		sort.Slice(occs, func(i, j int) bool {
			if occs[i].Owner != occs[j].Owner {
				return occs[i].Owner < occs[j].Owner
			}
			return occs[i].EnvName < occs[j].EnvName
		})
		result.EnvVarMismatches = append(result.EnvVarMismatches, envVarNamespaceMismatch{
			Namespace:   ns,
			Occurrences: occs,
		})
	}
	sort.Slice(result.EnvVarMismatches, func(i, j int) bool {
		return result.EnvVarMismatches[i].Namespace < result.EnvVarMismatches[j].Namespace
	})

	return result
}

// extractMetadataName pulls metadata.name from a parsed manifest node.
// Empty string when missing — the namespace walk still reports the
// resource as Kind/? so users get partial-information error output
// rather than nothing.
func extractMetadataName(node map[string]any) string {
	md, _ := node["metadata"].(map[string]any)
	if md == nil {
		return ""
	}
	name, _ := md["name"].(string)
	return name
}

// extractMetadataNamespace pulls metadata.namespace from a parsed
// manifest node, or returns "" when absent. Note: a missing namespace
// is fine — kubectl will use the apply-time `-n <ns>` flag. Only an
// explicit, non-matching value is a finding.
func extractMetadataNamespace(node map[string]any) string {
	md, _ := node["metadata"].(map[string]any)
	if md == nil {
		return ""
	}
	ns, _ := md["namespace"].(string)
	return ns
}

// extractContainers returns every container spec found in a manifest
// node, looking in both the top-level `spec.containers` (Pod-shaped:
// Pod, Job) and the nested PodTemplateSpec (Deployment, StatefulSet,
// DaemonSet, CronJob, ReplicaSet). Init containers and the CronJob's
// jobTemplate path are also covered.
//
// Returns the raw map nodes so callers can iterate their `env` lists
// without re-marshaling.
func extractContainers(node map[string]any) []map[string]any {
	var out []map[string]any
	// Direct Pod / Job spec.
	if spec, ok := node["spec"].(map[string]any); ok {
		out = append(out, containersFromPodSpec(spec)...)
		// PodTemplateSpec: spec.template.spec.containers (Deployment, etc.).
		if tmpl, ok := spec["template"].(map[string]any); ok {
			if tmplSpec, ok := tmpl["spec"].(map[string]any); ok {
				out = append(out, containersFromPodSpec(tmplSpec)...)
			}
		}
		// CronJob: spec.jobTemplate.spec.template.spec.containers.
		if jt, ok := spec["jobTemplate"].(map[string]any); ok {
			if jtSpec, ok := jt["spec"].(map[string]any); ok {
				if tmpl, ok := jtSpec["template"].(map[string]any); ok {
					if tmplSpec, ok := tmpl["spec"].(map[string]any); ok {
						out = append(out, containersFromPodSpec(tmplSpec)...)
					}
				}
			}
		}
	}
	return out
}

// containersFromPodSpec returns the concatenation of `containers` and
// `initContainers` from a PodSpec map. Either or both may be absent.
func containersFromPodSpec(spec map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"containers", "initContainers"} {
		raw, ok := spec[key].([]any)
		if !ok {
			continue
		}
		for _, c := range raw {
			if cm, ok := c.(map[string]any); ok {
				out = append(out, cm)
			}
		}
	}
	return out
}

// extractEnvVars returns the container's `env` list as a slice of
// map[string]any. Each entry typically has `name` and either `value`
// or `valueFrom`. Only `value` is interesting for the DNS check —
// secretKeyRef-driven values are opaque and not checkable.
func extractEnvVars(container map[string]any) []map[string]any {
	raw, ok := container["env"].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, ev := range raw {
		if m, ok := ev.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// extractNamespacesFromValue finds every `<word>.svc.cluster.local`
// occurrence in s and returns the captured namespace words. Multiple
// occurrences in one value (rare, but TEMPORAL_HOST+NATS_URL style
// composition) yield multiple results.
func extractNamespacesFromValue(s string) []string {
	matches := dnsNamespacePattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	seen := map[string]struct{}{}
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		ns := m[1]
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}

// splitYAMLDocs splits the multi-document YAML stream forge emits into
// per-document strings. The KCL renderer uses `\n---\n` as the
// canonical separator (see internal/cluster.extractManifests). We
// accept leading/trailing whitespace around docs and drop empties.
func splitYAMLDocs(s string) []string {
	docs := strings.Split(s, "\n---\n")
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		trimmed := strings.TrimSpace(d)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// dedupeSorted returns the input slice with duplicates removed and the
// result sorted. Used to keep the error report deterministic when the
// same resource is reached via two paths (e.g. Deployment top-level vs.
// its PodTemplateSpec).
func dedupeSorted(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// formatNamespaceMismatchError builds the user-facing error text. Kept
// out of the walking logic so the structured result remains
// machine-readable for future JSON consumers and so the formatting can
// be tweaked without disturbing the walk.
//
// The message lists every mismatched namespace once, the resources or
// env-vars that referenced it, and a concrete `forge.yaml` patch the
// user can paste in to fix the mismatch.
func formatNamespaceMismatchError(r *namespaceCheckResult, projectName, envName string) error {
	var b strings.Builder

	totalMismatched := len(r.MetadataMismatches) + len(r.EnvVarMismatches)
	fmt.Fprintf(&b, "namespace mismatch between rendered KCL and forge.yaml for env %q (%d distinct mismatch source(s) found):\n\n",
		envName, totalMismatched)

	fmt.Fprintf(&b, "  forge will apply to namespace: %s\n", r.EffectiveNS)
	fmt.Fprintf(&b, "    (source: %s)\n\n", r.EffectiveNSSource)

	if len(r.MetadataMismatches) > 0 {
		fmt.Fprintln(&b, "  Rendered KCL stamps a different namespace via metadata.namespace:")
		for _, m := range r.MetadataMismatches {
			fmt.Fprintf(&b, "    • ns=%q on:\n", m.Namespace)
			for _, r := range m.Resources {
				fmt.Fprintf(&b, "        - %s\n", r)
			}
		}
		fmt.Fprintln(&b)
	}

	if len(r.EnvVarMismatches) > 0 {
		fmt.Fprintln(&b, "  Rendered KCL references a different namespace via in-cluster DNS env-vars (`*.<ns>.svc.cluster.local`):")
		for _, e := range r.EnvVarMismatches {
			fmt.Fprintf(&b, "    • ns=%q referenced in:\n", e.Namespace)
			for _, occ := range e.Occurrences {
				fmt.Fprintf(&b, "        - %s env %s=%s\n", occ.Owner, occ.EnvName, occ.EnvValue)
			}
		}
		fmt.Fprintln(&b)
	}

	// Pick a concrete suggested namespace for the fix hint. Prefer the
	// most-referenced wrong namespace — that's almost always the one the
	// user actually wants (they hardcoded it in KCL because that's the
	// namespace they intended). Fall back to the first mismatched
	// namespace alphabetically when there's no clear winner.
	suggested := pickSuggestedNamespace(r)

	fmt.Fprintln(&b, "  Fix (one of):")
	fmt.Fprintf(&b, "    1. Add to forge.yaml so the apply targets the namespace KCL expects:\n")
	fmt.Fprintln(&b, "         environments:")
	fmt.Fprintf(&b, "           - name: %s\n", envName)
	fmt.Fprintf(&b, "             namespace: %s\n", suggested)
	fmt.Fprintf(&b, "    2. Edit deploy/kcl/%s/main.k so the hardcoded namespace(s) match the effective one (%s)\n", envName, r.EffectiveNS)
	fmt.Fprintf(&b, "       — typically by using KCL's `namespace` parameter instead of a string literal.\n")
	fmt.Fprintf(&b, "    3. Override at the CLI: forge deploy %s --namespace %s\n", envName, suggested)

	_ = projectName // reserved for future use in the fix hint
	return fmt.Errorf("%s", b.String())
}

// pickSuggestedNamespace returns the most-likely-correct namespace
// based on which wrong-namespace value appears most often in the
// findings. Falls back to the lexicographically first when frequencies
// tie or when only one namespace is in play.
func pickSuggestedNamespace(r *namespaceCheckResult) string {
	counts := map[string]int{}
	for _, m := range r.MetadataMismatches {
		counts[m.Namespace] += len(m.Resources)
	}
	for _, e := range r.EnvVarMismatches {
		counts[e.Namespace] += len(e.Occurrences)
	}
	if len(counts) == 0 {
		return r.EffectiveNS
	}
	bestNS := ""
	bestN := -1
	// Iterate sorted for deterministic tie-break (lexicographically
	// first wins on a tie).
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if counts[k] > bestN {
			bestN = counts[k]
			bestNS = k
		}
	}
	return bestNS
}
