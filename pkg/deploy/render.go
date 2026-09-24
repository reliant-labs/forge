package deploy

import (
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// Context carries the TARGET facts a spec deliberately does not: where the
// objects land, and which project they belong to.
//
// Nothing hosted is in here. There is no org, no customer, no allocated
// hostname and no image allowlist. The control plane stamps those AROUND
// Render's output. If Render took them as inputs it would stop being the same
// function for both destinations.
type Context struct {
	// Namespace every rendered object lands in. Required.
	Namespace string
	// PartOf is the app.kubernetes.io/part-of value (the project). Empty
	// falls back to the object's own name, which matches forge's
	// managed_labels.
	PartOf string
}

func (c Context) validate() error {
	if c.Namespace == "" {
		return errors.New("render context has no namespace: every tier object is namespaced, and guessing one is how a workload lands in another customer's namespace")
	}
	return nil
}

// Identity constants every rendered pod runs under. They are forge's values
// (lib/services.k) and control-plane's (Config.RunAsUser) alike.
const (
	RunAsUser     int64 = 65532
	DataMountPath       = "/data"
	TmpMountPath        = "/tmp"
)

// Label keys forge stamps on every object it renders (kcl/lib/labels.k
// managed_labels). Selectors use ONLY LabelName, because a selector is
// immutable on a Deployment and part-of must stay free to change.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPartOf    = "app.kubernetes.io/part-of"
)

// AnnotationDeletionPolicy is stamped on a ManagedDatabase's Cluster so that
// whatever PRUNES rendered objects can see the author's retain decision
// without reading the spec. A retain-policy Cluster must never be pruned:
// deleting it deletes its volumes.
const AnnotationDeletionPolicy = "forge.dev/deletion-policy"

func managedLabels(name, partOf string) map[string]string {
	if partOf == "" {
		partOf = name
	}
	return map[string]string{LabelName: name, LabelManagedBy: "forge", LabelPartOf: partOf}
}

// Render renders any tier object into the Kubernetes objects that run it. The
// object's metadata.name is the workload name, and its spec is the
// declaration. Its metadata.namespace is NOT consulted: on hosted the CR
// lives where the control plane keeps it, not where the workload runs, so
// the target is always ctx.Namespace.
//
// StaticSite renders NO Kubernetes objects. Its executor is the release
// planner (bucket sync, retention, CDN invalidation), not an apply, so it
// returns an empty list rather than an error: a caller rendering a whole
// environment should not have to special-case it.
func Render(obj runtime.Object, ctx Context) ([]*unstructured.Unstructured, error) {
	switch o := obj.(type) {
	case *v1alpha1.SimpleBackend:
		return RenderSimpleBackend(o.Name, o.Spec, ctx)
	case *v1alpha1.ManagedDatabase:
		return RenderManagedDatabase(o.Name, o.Spec, ctx)
	case *v1alpha1.StaticSite:
		if err := o.Spec.Validate(); err != nil {
			return nil, fmt.Errorf("staticsite %s: %w", o.Name, err)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("deploy.Render: %T is not a forge.dev tier", obj)
	}
}

// RenderSimpleBackend renders a SimpleBackend into its Deployment, Service
// (unless network is none), ServiceAccount, PersistentVolumeClaim (when
// StorageGiB is set) and NetworkPolicy.
//
// It reproduces forge's KCL projection (_project_simple_backend + the shared
// k8s builders, kcl/render.k) field for field. The exceptions are the
// deltas below, each chosen on merit and each pinned by
// TestRenderMatchesKCLProjection, which applies exactly this list to the
// KCL output and then requires equality:
//
//  1. NO Role or RoleBinding. The shared cluster builder grants every
//     workload get/list/watch on ALL configmaps and secrets in its
//     namespace. For this tier that is a read of every other app's
//     credentials, and on hosted a read of the platform secrets that live in
//     the hosted customer's namespace too. Nothing the tier renders needs the API
//     server: env is projected by the kubelet. The ServiceAccount stays, so
//     the pod does not run as `default`, but it gets no token:
//     automountServiceAccountToken is false (control-plane's hardening,
//     proven live).
//  2. fsGroup 65532 on the pod. Without it a PVC mounted at /data is
//     root-owned on most provisioners, and the non-root container cannot
//     write to it. That means storageGiB was silently unusable on forge's
//     path. control-plane set it, and it ran live against real volumes.
//  3. A writable emptyDir at /tmp. readOnlyRootFilesystem is kept, and it
//     breaks any process that writes a temp file, which is most of them.
//     This is control-plane's escape hatch, proven live.
//  4. network none declares NO container port. The KCL projection
//     inherited K8sCluster's default port and emitted containerPort 8080 on
//     a worker that listens on nothing. That was a bug, not a design.
//  5. A NetworkPolicy per backend (generic hardening, merge decision). It
//     is ingress default-deny, with an allow on the declared ports: from
//     the same namespace for private, from anywhere for public, and from
//     nothing for none. Egress is deliberately unrestricted. Addressability
//     is not containment, and egress policy belongs to the namespace (the
//     control plane layers its per-customer egress rules there).
//  6. A TCP probe when healthCheck.path is empty. This capability is new:
//     the KCL HealthCheck could only express an HTTP GET.
//
// What Render does NOT do, because it is hosted policy the operator
// composes around the result: imagePullPolicy Always on shared nodes,
// RuntimeClass and node isolation, the registry allowlist, the SecretRef
// customer-prefix rule, the DatabaseRef org check, hostname routes, and
// quota.
func RenderSimpleBackend(name string, spec v1alpha1.SimpleBackendSpec, ctx Context) ([]*unstructured.Unstructured, error) {
	if err := ctx.validate(); err != nil {
		return nil, err
	}
	if !isDNSLabel(name) {
		return nil, fmt.Errorf("simplebackend name %q must be an RFC-1123 label of at most 63 characters", name)
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("simplebackend %s: %w", name, err)
	}

	ns := ctx.Namespace
	labels := managedLabels(name, ctx.PartOf)
	selector := map[string]string{LabelName: name}
	meta := func(objName string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: objName, Namespace: ns, Labels: copyLabels(labels)}
	}

	res := spec.Resources.WithDefaults()
	uid := RunAsUser
	f, t := false, true

	container := corev1.Container{
		Name:            name,
		Image:           spec.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             renderEnv(spec.Env),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(res.CPURequestMillicores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(res.MemoryRequestBytes, resource.BinarySI),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(res.CPULimitMillicores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(res.MemoryLimitBytes, resource.BinarySI),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             &t,
			RunAsUser:                &uid,
			RunAsGroup:               &uid,
			ReadOnlyRootFilesystem:   &t,
			AllowPrivilegeEscalation: &f,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: TmpMountPath}},
	}
	if spec.ServesTraffic() {
		for _, p := range spec.Ports {
			container.Ports = append(container.Ports, corev1.ContainerPort{Name: portName(p), ContainerPort: p, Protocol: corev1.ProtocolTCP})
		}
	}
	if spec.HealthCheck != nil {
		container.LivenessProbe = renderProbe(*spec.HealthCheck)
		container.ReadinessProbe = renderProbe(*spec.HealthCheck)
	}
	volumes := []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if spec.StorageGiB > 0 {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: DataMountPath})
		volumes = append(volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName(name)},
		}})
	}

	replicas := int32(1) // ALWAYS 1: storageGiB is ReadWriteOnce. See SimpleBackendSpec.
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: meta(name),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(labels)},
				Spec: corev1.PodSpec{
					ServiceAccountName:           name,
					AutomountServiceAccountToken: &f,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &t,
						RunAsUser:      &uid,
						RunAsGroup:     &uid,
						FSGroup:        &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes:    volumes,
				},
			},
		},
	}

	objs := []runtime.Object{deployment}
	if spec.ServesTraffic() {
		svc := &corev1.Service{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: meta(name),
			Spec:       corev1.ServiceSpec{Selector: selector},
		}
		for _, p := range spec.Ports {
			svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: portName(p), Port: p, TargetPort: intstr.FromInt32(p), Protocol: corev1.ProtocolTCP})
		}
		objs = append(objs, svc)
	}
	objs = append(objs, &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   meta(name),
		AutomountServiceAccountToken: &f,
	})
	if spec.StorageGiB > 0 {
		objs = append(objs, &corev1.PersistentVolumeClaim{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
			ObjectMeta: meta(PVCName(name)),
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(int64(spec.StorageGiB)<<30, resource.BinarySI),
				}},
			},
		})
	}
	objs = append(objs, renderNetworkPolicy(name, spec, meta(name+"-ingress"), selector))
	return toUnstructured(objs)
}

// PVCName is the claim backing a backend's storageGiB. The name is exported
// because an observer must read back the claim this package emitted.
func PVCName(backendName string) string { return backendName + "-data" }

// portName matches forge's "port-<n>" (lib/services.k). IANA port names cap at
// 15 characters, and "port-65535" is 10.
func portName(p int32) string { return fmt.Sprintf("port-%d", p) }

func renderEnv(env []v1alpha1.EnvVar) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	out := make([]corev1.EnvVar, 0, len(env))
	for _, e := range env {
		ev := corev1.EnvVar{Name: e.Name}
		switch {
		case e.SecretRef != nil:
			ev.ValueFrom = secretKey(e.SecretRef.Name, e.SecretRef.Key)
		case e.ManagedSecret != "":
			ev.ValueFrom = secretKey(v1alpha1.ManagedSecretsSecretName, e.ManagedSecret)
		case e.DatabaseRef != nil:
			ev.ValueFrom = secretKey(DatabaseCredentialSecretName(e.DatabaseRef.Name), string(e.DatabaseRef.EffectiveKey()))
		default:
			ev.Value = e.Value
		}
		out = append(out, ev)
	}
	return out
}

func secretKey(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key,
	}}
}

// renderProbe derives one probe from the HealthCheck. It is called twice
// rather than sharing a pointer: liveness and readiness are separate objects,
// so a later per-probe change cannot silently apply to both.
func renderProbe(hc v1alpha1.HealthCheck) *corev1.Probe {
	hc = hc.WithDefaults()
	handler := corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(hc.Port)}}
	if hc.Path != "" {
		handler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: hc.Path, Port: intstr.FromInt32(hc.Port)}}
	}
	return &corev1.Probe{
		ProbeHandler:        handler,
		InitialDelaySeconds: hc.InitialDelaySeconds,
		PeriodSeconds:       hc.PeriodSeconds,
		TimeoutSeconds:      hc.TimeoutSeconds,
		FailureThreshold:    hc.FailureThreshold,
	}
}

func renderNetworkPolicy(name string, spec v1alpha1.SimpleBackendSpec, meta metav1.ObjectMeta, selector map[string]string) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: meta,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if !spec.ServesTraffic() {
		return np // no rules: ingress default-deny
	}
	tcp := corev1.ProtocolTCP
	rule := networkingv1.NetworkPolicyIngressRule{}
	for _, p := range spec.Ports {
		port := intstr.FromInt32(p)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	if !spec.IsPublic() {
		// An empty podSelector with no namespaceSelector means "every pod in
		// THIS namespace".
		rule.From = []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	}
	np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{rule}
	return np
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
	return []*unstructured.Unstructured{cluster}, nil
}

// toUnstructured converts typed objects and removes ONLY the noise the typed
// encoding carries for fields an applier never writes: nulls, the
// server-owned status, metadata.creationTimestamp, and the zero Deployment
// strategy.
//
// This is deliberately NOT a general "drop empty maps" prune. In Kubernetes
// an empty map is frequently the MEANING. `podSelector: {}` in a NetworkPolicy
// peer means "every pod in this namespace", and dropping it turns a private
// backend's policy into allow-from-anywhere. `emptyDir: {}` is the volume
// source. TestRenderKeepsSemanticEmptyMaps pins both.
func toUnstructured(objs []runtime.Object) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			return nil, fmt.Errorf("convert %T: %w", o, err)
		}
		dropNils(m)
		delete(m, "status")
		stripCreationTimestamps(m)
		if spec, ok := m["spec"].(map[string]any); ok {
			if s, ok := spec["strategy"].(map[string]any); ok && len(s) == 0 {
				delete(spec, "strategy")
			}
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
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
// (the object's own and a pod template's). Both are server-owned.
func stripCreationTimestamps(v any) {
	switch t := v.(type) {
	case map[string]any:
		if md, ok := t["metadata"].(map[string]any); ok {
			delete(md, "creationTimestamp")
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

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
