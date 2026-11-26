// Package mlcollect reads a cluster into an mldiscover.Snapshot for
// `forge ml discover`: read-only, through the user's own kube context, with
// exactly the reads ReadSet lists.
//
// Every read is a list (or a discovery GET), paginated, and a refused or
// failed list becomes a Snapshot.ReadErrors entry rather than an error: an
// RBAC grant narrower than the read set yields a smaller snapshot that says
// what it could not see. Only an unreachable API server is an error.
//
// The read has two phases. The first lists (ReadSet). The second GETs, by
// exact name, the ConfigMaps mldiscover.ConfigRefs names from what the first
// found (an exporter's counters CSV, NPD's monitor JSON, a collector's
// config, kube-system's kubeadm-config and kubelet-config): ConfigMaps are
// never listed.
//
// Before an object is stored the collector strips its managedFields and every
// annotation, and blanks the literal value of every container env var (the
// name and any valueFrom reference stay; EnvAllowlist keeps three DCGM
// settings). ConfigMap data and custom object specs are redacted (redact.go).
// A snapshot is written to .forge/state and may be shared as a fixture or
// with a bug report, so it must not carry a credential.
//
// Probe (probe.go) is separate and opt-in: it reads exporters' /metrics
// through the API server's pod proxy.
package mlcollect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// PageSize is the Limit of every list.
const PageSize = 500

// Clients are the three API clients the collector reads through, all from
// one rest.Config: typed lists and discovery, the metadata API (CRD names and
// object counts, so CRD schemas and CR bodies are never sent), and the
// dynamic client (the few custom objects whose spec a rule reads).
type Clients struct {
	Kube     kubernetes.Interface
	Metadata metadata.Interface
	Dynamic  dynamic.Interface
}

// NewClients builds Clients from a rest.Config.
func NewClients(cfg *rest.Config) (Clients, error) {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, err
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return Clients{}, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Clients{}, err
	}
	return Clients{Kube: kube, Metadata: md, Dynamic: dyn}, nil
}

// RESTConfig loads the rest.Config of a kube context from the default
// kubeconfig loading rules ($KUBECONFIG, else ~/.kube/config). An empty
// context is the current-context. It returns the context name it resolved.
func RESTConfig(kubeContext string) (*rest.Config, string, error) {
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: kubeContext})
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("read kubeconfig: %w", err)
	}
	name := kubeContext
	if name == "" {
		name = raw.CurrentContext
	}
	if name == "" {
		return nil, "", errors.New("the kubeconfig has no current-context: name one with --context")
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, "", fmt.Errorf("kube context %q is not in the kubeconfig", name)
	}
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kube context %q: %w", name, err)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.UserAgent = "forge-ml-discover"
	// A read of a large cluster is a few dozen paged lists: the client's
	// default 5 QPS would make it take minutes for no benefit.
	cfg.QPS, cfg.Burst = 50, 100
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	return cfg, name, nil
}

// ErrUnreachable wraps the error of a cluster the collector could not reach
// at all.
var ErrUnreachable = errors.New("cluster unreachable")

// Options tunes Collect.
type Options struct {
	// Context is recorded on the snapshot.
	Context string
	// Now stamps CollectedAt; nil is time.Now.
	Now func() time.Time
	// PageSize overrides PageSize (tests).
	PageSize int64
	// ConfigRefs names the ConfigMaps the second phase GETs, from the first
	// phase's snapshot; nil is mldiscover.ConfigRefs.
	ConfigRefs func(*mldiscover.Snapshot) []mldiscover.ObjectRef
}

// Collect reads the cluster into a Snapshot. It errors only when the API
// server cannot be reached (wrapping ErrUnreachable); every other failure is
// a ReadErrors entry.
func Collect(ctx context.Context, c Clients, opts Options) (*mldiscover.Snapshot, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	r := &reader{c: c, page: opts.PageSize}
	if r.page <= 0 {
		r.page = PageSize
	}
	s := &mldiscover.Snapshot{Schema: mldiscover.SnapshotSchema, Context: opts.Context}

	v, err := c.Kube.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	s.ServerVersion = v.GitVersion

	// Served groups and the versions each serves a resource at: what a CRD's
	// objects are read through. One aggregated-discovery request on a
	// current API server. A resource is read at its group's preferred
	// version when served there, else at the first version that serves it
	// (opentelemetry.io prefers v1beta1, where instrumentations is not).
	preferred := map[string]string{}
	servedAt := map[string][]string{} // "plural.group" → versions
	groups, lists, err := c.Kube.Discovery().ServerGroupsAndResources()
	if err != nil {
		r.fail(s, "/apis", err)
	}
	for _, g := range groups {
		if g != nil && g.Name != "" {
			s.APIGroups = append(s.APIGroups, g.Name)
			preferred[g.Name] = g.PreferredVersion.Version
		}
	}
	sort.Strings(s.APIGroups)
	for _, l := range lists {
		if l == nil {
			continue
		}
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, res := range l.APIResources {
			if !strings.Contains(res.Name, "/") {
				key := res.Name + "." + gv.Group
				servedAt[key] = append(servedAt[key], gv.Version)
			}
		}
	}
	versionOf := func(group, resource string) string {
		versions := servedAt[resource+"."+group]
		if len(versions) == 0 || slices.Contains(versions, preferred[group]) {
			return preferred[group]
		}
		return versions[0]
	}

	crds := map[string]bool{}
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	if items, err := r.metadataList(ctx, crdGVR); err != nil {
		r.fail(s, "customresourcedefinitions.apiextensions.k8s.io", err)
	} else {
		for _, it := range items {
			crds[it.Name] = true
			s.CRDs = append(s.CRDs, it.Name)
		}
		sort.Strings(s.CRDs)
	}

	core := c.Kube.CoreV1()
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*corev1.NodeList, error) {
		return core.Nodes().List(ctx, o)
	}, func(l *corev1.NodeList) []corev1.Node { return l.Items }); err != nil {
		r.fail(s, "nodes", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
		}
		s.Nodes = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*corev1.PodList, error) {
		return core.Pods(metav1.NamespaceAll).List(ctx, o)
	}, func(l *corev1.PodList) []corev1.Pod { return l.Items }); err != nil {
		r.fail(s, "pods", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripPodSpec(&items[i].Spec)
		}
		s.Pods = items
	}
	apps := c.Kube.AppsV1()
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*appsv1.DaemonSetList, error) {
		return apps.DaemonSets(metav1.NamespaceAll).List(ctx, o)
	}, func(l *appsv1.DaemonSetList) []appsv1.DaemonSet { return l.Items }); err != nil {
		r.fail(s, "daemonsets.apps", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
		}
		s.DaemonSets = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*appsv1.DeploymentList, error) {
		return apps.Deployments(metav1.NamespaceAll).List(ctx, o)
	}, func(l *appsv1.DeploymentList) []appsv1.Deployment { return l.Items }); err != nil {
		r.fail(s, "deployments.apps", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
		}
		s.Deployments = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*appsv1.StatefulSetList, error) {
		return apps.StatefulSets(metav1.NamespaceAll).List(ctx, o)
	}, func(l *appsv1.StatefulSetList) []appsv1.StatefulSet { return l.Items }); err != nil {
		r.fail(s, "statefulsets.apps", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
			for j := range items[i].Spec.VolumeClaimTemplates {
				strip(&items[i].Spec.VolumeClaimTemplates[j].ObjectMeta)
			}
		}
		s.StatefulSets = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*batchv1.JobList, error) {
		return c.Kube.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, o)
	}, func(l *batchv1.JobList) []batchv1.Job { return l.Items }); err != nil {
		r.fail(s, "jobs.batch", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
		}
		s.Jobs = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*autoscalingv2.HorizontalPodAutoscalerList, error) {
		return c.Kube.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, o)
	}, func(l *autoscalingv2.HorizontalPodAutoscalerList) []autoscalingv2.HorizontalPodAutoscaler {
		return l.Items
	}); err != nil {
		r.fail(s, "horizontalpodautoscalers.autoscaling", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
		}
		s.HPAs = items
	}

	// Counts: DRA's ResourceSlices when the group is served, then each ML
	// CRD that is installed, through the metadata API.
	count := func(key, group, resource string) {
		version := versionOf(group, resource)
		if version == "" {
			r.fail(s, key, fmt.Errorf("the CRD is installed but API group %s is not served", group))
			return
		}
		items, err := r.metadataList(ctx, schema.GroupVersionResource{Group: group, Version: version, Resource: resource})
		if err != nil {
			r.fail(s, key, err)
			return
		}
		if s.CustomResources == nil {
			s.CustomResources = map[string]int{}
		}
		s.CustomResources[key] = len(items)
	}
	if preferred["resource.k8s.io"] != "" {
		count(resourceSliceKey, "resource.k8s.io", "resourceslices")
	}
	for _, name := range CountedCRDs() {
		if !crds[name] {
			continue
		}
		plural, group, _ := strings.Cut(name, ".")
		count(name, group, plural)
	}

	// Full custom objects, for the specs rules read.
	for _, name := range customObjectCRDs {
		if !crds[name] {
			continue
		}
		plural, group, _ := strings.Cut(name, ".")
		version := versionOf(group, plural)
		if version == "" {
			continue // already reported by count
		}
		objs, err := r.dynamicList(ctx, schema.GroupVersionResource{Group: group, Version: version, Resource: plural})
		if err != nil {
			r.fail(s, name, err)
			continue
		}
		if s.CustomObjects == nil {
			s.CustomObjects = map[string][]map[string]any{}
		}
		s.CustomObjects[name] = objs
	}

	// Phase two: the ConfigMaps the found components reference, by name.
	refs := opts.ConfigRefs
	if refs == nil {
		refs = mldiscover.ConfigRefs
	}
	seen := map[string]bool{}
	for _, ref := range refs(s) {
		id := ref.Namespace + "/" + ref.Name
		if ref.Name == "" || seen[id] {
			continue
		}
		seen[id] = true
		cm, err := core.ConfigMaps(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// Absent is an answer, not a refused read: a ConfigMap named
			// by convention (kubeadm-config on a cluster kubeadm did not
			// build) simply is not there, and discovery says so from
			// its absence in Snapshot.ConfigMaps.
			continue
		}
		if err != nil {
			r.fail(s, "configmaps", fmt.Errorf("%s (%s): %w", id, ref.Why, err))
			continue
		}
		RedactConfigMap(cm)
		s.ConfigMaps = append(s.ConfigMaps, *cm)
	}
	sort.Slice(s.ConfigMaps, func(i, j int) bool {
		a, b := s.ConfigMaps[i], s.ConfigMaps[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	sort.SliceStable(s.ReadErrors, func(i, j int) bool { return s.ReadErrors[i].Resource < s.ReadErrors[j].Resource })
	s.CollectedAt = now().UTC().Truncate(time.Second)
	return s, nil
}

type reader struct {
	c    Clients
	page int64
}

func (r *reader) fail(s *mldiscover.Snapshot, resource string, err error) {
	s.ReadErrors = append(s.ReadErrors, mldiscover.ReadError{Resource: resource, Error: err.Error()})
}

// listAll pages through a typed list, PageSize objects at a time.
func listAll[L interface{ GetContinue() string }, T any](ctx context.Context, page int64,
	list func(context.Context, metav1.ListOptions) (L, error), items func(L) []T,
) ([]T, error) {
	var out []T
	opts := metav1.ListOptions{Limit: page}
	for {
		l, err := list(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, items(l)...)
		next := l.GetContinue()
		if next == "" {
			return out, nil
		}
		opts.Continue = next
	}
}

func (r *reader) metadataList(ctx context.Context, gvr schema.GroupVersionResource) ([]metav1.PartialObjectMetadata, error) {
	return listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*metav1.PartialObjectMetadataList, error) {
		return r.c.Metadata.Resource(gvr).List(ctx, o)
	}, func(l *metav1.PartialObjectMetadataList) []metav1.PartialObjectMetadata { return l.Items })
}

func (r *reader) dynamicList(ctx context.Context, gvr schema.GroupVersionResource) ([]map[string]any, error) {
	var out []map[string]any
	opts := metav1.ListOptions{Limit: r.page}
	for {
		l, err := r.c.Dynamic.Resource(gvr).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			u := l.Items[i]
			u.SetManagedFields(nil)
			u.SetAnnotations(nil)
			out = append(out, RedactObject(u.Object))
		}
		next := l.GetContinue()
		if next == "" {
			return out, nil
		}
		opts.Continue = next
	}
}

// strip removes what a snapshot never keeps from an object's metadata.
func strip(m *metav1.ObjectMeta) {
	m.ManagedFields = nil
	m.Annotations = nil
}

func stripTemplate(t *corev1.PodTemplateSpec) {
	strip(&t.ObjectMeta)
	stripPodSpec(&t.Spec)
}

// stripPodSpec blanks every literal env value outside EnvAllowlist. The name
// stays (a rule asks whether OTEL_EXPORTER_OTLP_* or NCCL_* is set), and so
// does valueFrom, which only names a Secret or ConfigMap key and holds no
// data.
func stripPodSpec(p *corev1.PodSpec) {
	blank := func(env []corev1.EnvVar) {
		for j := range env {
			if !EnvAllowlist[env[j].Name] {
				env[j].Value = ""
			}
		}
	}
	for _, cs := range [][]corev1.Container{p.InitContainers, p.Containers} {
		for i := range cs {
			blank(cs[i].Env)
		}
	}
	for i := range p.EphemeralContainers {
		blank(p.EphemeralContainers[i].Env)
	}
}
