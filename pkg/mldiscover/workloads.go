package mldiscover

import (
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Workload kinds a WorkloadRef names.
const (
	WorkloadDaemonSet   = "DaemonSet"
	WorkloadDeployment  = "Deployment"
	WorkloadStatefulSet = "StatefulSet"
	WorkloadJob         = "Job"
	WorkloadPod         = "Pod"
)

// WorkloadRef names the object that owns a pod.
type WorkloadRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// String is Kind/namespace/name.
func (w WorkloadRef) String() string { return w.Kind + "/" + w.Namespace + "/" + w.Name }

// PodOwner resolves the workload that owns a pod: its DaemonSet,
// StatefulSet or Job by owner reference, its Deployment through the
// ReplicaSet's name and the Deployment's selector, else the pod itself
// (a bare pod, or an owner the snapshot does not hold).
func PodOwner(s *Snapshot, pod *corev1.Pod) WorkloadRef {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case WorkloadDaemonSet, WorkloadStatefulSet, WorkloadJob:
			return WorkloadRef{Kind: ref.Kind, Namespace: pod.Namespace, Name: ref.Name}
		case "ReplicaSet":
			for i := range s.Deployments {
				d := &s.Deployments[i]
				if d.Namespace == pod.Namespace && strings.HasPrefix(ref.Name, d.Name+"-") && selects(d.Spec.Selector, pod.Labels) {
					return WorkloadRef{Kind: WorkloadDeployment, Namespace: d.Namespace, Name: d.Name}
				}
			}
			// The Deployment is not in the snapshot: strip the
			// pod-template hash.
			if i := strings.LastIndexByte(ref.Name, '-'); i > 0 {
				return WorkloadRef{Kind: WorkloadDeployment, Namespace: pod.Namespace, Name: ref.Name[:i]}
			}
		}
	}
	return WorkloadRef{Kind: WorkloadPod, Namespace: pod.Namespace, Name: pod.Name}
}

func selects(sel *metav1.LabelSelector, l map[string]string) bool {
	if sel == nil {
		return false
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil || s.Empty() {
		return false
	}
	return s.Matches(labels.Set(l))
}

// NodeCoverage returns the nodes a component's pods run on, sorted. It
// reads the component's pods (by owner) when the snapshot holds any, and
// otherwise, for a DaemonSet, the nodes its node selector, required node
// affinity and tolerations admit. A component with no workload in the
// snapshot (a DCGM exporter known only from the GPU Operator's
// ClusterPolicy) covers every node with the component's vendor's
// accelerators.
func NodeCoverage(s *Snapshot, c Component) []string {
	set := map[string]bool{}
	kind, name, _ := strings.Cut(c.Workload, "/")
	if c.Workload != "" {
		for i := range s.Pods {
			p := &s.Pods[i]
			if p.Spec.NodeName == "" || !PodActive(p) || p.Namespace != c.Namespace {
				continue
			}
			o := PodOwner(s, p)
			if o.Kind == kind && o.Name == name {
				set[p.Spec.NodeName] = true
			}
		}
		if len(set) == 0 && kind == WorkloadDaemonSet {
			for i := range s.DaemonSets {
				ds := &s.DaemonSets[i]
				if ds.Namespace == c.Namespace && ds.Name == name {
					for _, n := range daemonSetNodes(s, ds) {
						set[n] = true
					}
				}
			}
		}
	} else {
		for i := range s.Nodes {
			if acc, ok := NodeAcceleratorOf(&s.Nodes[i]); ok && (c.Vendor == "" || acc.Vendor == c.Vendor) {
				set[s.Nodes[i].Name] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// daemonSetNodes simulates where a DaemonSet schedules: node selector,
// required node affinity, and NoSchedule/NoExecute taints.
func daemonSetNodes(s *Snapshot, ds *appsv1.DaemonSet) []string {
	spec := &ds.Spec.Template.Spec
	var out []string
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if !labels.SelectorFromSet(spec.NodeSelector).Matches(labels.Set(n.Labels)) {
			continue
		}
		if !requiredAffinityMatches(spec.Affinity, n.Labels) || !tolerates(spec.Tolerations, n.Spec.Taints) {
			continue
		}
		out = append(out, n.Name)
	}
	return out
}

func requiredAffinityMatches(a *corev1.Affinity, l map[string]string) bool {
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	terms := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		return true
	}
	for _, t := range terms {
		if termMatches(t, l) {
			return true
		}
	}
	return false
}

func termMatches(t corev1.NodeSelectorTerm, l map[string]string) bool {
	for _, e := range t.MatchExpressions {
		v, has := l[e.Key]
		switch e.Operator {
		case corev1.NodeSelectorOpIn:
			if !has || !contains(e.Values, v) {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if has && contains(e.Values, v) {
				return false
			}
		case corev1.NodeSelectorOpExists:
			if !has {
				return false
			}
		case corev1.NodeSelectorOpDoesNotExist:
			if has {
				return false
			}
		}
	}
	return true
}

func tolerates(tols []corev1.Toleration, taints []corev1.Taint) bool {
	for i := range taints {
		t := &taints[i]
		if t.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		ok := false
		for j := range tols {
			if toleratesTaint(&tols[j], t) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// toleratesTaint is the Equal/Exists toleration match (the comparison
// operators of newer releases are not needed to place an exporter).
func toleratesTaint(tol *corev1.Toleration, t *corev1.Taint) bool {
	if tol.Effect != "" && tol.Effect != t.Effect {
		return false
	}
	if tol.Key != "" && tol.Key != t.Key {
		return false
	}
	switch tol.Operator {
	case corev1.TolerationOpExists:
		return true
	case "", corev1.TolerationOpEqual:
		return tol.Key != "" && tol.Value == t.Value
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Containers lists a pod spec's init and regular containers, init first.
func Containers(spec *corev1.PodSpec) []*corev1.Container {
	out := make([]*corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	for i := range spec.InitContainers {
		out = append(out, &spec.InitContainers[i])
	}
	for i := range spec.Containers {
		out = append(out, &spec.Containers[i])
	}
	return out
}
