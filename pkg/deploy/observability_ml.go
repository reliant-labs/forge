package deploy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The ML cluster collector (kcl/ml.k MLCollector): one collector per cluster
// an MLStack declares, beside the node agent and exporting to the same
// destination with the same credential. Where the node agent collects what
// each node sees (pods' telemetry, kubelet stats, logs), this one reads the
// cluster's own objects through the API server: Node inventory, the
// DaemonSets and Deployments that make up the ML stack, and Events. It is a
// single replica, because k8sobjects read by two would ship every row twice.
//
// Every row it ships carries the resource attributes
// MLClusterNameAttribute (the MLStack's cluster) and MLCollectorAttribute =
// "cluster"; each log record carries k8sobjects' own
// `k8s.resource.name` attribute ("nodes", "daemonsets", "deployments",
// "events") and the object as its JSON body.
const (
	// MLCollectorName names the ML cluster collector's Deployment,
	// ConfigMap and ServiceAccount; its ClusterRole and binding are
	// "<namespace>-ml-cluster-collector".
	MLCollectorName = "ml-cluster-collector"
	// MLClusterNameAttribute is the resource attribute naming the MLStack's
	// cluster (its kube context) on the collector's rows and on the node
	// agent's ML scrapes.
	MLClusterNameAttribute = "k8s.cluster.name"
	// MLCollectorAttribute marks the collector's rows; its value is "cluster".
	MLCollectorAttribute = "forge.ml.collector"
	// mlTelemetrySourcePrefix is the Source of a Telemetry target an
	// MLStack Adopt or Install lowers to: "ml-stack:<cluster>".
	mlTelemetrySourcePrefix = "ml-stack:"
	// mlClusterProcessor is the node agent's processor stamping the cluster
	// on an ML scrape's pipeline.
	mlClusterProcessor   = "resource/ml_cluster"
	mlCollectorMemoryMiB = 256
)

// MLClusterCollector is the ML cluster collector a cluster's
// ObservabilityAgent record asks for (kcl/ml.k MLCollector). Nil renders none.
type MLClusterCollector struct {
	// Cluster is the MLStack's cluster (its kube context), stamped as
	// MLClusterNameAttribute.
	Cluster string `json:"cluster"`
	// NodeInterval is how often Nodes are listed; InventoryInterval how
	// often DaemonSets and Deployments are. Collector durations ("60s").
	NodeInterval      string `json:"nodeInterval"`
	InventoryInterval string `json:"inventoryInterval"`
	// Events watches events.k8s.io Events.
	Events bool `json:"events"`
}

var durationRE = regexp.MustCompile(`^[0-9]+(ms|s|m|h)$`)

func (c MLClusterCollector) validate() error {
	if c.Cluster == "" {
		return fmt.Errorf("ml collector: cluster is required")
	}
	for field, v := range map[string]string{"nodeInterval": c.NodeInterval, "inventoryInterval": c.InventoryInterval} {
		if !durationRE.MatchString(v) {
			return fmt.Errorf("ml collector: %s %q is not a duration like 60s", field, v)
		}
	}
	return nil
}

// MLCollectorConfig is the ML cluster collector's configuration, as YAML.
func (s ObservabilityAgentSpec) MLCollectorConfig(c MLClusterCollector) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := c.validate(); err != nil {
		return "", err
	}
	objects := []any{
		map[string]any{"name": "nodes", "mode": "pull", "interval": c.NodeInterval},
		map[string]any{"name": "daemonsets", "group": "apps", "mode": "pull", "interval": c.InventoryInterval},
		map[string]any{"name": "deployments", "group": "apps", "mode": "pull", "interval": c.InventoryInterval},
	}
	if c.Events {
		objects = append(objects, map[string]any{"name": "events", "group": "events.k8s.io", "mode": "watch"})
	}
	// No persistent queue: a Deployment has no host directory to keep one
	// in, and a pull re-reads the whole inventory on its next interval.
	exp := s.exporter()
	delete(exp["sending_queue"].(map[string]any), "storage")
	extensions := map[string]any{"health_check": map[string]any{"endpoint": "0.0.0.0:13133"}}
	extNames := []string{"health_check"}
	if s.TokenSecret != nil {
		extensions[bearerAuthName] = bearerExtension()
		extNames = append(extNames, bearerAuthName)
	}
	limit := mlCollectorMemoryMiB
	cfg := map[string]any{
		"extensions": extensions,
		"receivers": map[string]any{
			"k8sobjects": map[string]any{"auth_type": "serviceAccount", "objects": objects},
		},
		"processors": map[string]any{
			"memory_limiter": map[string]any{"check_interval": "1s", "limit_mib": limit * 80 / 100, "spike_limit_mib": limit * 20 / 100},
			"resource":       mlClusterResource(c.Cluster, true),
			"batch":          map[string]any{},
		},
		"exporters": map[string]any{s.exporterName(): exp},
		"service": map[string]any{
			"extensions": strs(extNames...),
			"pipelines": map[string]any{
				"logs": map[string]any{
					"receivers":  strs("k8sobjects"),
					"processors": strs("memory_limiter", "resource", "batch"),
					"exporters":  strs(s.exporterName()),
				},
			},
		},
	}
	return marshalConfig(cfg)
}

// mlClusterResource is the resource processor stamping the cluster, and on
// the cluster collector also MLCollectorAttribute.
func mlClusterResource(cluster string, collector bool) map[string]any {
	attrs := []any{map[string]any{"key": MLClusterNameAttribute, "value": cluster, "action": "upsert"}}
	if collector {
		attrs = append(attrs, map[string]any{"key": MLCollectorAttribute, "value": "cluster", "action": "upsert"})
	}
	return map[string]any{"attributes": attrs}
}

// stampMLScrapeCluster adds MLClusterNameAttribute to the node agent's
// pipeline of each ML scrape (a target whose Source is "ml-stack:<cluster>"),
// before its batch processor. Other pipelines are untouched.
func (s ObservabilityAgentSpec) stampMLScrapeCluster(cfg map[string]any) {
	pipelines, _ := cfg["service"].(map[string]any)["pipelines"].(map[string]any)
	processors, _ := cfg["processors"].(map[string]any)
	for _, t := range s.declaredTelemetry() {
		cluster, ok := strings.CutPrefix(t.Source, mlTelemetrySourcePrefix)
		if !ok || cluster == "" {
			continue
		}
		p, ok := pipelines["metrics/telemetry_"+t.ServiceName].(map[string]any)
		if !ok {
			continue
		}
		name := mlClusterProcessor + "/" + t.ServiceName
		// The agent discovers only its own node's pods (DiscoverNodePods),
		// so every scraped row's exporter pod runs on K8S_NODE_NAME: stamp
		// it, since a scrape row otherwise carries no k8s.node.name and
		// DCGM's Hostname label is often not a node name.
		res := mlClusterResource(cluster, false)
		res["attributes"] = append(res["attributes"].([]any),
			map[string]any{"key": "k8s.node.name", "value": "${env:K8S_NODE_NAME}", "action": "upsert"})
		processors[name] = res
		var out []any
		inserted := false
		for _, v := range p["processors"].([]any) {
			if v == "batch" && !inserted {
				out = append(out, name)
				inserted = true
			}
			out = append(out, v)
		}
		if !inserted {
			out = append(out, name)
		}
		p["processors"] = out
	}
}

// renderMLCollector renders the ML cluster collector's ServiceAccount,
// read-only ClusterRole + binding, ConfigMap and one-replica Deployment.
func renderMLCollector(s ObservabilityAgentSpec, c MLClusterCollector, ctx ObservabilityContext) ([]runtime.Object, error) {
	cfg, err := s.MLCollectorConfig(c)
	if err != nil {
		return nil, fmt.Errorf("ml cluster collector: %w", err)
	}
	name, ns := MLCollectorName, ctx.Namespace
	labels := managedLabels(name, ctx.PartOf)
	selector := map[string]string{LabelName: name}
	read := []string{"get", "list", "watch"}
	sa := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: objectMeta(name, ns, labels)}
	// Read-only, and only what the receivers list: no Secrets, no
	// nodes/proxy, no write verb.
	role := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: objectMeta(ns+"-"+name, "", labels),
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes", "events"}, Verbs: read},
			{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: read},
			{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "deployments"}, Verbs: read},
		},
	}
	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: objectMeta(ns+"-"+name, "", labels),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: ns + "-" + name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ns}},
	}
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: objectMeta(name, ns, labels),
		Data:       map[string]string{"config.yaml": cfg},
	}
	limit := mlCollectorMemoryMiB
	env := []corev1.EnvVar{{Name: "GOMEMLIMIT", Value: strconv.Itoa(limit*80/100) + "MiB"}}
	if s.HeadersSecret != nil {
		env = append(env, corev1.EnvVar{Name: agentHeadersEnv, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: s.HeadersSecret.Name}, Key: s.HeadersSecret.Key},
		}})
	}
	volumes := []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}}
	mounts := []corev1.VolumeMount{{Name: "config", MountPath: "/etc/otel", ReadOnly: true}}
	if s.TokenSecret != nil {
		// The node agent's token Secret, mounted the same way (a whole
		// directory, optional) so a renewal reaches it with no restart.
		optional := true
		volumes = append(volumes, corev1.Volume{Name: "otlp-token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: s.TokenSecret.Name,
			Optional:   &optional,
			Items:      []corev1.KeyToPath{{Key: s.TokenSecret.Key, Path: agentTokenFile}},
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "otlp-token", MountPath: agentTokenDir, ReadOnly: true})
	}
	replicas, nonRoot, noEscalate, readOnly, uid := int32(1), true, false, true, int64(10001)
	probe := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(13133)}}
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: objectMeta(name, ns, labels),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			// Never two at once: both would ship the same inventory.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(labels)},
				Spec: corev1.PodSpec{
					ServiceAccountName: name,
					EnableServiceLinks: new(false),
					Containers: []corev1.Container{{
						Name:           "collector",
						Image:          s.Image,
						Args:           []string{"--config=/etc/otel/config.yaml"},
						Env:            env,
						Ports:          []corev1.ContainerPort{{Name: "health", ContainerPort: 13133, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: probe},
						LivenessProbe:  &corev1.Probe{ProbeHandler: probe, InitialDelaySeconds: 10},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse(strconv.Itoa(limit/2) + "Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(strconv.Itoa(limit) + "Mi")},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &nonRoot,
							RunAsUser:                &uid,
							AllowPrivilegeEscalation: &noEscalate,
							ReadOnlyRootFilesystem:   &readOnly,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	return []runtime.Object{sa, role, binding, cm, dep}, nil
}
