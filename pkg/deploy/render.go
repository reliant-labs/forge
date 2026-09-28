package deploy

import (
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// Context carries the TARGET facts a spec deliberately does not: where the
// objects land, which project and environment they belong to, and the
// env-wide network policy.
//
// Nothing hosted is in here. There is no org, no customer, no allocated
// hostname and no image allowlist. The control plane stamps those AROUND the
// renderer's output. If the renderer took them as inputs it would stop being
// the same function for both destinations.
type Context struct {
	// Namespace every rendered object lands in. Required.
	Namespace string
	// PartOf is the app.kubernetes.io/part-of value (the project). Empty
	// falls back to each object's own name (kcl/lib/labels.k:185-191).
	PartOf string
	// Env is the forge.dev/env ownership stamp (kcl/lib/labels.k). It is
	// written to every object's labels and every pod template's labels,
	// never to a selector. Empty emits no label, matching labels.k's
	// "no -D env= binding" case.
	Env string
	// Network, when set, adds the ENV-WIDE NetworkPolicy bundle
	// (kcl/lib/netpol.k): default-deny plus the DNS, same-namespace,
	// external-egress, telemetry and ingress-controller allows.
	//
	// WHY IT IS HERE, AND OPTIONAL. The per-workload ingress policy is a
	// fact about one workload (which of ITS ports may be dialled, and from
	// where), so RenderWorkloads always emits it. The bundle is a fact about
	// the NAMESPACE: which egress the environment's database, collector and
	// gateway need. RenderWorkloads already renders a whole env's set, which
	// is the one place that knows every workload shares the namespace, so
	// the bundle rides the same call instead of a second KCL renderer
	// (kcl/workloads/render.k:171 today). It is a pointer because the two
	// executors want different answers: a self-hosted env declares its
	// egress here, while the hosted control plane passes nil and layers its
	// own per-customer egress rules on the namespace it owns.
	Network *EnvNetworkPolicy
}

// EnvNetworkPolicy is the input to the env-wide NetworkPolicy bundle
// (kcl/lib/netpol.k:53, WorkloadEnv's egress_ports / telemetry_namespace /
// ingress_namespace, kcl/workloads/render.k:96-116).
type EnvNetworkPolicy struct {
	// EgressPorts are TCP ports every pod may dial anywhere: 443 for HTTPS,
	// 5432 for a managed Postgres outside the cluster. Empty omits the
	// allow-egress-external policy.
	EgressPorts []int32
	// TelemetryNamespace runs the OTLP collector. Empty omits the policy.
	TelemetryNamespace string
	// IngressNamespace runs the Gateway / ingress controller. Empty omits
	// the policy.
	IngressNamespace string
}

func (c Context) validate() error {
	if c.Namespace == "" {
		return errors.New("render context has no namespace: every tier object is namespaced, and guessing one is how a workload lands in another customer's namespace")
	}
	return nil
}

// RunAsUser is the identity every rendered pod runs under. It is forge's value
// (lib/services.k) and control-plane's (Config.RunAsUser) alike.
const RunAsUser int64 = 65532

// Label keys forge stamps on every object it renders (kcl/lib/labels.k
// managed_labels). Selectors use ONLY LabelName, because a selector is
// immutable on a Deployment and part-of must stay free to change.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPartOf    = "app.kubernetes.io/part-of"
	// LabelEnv is the env-ownership stamp (kcl/lib/labels.k:84).
	LabelEnv = "forge.dev/env"
	// LabelJobName is a Job's STABLE handle, since its metadata.name carries
	// a spec hash (kcl/workloads/expand.k:706-709).
	LabelJobName = "forge.dev/job-name"
	// LabelSpecHash answers "did this Job actually change?" without a YAML
	// diff.
	LabelSpecHash = "forge.dev/spec-hash"
)

// AnnotationDeployPhase places a standalone Job in the deploy
// (kcl/workloads/expand.k:714-718). internal/cluster reads it.
const AnnotationDeployPhase = "forge.dev/deploy-phase"

// AnnotationDeletionPolicy is stamped on a ManagedDatabase's Cluster so that
// whatever PRUNES rendered objects can see the author's retain decision
// without reading the spec. A retain-policy Cluster must never be pruned:
// deleting it deletes its volumes.
const AnnotationDeletionPolicy = "forge.dev/deletion-policy"

// managedLabels ports kcl/lib/labels.k:185-191.
func managedLabels(name, partOf string) map[string]string {
	if partOf == "" {
		partOf = name
	}
	return map[string]string{LabelName: name, LabelManagedBy: "forge", LabelPartOf: partOf}
}

// Render renders one StaticSite or ManagedDatabase. The object's
// metadata.name is the resource name and its spec is the declaration. Its
// metadata.namespace is NOT consulted: on hosted the CR lives where the
// control plane keeps it, not where the workload runs, so the target is
// always ctx.Namespace.
//
// A Workload is REFUSED here. Its `before` ordering and workloadURL
// references are relations between workloads, so one Workload cannot be
// rendered correctly on its own. Render the env's whole set with
// RenderWorkloads.
//
// StaticSite renders NO Kubernetes objects. Its executor is the release
// planner (bucket sync, retention, CDN invalidation), not an apply, so it
// returns an empty list rather than an error: a caller rendering a whole
// environment should not have to special-case it.
func Render(obj runtime.Object, ctx Context) ([]*unstructured.Unstructured, error) {
	switch o := obj.(type) {
	case *v1alpha1.ManagedDatabase:
		return RenderManagedDatabase(o.Name, o.Spec, ctx)
	case *v1alpha1.StaticSite:
		if err := o.Spec.Validate(); err != nil {
			return nil, fmt.Errorf("staticsite %s: %w", o.Name, err)
		}
		return nil, nil
	case *v1alpha1.Workload:
		return nil, fmt.Errorf("deploy.Render: workload %s must be rendered with its environment's other workloads (deploy.RenderWorkloads): `before` and workloadURL are resolved across the set", o.Name)
	default:
		return nil, fmt.Errorf("deploy.Render: %T is not a forge.dev tier", obj)
	}
}

// DatabaseCredentialSecretName is the Secret CloudNativePG publishes for a
// Cluster's application role: "<cluster>-app", in the Cluster's namespace.
// This is a CONTRACT WITH CNPG, not a name forge chooses. A DatabaseRef env
// var reads it directly, because the Cluster renders into the same namespace
// as the workload that references it.
func DatabaseCredentialSecretName(databaseName string) string { return databaseName + "-app" }

// DatabaseIdentifiers are the Postgres objects a ManagedDatabase creates. The
// Cluster name is the resource name unchanged. The database is that name with
// hyphens folded to underscores, so no reference ever has to remember to quote
// it. The role is the database plus "_app": it is distinct from the database
// and it is never `postgres`.
func DatabaseIdentifiers(name string) (database, role string) {
	database = strings.ReplaceAll(name, "-", "_")
	return database, database + "_app"
}

// RenderManagedDatabase renders a ManagedDatabase into a CloudNativePG Cluster.
//
// This is control-plane's closed shape (manageddatabase.BuildClusterPlan),
// the one that ran live. enableSuperuserAccess is written as false rather
// than left to CNPG's default: the value is the difference between a customer
// holding an owner role and holding `postgres`, and a CNPG release changing
// its default must not change that. There is NO backup stanza. That is a
// gate, not an omission: a Cluster rendered with a backup section would LOOK
// backed up while no restore had ever been drilled.
//
// The Cluster is unstructured on purpose. Importing CNPG's typed API would
// pull the whole operator module into every forge consumer to type-check six
// fields.
//
// Self-hosted, applying this requires the CNPG operator in the cluster. The
// executor should detect the postgresql.cnpg.io CRD and fail with a runbook
// if it is missing.
func RenderManagedDatabase(name string, spec v1alpha1.ManagedDatabaseSpec, ctx Context) ([]*unstructured.Unstructured, error) {
	if err := ctx.validate(); err != nil {
		return nil, err
	}
	if err := v1alpha1.ValidateDatabaseName(name); err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("manageddatabase %s: %w", name, err)
	}
	spec = spec.WithDefaults()
	database, role := DatabaseIdentifiers(name)

	labels := map[string]any{}
	for k, v := range managedLabels(name, ctx.PartOf) {
		labels[k] = v
	}
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   ctx.Namespace,
			"labels":      labels,
			"annotations": map[string]any{AnnotationDeletionPolicy: string(spec.DeletionPolicy)},
		},
		"spec": map[string]any{
			"instances":             int64(spec.Instances),
			"enableSuperuserAccess": false,
			"bootstrap": map[string]any{
				"initdb": map[string]any{"database": database, "owner": role},
			},
			"storage": map[string]any{"size": fmt.Sprintf("%dGi", spec.StorageGiB)},
		},
	}}
	stampEnv([]*unstructured.Unstructured{cluster}, ctx.Env)
	return []*unstructured.Unstructured{cluster}, nil
}

// toUnstructured converts typed objects and removes ONLY the noise the typed
// encoding carries for fields an applier never writes: nulls, the
// server-owned status, metadata.creationTimestamp (and a metadata block left
// empty by removing it), and the zero Deployment strategy.
//
// This is deliberately NOT a general "drop empty maps" prune. In Kubernetes
// an empty map is frequently the MEANING. `podSelector: {}` in a NetworkPolicy
// peer means "every pod in this namespace", and dropping it turns a private
// workload's policy into allow-from-anywhere. `emptyDir: {}` is the volume
// source. TestRenderKeepsSemanticEmptyMaps pins both.
func toUnstructured(objs []runtime.Object) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		m, err := toMap(o)
		if err != nil {
			return nil, err
		}
		delete(m, "status")
		if spec, ok := m["spec"].(map[string]any); ok {
			if s, ok := spec["strategy"].(map[string]any); ok && len(s) == 0 {
				delete(spec, "strategy")
			}
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// toMap is the typed → map conversion shared by toUnstructured and the Job
// spec hash, so the hash is taken over exactly what is emitted.
func toMap(o any) (map[string]any, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, fmt.Errorf("convert %T: %w", o, err)
	}
	dropNils(m)
	stripCreationTimestamps(m)
	return m, nil
}

func dropNils(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if val == nil {
				delete(t, k)
				continue
			}
			dropNils(val)
		}
	case []any:
		for _, val := range t {
			dropNils(val)
		}
	}
}

// stripCreationTimestamps removes metadata.creationTimestamp at every depth
// (the object's own, a pod template's, a CronJob's job template's). All are
// server-owned. A template metadata block that held nothing else is removed
// with it: an empty metadata map never means anything.
func stripCreationTimestamps(v any) {
	switch t := v.(type) {
	case map[string]any:
		if md, ok := t["metadata"].(map[string]any); ok {
			delete(md, "creationTimestamp")
			if len(md) == 0 {
				delete(t, "metadata")
			}
		}
		for _, val := range t {
			stripCreationTimestamps(val)
		}
	case []any:
		for _, val := range t {
			stripCreationTimestamps(val)
		}
	}
}

// stampEnv ports kcl/lib/labels.k stamp_env_all: forge.dev/env on every
// object's metadata.labels, and on the pod template of the kinds whose
// spec.template IS one (labels.k:97 _POD_TEMPLATE_KINDS) or a CronJob's
// spec.jobTemplate.spec.template. Never a selector (labels.k:64-74). A key
// already present is left as written (labels.k:112-123).
func stampEnv(objs []*unstructured.Unstructured, env string) {
	if env == "" {
		return
	}
	add := func(meta map[string]any) {
		labels, _ := meta["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			meta["labels"] = labels
		}
		if _, ok := labels[LabelEnv]; !ok {
			labels[LabelEnv] = env
		}
	}
	templateMeta := func(parent map[string]any) {
		tmpl, ok := parent["template"].(map[string]any)
		if !ok {
			return
		}
		md, _ := tmpl["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
			tmpl["metadata"] = md
		}
		add(md)
	}
	for _, o := range objs {
		md, _ := o.Object["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
			o.Object["metadata"] = md
		}
		add(md)
		spec, _ := o.Object["spec"].(map[string]any)
		switch o.GetKind() {
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
			if spec != nil {
				templateMeta(spec)
			}
		case "CronJob":
			if jt, ok := spec["jobTemplate"].(map[string]any); ok {
				if js, ok := jt["spec"].(map[string]any); ok {
					templateMeta(js)
				}
			}
		}
	}
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func objectMeta(name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: copyLabels(labels)}
}

func isDNSLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(s)-1
		if !ok {
			return false
		}
	}
	return true
}
