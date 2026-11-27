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
// settings), the credentials in every container's command and args, and the
// value of every HTTP probe or lifecycle hook header. ConfigMap data and
// custom object specs are redacted (redact.go).
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
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/reliant-labs/forge/internal/mldiscover"
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
	r := &reader{c: c, page: opts.PageSize,
		s:         &mldiscover.Snapshot{Schema: mldiscover.SnapshotSchema, Context: opts.Context},
		preferred: map[string]string{}, servedAt: map[string][]string{}, crds: map[string]bool{}}
	if r.page <= 0 {
		r.page = PageSize
	}
	v, err := c.Kube.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	r.s.ServerVersion = v.GitVersion

	r.discoverAPIs()
	r.listCRDs(ctx)
	r.listCore(ctx)
	r.listApps(ctx)
	r.listBatchAndAutoscaling(ctx)
	r.countCustomResources(ctx)
	r.readCustomObjects(ctx)
	refs := opts.ConfigRefs
	if refs == nil {
		refs = mldiscover.ConfigRefs
	}
	r.readConfigMaps(ctx, refs(r.s))

	sort.SliceStable(r.s.ReadErrors, func(i, j int) bool { return r.s.ReadErrors[i].Resource < r.s.ReadErrors[j].Resource })
	r.s.CollectedAt = now().UTC().Truncate(time.Second)
	return r.s, nil
}

// reader is one Collect: the clients, the snapshot it fills, and what API
// discovery and the CRD list told it.
type reader struct {
	c    Clients
	page int64
	s    *mldiscover.Snapshot
	// preferred is each served group's preferred version; servedAt the
	// versions each "plural.group" is served at; crds the installed CRDs.
	preferred map[string]string
	servedAt  map[string][]string
	crds      map[string]bool
}

func (r *reader) fail(resource string, err error) {
	r.s.ReadErrors = append(r.s.ReadErrors, mldiscover.ReadError{Resource: resource, Error: err.Error()})
}

// discoverAPIs records the served groups and the versions each serves a
// resource at: what a CRD's objects are read through. One aggregated-discovery
// request on a current API server.
//
// A group whose discovery failed (an aggregated API that is not ready, such
// as metrics-server's metrics.k8s.io on a fresh cluster) leaves the rest of
// the answer intact: the partial result is used, a failed group the read set
// does not need is ignored, and one it needs is a ReadError naming that
// group/version.
func (r *reader) discoverAPIs() {
	groups, lists, err := r.c.Kube.Discovery().ServerGroupsAndResources()
	if err != nil {
		var failed *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &failed) {
			r.fail("/apis", err)
		} else {
			needed := readSetGroups()
			for gv, gerr := range failed.Groups {
				if needed[gv.Group] {
					r.fail("/apis/"+gv.String(), gerr)
				}
			}
		}
	}
	for _, g := range groups {
		if g != nil && g.Name != "" {
			r.s.APIGroups = append(r.s.APIGroups, g.Name)
			r.preferred[g.Name] = g.PreferredVersion.Version
		}
	}
	sort.Strings(r.s.APIGroups)
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
				r.servedAt[key] = append(r.servedAt[key], gv.Version)
			}
		}
	}
}

// readSetGroups is every API group a ReadSet row reads from.
func readSetGroups() map[string]bool {
	out := map[string]bool{}
	for _, row := range ReadSet() {
		if !strings.HasPrefix(row.Resource, "/") {
			out[row.Group] = true
		}
	}
	return out
}

// versionOf is the version a resource is read at: its group's preferred
// version when served there, else the first version that serves it
// (opentelemetry.io prefers v1beta1, where instrumentations is not).
func (r *reader) versionOf(group, resource string) string {
	versions := r.servedAt[resource+"."+group]
	if len(versions) == 0 || slices.Contains(versions, r.preferred[group]) {
		return r.preferred[group]
	}
	return versions[0]
}

func (r *reader) listCRDs(ctx context.Context) {
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	items, err := r.metadataList(ctx, crdGVR)
	if err != nil {
		r.fail("customresourcedefinitions.apiextensions.k8s.io", err)
		return
	}
	for _, it := range items {
		r.crds[it.Name] = true
		r.s.CRDs = append(r.s.CRDs, it.Name)
	}
	sort.Strings(r.s.CRDs)
}

func (r *reader) listCore(ctx context.Context) {
	core := r.c.Kube.CoreV1()
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*corev1.NodeList, error) {
		return core.Nodes().List(ctx, o)
	}, func(l *corev1.NodeList) []corev1.Node { return l.Items }); err != nil {
		r.fail("nodes", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripNode(&items[i])
		}
		r.s.Nodes = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*corev1.PodList, error) {
		return core.Pods(metav1.NamespaceAll).List(ctx, o)
	}, func(l *corev1.PodList) []corev1.Pod { return l.Items }); err != nil {
		r.fail("pods", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripPodSpec(&items[i].Spec)
			stripPodStatus(&items[i].Status)
		}
		r.s.Pods = items
	}
}

func (r *reader) listApps(ctx context.Context) {
	apps := r.c.Kube.AppsV1()
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*appsv1.DaemonSetList, error) {
		return apps.DaemonSets(metav1.NamespaceAll).List(ctx, o)
	}, func(l *appsv1.DaemonSetList) []appsv1.DaemonSet { return l.Items }); err != nil {
		r.fail("daemonsets.apps", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
		}
		r.s.DaemonSets = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*appsv1.DeploymentList, error) {
		return apps.Deployments(metav1.NamespaceAll).List(ctx, o)
	}, func(l *appsv1.DeploymentList) []appsv1.Deployment { return l.Items }); err != nil {
		r.fail("deployments.apps", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
		}
		r.s.Deployments = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*appsv1.StatefulSetList, error) {
		return apps.StatefulSets(metav1.NamespaceAll).List(ctx, o)
	}, func(l *appsv1.StatefulSetList) []appsv1.StatefulSet { return l.Items }); err != nil {
		r.fail("statefulsets.apps", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
			for j := range items[i].Spec.VolumeClaimTemplates {
				strip(&items[i].Spec.VolumeClaimTemplates[j].ObjectMeta)
			}
		}
		r.s.StatefulSets = items
	}
}

func (r *reader) listBatchAndAutoscaling(ctx context.Context) {
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*batchv1.JobList, error) {
		return r.c.Kube.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, o)
	}, func(l *batchv1.JobList) []batchv1.Job { return l.Items }); err != nil {
		r.fail("jobs.batch", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
			stripTemplate(&items[i].Spec.Template)
		}
		r.s.Jobs = items
	}
	if items, err := listAll(ctx, r.page, func(ctx context.Context, o metav1.ListOptions) (*autoscalingv2.HorizontalPodAutoscalerList, error) {
		return r.c.Kube.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, o)
	}, func(l *autoscalingv2.HorizontalPodAutoscalerList) []autoscalingv2.HorizontalPodAutoscaler {
		return l.Items
	}); err != nil {
		r.fail("horizontalpodautoscalers.autoscaling", err)
	} else {
		for i := range items {
			strip(&items[i].ObjectMeta)
		}
		r.s.HPAs = items
	}
}

// countCustomResources counts DRA's ResourceSlices when the group is served,
// then each ML CRD that is installed, through the metadata API.
func (r *reader) countCustomResources(ctx context.Context) {
	count := func(key, group, resource string) {
		version := r.versionOf(group, resource)
		if version == "" {
			r.fail(key, fmt.Errorf("the CRD is installed but API group %s is not served", group))
			return
		}
		items, err := r.metadataList(ctx, schema.GroupVersionResource{Group: group, Version: version, Resource: resource})
		if err != nil {
			r.fail(key, err)
			return
		}
		if r.s.CustomResources == nil {
			r.s.CustomResources = map[string]int{}
		}
		r.s.CustomResources[key] = len(items)
	}
	if r.preferred["resource.k8s.io"] != "" {
		count(resourceSliceKey, "resource.k8s.io", "resourceslices")
	}
	for _, name := range CountedCRDs() {
		if !r.crds[name] {
			continue
		}
		plural, group, _ := strings.Cut(name, ".")
		count(name, group, plural)
	}
}

// readCustomObjects reads, redacted, the custom objects whose spec a rule
// reads.
func (r *reader) readCustomObjects(ctx context.Context) {
	for _, name := range customObjectCRDs {
		if !r.crds[name] {
			continue
		}
		plural, group, _ := strings.Cut(name, ".")
		version := r.versionOf(group, plural)
		if version == "" {
			continue // already reported by countCustomResources
		}
		objs, err := r.dynamicList(ctx, schema.GroupVersionResource{Group: group, Version: version, Resource: plural})
		if err != nil {
			r.fail(name, err)
			continue
		}
		if r.s.CustomObjects == nil {
			r.s.CustomObjects = map[string][]map[string]any{}
		}
		r.s.CustomObjects[name] = objs
	}
}

// readConfigMaps is phase two: the ConfigMaps the found components
// reference, each GET by name, redacted.
func (r *reader) readConfigMaps(ctx context.Context, refs []mldiscover.ObjectRef) {
	seen := map[string]bool{}
	for _, ref := range refs {
		id := ref.Namespace + "/" + ref.Name
		if ref.Name == "" || seen[id] {
			continue
		}
		seen[id] = true
		cm, err := r.c.Kube.CoreV1().ConfigMaps(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// Absent is an answer, not a refused read: a ConfigMap named
			// by convention (kubeadm-config on a cluster kubeadm did not
			// build) simply is not there, and discovery says so from
			// its absence in Snapshot.ConfigMaps.
			continue
		}
		if err != nil {
			r.fail("configmaps", fmt.Errorf("%s (%s): %w", id, ref.Why, err))
			continue
		}
		RedactConfigMap(cm)
		r.s.ConfigMaps = append(r.s.ConfigMaps, *cm)
	}
	sort.Slice(r.s.ConfigMaps, func(i, j int) bool {
		a, b := r.s.ConfigMaps[i], r.s.ConfigMaps[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
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
	redactStringMap(m.Labels)
}

func stripTemplate(t *corev1.PodTemplateSpec) {
	strip(&t.ObjectMeta)
	stripPodSpec(&t.Spec)
}

// stripPodSpec removes the credentials a pod spec can carry. It blanks every
// literal env value outside EnvAllowlist: the name stays (a rule asks whether
// OTEL_EXPORTER_OTLP_* or NCCL_* is set), and so does valueFrom, which only
// names a Secret or ConfigMap key and holds no data. It redacts every
// container's command and args as the one argv they form (RedactArgv), its
// exec probes and lifecycle
// hooks likewise, and replaces the value of every HTTP header a probe or hook
// sends (an Authorization header is the usual one), keeping the name. A CSI
// volume's attributes and a FlexVolume's options keep only values under
// keys that do not name a credential. Volume URLs lose embedded credentials,
// inline Windows GMSA documents are replaced whole, and embedded PVC metadata
// follows the same stripping rules as top-level metadata.
func stripPodSpec(p *corev1.PodSpec) {
	for i := range p.InitContainers {
		stripContainer(&p.InitContainers[i])
	}
	for i := range p.Containers {
		stripContainer(&p.Containers[i])
	}
	for i := range p.EphemeralContainers {
		c := &p.EphemeralContainers[i].EphemeralContainerCommon
		c.Image = RedactArgText(c.Image)
		if c.SecurityContext != nil {
			stripWindowsOptions(c.SecurityContext.WindowsOptions)
		}
		blankEnv(c.Env)
		c.Command, c.Args = RedactArgv(c.Command, c.Args)
		for _, pr := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
			stripHandler(probeHandler(pr))
		}
		stripLifecycle(c.Lifecycle)
	}
	for i := range p.Volumes {
		v := &p.Volumes[i].VolumeSource
		if v.CSI != nil {
			redactStringMap(v.CSI.VolumeAttributes)
		}
		if v.FlexVolume != nil {
			redactStringMap(v.FlexVolume.Options)
		}
		if v.GitRepo != nil {
			v.GitRepo.Repository = RedactArgText(v.GitRepo.Repository)
		}
		if v.AzureDisk != nil {
			v.AzureDisk.DataDiskURI = RedactArgText(v.AzureDisk.DataDiskURI)
		}
		if v.ScaleIO != nil {
			v.ScaleIO.Gateway = RedactArgText(v.ScaleIO.Gateway)
		}
		if v.Ephemeral != nil && v.Ephemeral.VolumeClaimTemplate != nil {
			strip(&v.Ephemeral.VolumeClaimTemplate.ObjectMeta)
		}
	}
	if p.SecurityContext != nil {
		stripWindowsOptions(p.SecurityContext.WindowsOptions)
	}
}

// Core-object retention audit: metadata annotations/managed fields are dropped;
// label values are sanitized. Pod specs retain scheduling/resource/security
// settings and object/key/path references (Secret references are not data).
// Inline data surfaces are env, argv, probe/hook headers and paths, volume
// option maps/URLs, and GMSA documents, all handled above. Node specs/status
// retain addresses, IDs, resource quantities, version information and reference
// names; provider/image URLs are sanitized below. Status reason codes, phases,
// timestamps and readiness stay, but diagnostic messages do not.
//
// Status messages are arbitrary application/runtime output, not inventory.
// Keep scheduling state, resource counts, identities and references, but never
// persist diagnostic text that can echo credentials (including terminated execs).
func stripPodStatus(s *corev1.PodStatus) {
	s.Message = ""
	for i := range s.Conditions {
		s.Conditions[i].Message = ""
	}
	for _, statuses := range [][]corev1.ContainerStatus{s.ContainerStatuses, s.InitContainerStatuses, s.EphemeralContainerStatuses} {
		for i := range statuses {
			statuses[i].Image = RedactArgText(statuses[i].Image)
			statuses[i].ImageID = RedactArgText(statuses[i].ImageID)
			for _, state := range []*corev1.ContainerState{&statuses[i].State, &statuses[i].LastTerminationState} {
				if state.Waiting != nil {
					state.Waiting.Message = ""
				}
				if state.Terminated != nil {
					state.Terminated.Message = ""
				}
			}
		}
	}
}

func stripNode(n *corev1.Node) {
	n.Spec.ProviderID = RedactArgText(n.Spec.ProviderID)
	if n.Status.Config != nil {
		n.Status.Config.Error = ""
	}
	for i := range n.Status.Conditions {
		n.Status.Conditions[i].Message = ""
	}
	for i := range n.Status.Images {
		for j := range n.Status.Images[i].Names {
			n.Status.Images[i].Names[j] = RedactArgText(n.Status.Images[i].Names[j])
		}
	}
}

func stripWindowsOptions(w *corev1.WindowsSecurityContextOptions) {
	if w != nil && w.GMSACredentialSpec != nil {
		v := Redacted
		w.GMSACredentialSpec = &v
	}
}

func stripContainer(c *corev1.Container) {
	c.Image = RedactArgText(c.Image)
	if c.SecurityContext != nil {
		stripWindowsOptions(c.SecurityContext.WindowsOptions)
	}
	blankEnv(c.Env)
	c.Command, c.Args = RedactArgv(c.Command, c.Args)
	for _, pr := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		stripHandler(probeHandler(pr))
	}
	stripLifecycle(c.Lifecycle)
}

func blankEnv(env []corev1.EnvVar) {
	for j := range env {
		if !EnvAllowlist[env[j].Name] {
			env[j].Value = ""
		}
	}
}

func probeHandler(pr *corev1.Probe) *corev1.ProbeHandler {
	if pr == nil {
		return nil
	}
	return &pr.ProbeHandler
}

func stripLifecycle(l *corev1.Lifecycle) {
	if l == nil {
		return
	}
	for _, h := range []*corev1.LifecycleHandler{l.PostStart, l.PreStop} {
		if h == nil {
			continue
		}
		stripHandler(&corev1.ProbeHandler{Exec: h.Exec, HTTPGet: h.HTTPGet})
	}
}

// stripHandler redacts an exec command and replaces every HTTP header value.
func stripHandler(h *corev1.ProbeHandler) {
	if h == nil {
		return
	}
	if h.Exec != nil {
		h.Exec.Command = RedactArgs(h.Exec.Command)
	}
	if h.HTTPGet != nil {
		for i := range h.HTTPGet.HTTPHeaders {
			h.HTTPGet.HTTPHeaders[i].Value = Redacted
		}
		h.HTTPGet.Path = RedactArgText(h.HTTPGet.Path)
	}
}

func redactStringMap(m map[string]string) {
	for k, v := range m {
		if SensitiveKey.MatchString(k) {
			m[k] = Redacted
		} else {
			m[k] = RedactArgText(v)
		}
	}
}
