package mlcollect

import (
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Read is one row of discovery's read set: a resource the collector reads,
// with the one verb it uses and why. ReadSet is the whole of it, and the
// collector reads nothing it does not list (TestCollectorReadsOnlyTheReadSet
// fails a read outside it). It is a security promise, printed by
// `forge ml discover --help` and the ml/discover skill from this table.
type Read struct {
	// Group is the API group ("" for core). Discovery endpoints have none.
	Group string `json:"group,omitempty"`
	// Resource is the plural resource name, or the discovery path
	// (/version, /api, /apis) for a non-resource read.
	Resource string `json:"resource"`
	// Verb is "get" or "list". Never anything else.
	Verb string `json:"verb"`
	// Scope says how much of each object is kept: "full" (the object, minus
	// managedFields, annotations and container env values), "redacted" (as
	// full, and every value under a sensitive key replaced: see Redact),
	// "metadata" (names and labels only; the collector uses the metadata API,
	// so the server never sends the rest), "count", or "names" (metric names
	// and label keys, never a sample value).
	Scope string `json:"scope"`
	// When is set for a read that only happens under a condition (the CRD
	// exists, --probe).
	When string `json:"when,omitempty"`
	// Probe marks the read only `--probe` makes. It is the one read outside
	// ClusterRole(); ProbeRules() grants it.
	Probe bool `json:"probe,omitempty"`
	// Why is what discovery uses it for.
	Why string `json:"why"`
}

// Name is the resource as kubectl names it (pods, daemonsets.apps,
// clusterpolicies.nvidia.com), or the discovery path.
func (r Read) Name() string {
	if r.Group == "" {
		return r.Resource
	}
	return r.Resource + "." + r.Group
}

// The verbs the collector uses. A test pins that no other verb is ever sent.
const (
	VerbGet  = "get"
	VerbList = "list"
)

// countedCRDs are the ML CRDs (Appendix B of the ML proposal) whose objects
// discovery counts, so a CRD with no objects is told apart from one in use.
// Each is read metadata-only, and only when the CRD is installed.
var countedCRDs = []string{
	"clusterpolicies.nvidia.com",
	"nvidiadrivers.nvidia.com",
	"deviceconfigs.amd.com",
	"servicemonitors.monitoring.coreos.com",
	"podmonitors.monitoring.coreos.com",
	"podmonitorings.monitoring.googleapis.com",
	"clusterpodmonitorings.monitoring.googleapis.com",
	"opentelemetrycollectors.opentelemetry.io",
	"instrumentations.opentelemetry.io",
	"clusterqueues.kueue.x-k8s.io",
	"localqueues.kueue.x-k8s.io",
	"resourceflavors.kueue.x-k8s.io",
	"queues.scheduling.volcano.sh",
	"jobs.batch.volcano.sh",
	"jobsets.jobset.x-k8s.io",
	"trainjobs.trainer.kubeflow.org",
	"clustertrainingruntimes.trainer.kubeflow.org",
	"pytorchjobs.kubeflow.org",
	"leaderworkersets.leaderworkerset.x-k8s.io",
	"rayclusters.ray.io",
	"rayjobs.ray.io",
	"rayservices.ray.io",
	"inferenceservices.serving.kserve.io",
	"servingruntimes.serving.kserve.io",
	"clusterservingruntimes.serving.kserve.io",
	"llminferenceservices.serving.kserve.io",
	"inferencepools.inference.networking.k8s.io",
	"inferencepools.inference.networking.x-k8s.io",
	"scaledobjects.keda.sh",
	"nodepools.karpenter.sh",
}

// CountedCRDs returns the ML CRDs whose objects discovery counts, sorted.
func CountedCRDs() []string {
	out := append([]string(nil), countedCRDs...)
	sort.Strings(out)
	return out
}

// customObjectCRDs are the custom resources read in full (redacted), because
// a rule reads their spec: whether the GPU Operator's ClusterPolicy runs its
// DCGM exporter and which counters ConfigMap it names, what an AMD
// DeviceConfig deploys, and an OpenTelemetryCollector's receivers and
// pipelines.
var customObjectCRDs = []string{
	"clusterpolicies.nvidia.com",
	"deviceconfigs.amd.com",
	"opentelemetrycollectors.opentelemetry.io",
}

// resourceSliceKey is DRA's ResourceSlices, counted into
// Snapshot.CustomResources under this key when resource.k8s.io is served. It
// is a built-in API, not a CRD, but the key convention is the same.
const resourceSliceKey = "resourceslices.resource.k8s.io"

// ReadSet is everything `forge ml discover` reads, in the order it reads it.
// Nothing here is a Secret, pods/log, nodes/proxy, or any write.
func ReadSet() []Read {
	rows := []Read{
		{Resource: "/version", Verb: VerbGet, Scope: "full", Why: "the API server's version; also the reachability check"},
		{Resource: "/api, /apis", Verb: VerbGet, Scope: "full", Why: "the API groups the server serves (resource.k8s.io for DRA, kueue.x-k8s.io)"},
		{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", Verb: VerbList, Scope: "metadata", Why: "which CRDs are installed (names only: CRD schemas are never fetched)"},
		{Resource: "nodes", Verb: VerbList, Scope: "full", Why: "accelerator capacity and allocatable, labels, conditions, images, nodeInfo"},
		{Resource: "pods", Verb: VerbList, Scope: "full", Why: "exporters and serving pods by image, accelerator requests (all namespaces)"},
		{Group: "apps", Resource: "daemonsets", Verb: VerbList, Scope: "full", Why: "device plugins, exporters, node-problem-detector, pull caches"},
		{Group: "apps", Resource: "deployments", Verb: VerbList, Scope: "full", Why: "operators and controllers (GPU Operator, Kueue, KServe, KubeRay)"},
		{Group: "apps", Resource: "statefulsets", Verb: VerbList, Scope: "full", Why: "monitoring and serving stacks deployed as StatefulSets"},
		{Group: "batch", Resource: "jobs", Verb: VerbList, Scope: "full", Why: "training jobs and their accelerator requests"},
		{Group: "autoscaling", Resource: "horizontalpodautoscalers", Verb: VerbList, Scope: "full", Why: "how serving workloads scale (autoscaling/v2)"},
		{Group: "resource.k8s.io", Resource: "resourceslices", Verb: VerbList, Scope: "count", When: "resource.k8s.io is served", Why: "whether a DRA driver publishes devices"},
	}
	for _, name := range CountedCRDs() {
		plural, group, _ := strings.Cut(name, ".")
		rows = append(rows, Read{Group: group, Resource: plural, Verb: VerbList, Scope: "count", When: "the CRD is installed", Why: "how many objects of this ML CRD exist"})
	}
	for _, name := range customObjectCRDs {
		plural, group, _ := strings.Cut(name, ".")
		rows = append(rows, Read{Group: group, Resource: plural, Verb: VerbList, Scope: "redacted", When: "the CRD is installed",
			Why: "the spec a rule reads (DCGM exporter and its counters ConfigMap, a collector's receivers and pipelines)"})
	}
	rows = append(rows,
		Read{Resource: "configmaps", Verb: VerbGet, Scope: "redacted", When: "a found component names it",
			Why: "by exact name, never listed: a DCGM counters CSV, NPD monitor JSON, a collector's config, kube-system kubeadm-config and kubelet-config"},
		Read{Resource: "pods/proxy", Verb: VerbGet, Scope: "names", When: "--probe", Probe: true,
			Why: "GET an exporter's /metrics through the API server, at most one pod per component and pool: metric names and label keys only"},
	)
	return rows
}

// Excluded is what discovery never reads, whatever the grant allows.
var Excluded = []string{
	"secrets (any namespace, any verb)",
	"pods/log, pods/exec, pods/portforward (pods/proxy only with --probe, only GET of a metrics path)",
	"listing configmaps (each is read by exact name, from what a found component references)",
	"nodes/proxy (effectively exec on the node)",
	"every write verb: create, update, patch, delete, deletecollection",
}

// ClusterRole is the read set as RBAC: a ClusterRole granting exactly what
// discovery reads without --probe, for a team that runs it under a dedicated
// identity. ConfigMaps are get only: RBAC cannot pin the names, which depend
// on what discovery finds, but the collector never lists them.
func ClusterRole() []rbacv1.PolicyRule { return rbacRules(false) }

// ProbeRules is the extra grant `--probe` needs: get on pods/proxy. It is kept
// apart from ClusterRole because the proxy reaches any port of any pod; grant
// it only where the probe is wanted.
func ProbeRules() []rbacv1.PolicyRule { return rbacRules(true) }

func rbacRules(probe bool) []rbacv1.PolicyRule {
	type key struct{ group, verb string }
	byKey := map[key]map[string]bool{}
	for _, r := range ReadSet() {
		if strings.HasPrefix(r.Resource, "/") || r.Probe != probe {
			continue
		}
		k := key{r.Group, r.Verb}
		if byKey[k] == nil {
			byKey[k] = map[string]bool{}
		}
		byKey[k][r.Resource] = true
	}
	keys := make([]key, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].group != keys[j].group {
			return keys[i].group < keys[j].group
		}
		return keys[i].verb < keys[j].verb
	})
	rules := make([]rbacv1.PolicyRule, 0, len(keys)+1)
	for _, k := range keys {
		res := make([]string, 0, len(byKey[k]))
		for r := range byKey[k] {
			res = append(res, r)
		}
		sort.Strings(res)
		rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{k.group}, Resources: res, Verbs: []string{k.verb}})
	}
	if !probe {
		rules = append(rules, rbacv1.PolicyRule{NonResourceURLs: []string{"/version", "/api", "/api/*", "/apis", "/apis/*"}, Verbs: []string{VerbGet}})
	}
	return rules
}

// ReadSetTable renders ReadSet as aligned text, for --help and the skill:
// one line per read, with the counted ML CRDs folded into one wrapped list.
func ReadSetTable() string {
	var b strings.Builder
	var rows []Read
	var counted []string
	for _, r := range ReadSet() {
		if r.Scope == "count" && r.When == "the CRD is installed" {
			counted = append(counted, r.Name())
			continue
		}
		rows = append(rows, r)
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r.Name()))
	}
	for _, r := range rows {
		when := ""
		if r.When != "" {
			when = " (when " + r.When + ")"
		}
		fmt.Fprintf(&b, "  %-4s %-*s  %-8s %s%s\n", r.Verb, width, r.Name(), r.Scope, r.Why, when)
	}
	b.WriteString("  list each installed ML CRD, counted (metadata only):\n")
	line := "   "
	for i, name := range counted {
		item := " " + name
		if i < len(counted)-1 {
			item += ","
		}
		if len(line)+len(item) > 78 {
			b.WriteString(line + "\n")
			line = "   "
		}
		line += item
	}
	b.WriteString(line + "\n")
	return b.String()
}
