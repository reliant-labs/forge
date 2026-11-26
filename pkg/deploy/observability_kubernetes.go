package deploy

import (
	"fmt"
	"regexp"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The cluster collector (kcl/schema.k KubernetesInventory): one collector
// per cluster that opts in, beside the node agent and exporting to the same
// destination with the same credential. Where the node agent collects what
// each node sees (pods' telemetry, kubelet stats, logs), this one reads the
// cluster's own objects through the API server: Nodes, DaemonSets and
// Deployments, and Events. It is a single replica, because k8sobjects read
// by two would ship every row twice.
//
// Every row it ships is a log record carrying the resource attribute
// ClusterNameAttribute (the agent's cluster, its kube context), k8sobjects'
// own `k8s.resource.name` attribute ("nodes", "daemonsets", "deployments",
// "events") and the object as its JSON body (an Event: the watch envelope,
// `{type, object}`). It sets no service.name.
const (
	// ClusterCollectorName names the cluster collector's Deployment,
	// ConfigMap and ServiceAccount; its ClusterRole and binding are
	// "<namespace>-cluster-collector".
	ClusterCollectorName = "cluster-collector"
	// ClusterNameAttribute is the resource attribute naming the cluster
	// (its kube context) on the cluster collector's rows and, with
	// KubernetesInventory, on the node agent's scrapes.
	ClusterNameAttribute = "k8s.cluster.name"
	// NodeNameAttribute is the resource attribute naming the node on the
	// node agent's scrapes, with KubernetesInventory.
	NodeNameAttribute = "k8s.node.name"
	// scrapeClusterProcessor is the node agent's processor stamping the
	// cluster and node on its scrape pipelines.
	scrapeClusterProcessor    = "resource/k8s_cluster"
	clusterCollectorMemoryMiB = 256
)

// KubernetesInventory is what a cluster's ObservabilityAgent record asks
// for when the env opts in (kcl/schema.k KubernetesInventory). Nil renders
// none.
type KubernetesInventory struct {
	// Cluster is the agent's cluster (its kube context), stamped as
	// ClusterNameAttribute.
	Cluster string `json:"cluster"`
	// NodeInterval is how often Nodes are listed; WorkloadInterval how
	// often DaemonSets and Deployments are. Collector durations ("60s").
	NodeInterval     string `json:"nodeInterval"`
	WorkloadInterval string `json:"workloadInterval"`
	// Events watches events.k8s.io Events.
	Events bool `json:"events"`
}

var durationRE = regexp.MustCompile(`^[0-9]+(ms|s|m|h)$`)

func (c KubernetesInventory) validate() error {
	if c.Cluster == "" {
		return fmt.Errorf("kubernetes inventory: cluster is required")
	}
	for field, v := range map[string]string{"nodeInterval": c.NodeInterval, "workloadInterval": c.WorkloadInterval} {
		if !durationRE.MatchString(v) {
			return fmt.Errorf("kubernetes inventory: %s %q is not a duration like 60s", field, v)
		}
	}
	return nil
}

// ClusterCollectorConfig is the cluster collector's configuration, as YAML.
func (s ObservabilityAgentSpec) ClusterCollectorConfig(c KubernetesInventory) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := c.validate(); err != nil {
		return "", err
	}
	objects := []any{
		map[string]any{"name": "nodes", "mode": "pull", "interval": c.NodeInterval},
		map[string]any{"name": "daemonsets", "group": "apps", "mode": "pull", "interval": c.WorkloadInterval},
		map[string]any{"name": "deployments", "group": "apps", "mode": "pull", "interval": c.WorkloadInterval},
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
	limit := clusterCollectorMemoryMiB
	cfg := map[string]any{
		"extensions": extensions,
		"receivers": map[string]any{
			"k8sobjects": map[string]any{"auth_type": "serviceAccount", "objects": objects},
		},
		"processors": map[string]any{
			"memory_limiter": map[string]any{"check_interval": "1s", "limit_mib": limit * 80 / 100, "spike_limit_mib": limit * 20 / 100},
			"resource": map[string]any{"attributes": []any{
				map[string]any{"key": ClusterNameAttribute, "value": c.Cluster, "action": "upsert"},
			}},
			"batch": map[string]any{},
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

// stampScrapeCluster adds ClusterNameAttribute and NodeNameAttribute to
// each of the node agent's declared-telemetry scrape pipelines
// (`metrics/telemetry_<service>`), before its batch processor, when the
// agent's cluster opts in to its Kubernetes inventory, so a scraped row
// joins the inventory's Nodes. Other pipelines are untouched.
func (s ObservabilityAgentSpec) stampScrapeCluster(cfg map[string]any) {
	if s.KubernetesInventory == nil {
		return
	}
	pipelines, _ := cfg["service"].(map[string]any)["pipelines"].(map[string]any)
	processors, _ := cfg["processors"].(map[string]any)
	stamped := false
	for _, t := range s.declaredTelemetry() {
		p, ok := pipelines["metrics/telemetry_"+t.ServiceName].(map[string]any)
		if !ok {
			continue
		}
		stamped = true
		var out []any
		inserted := false
		for _, v := range p["processors"].([]any) {
			if v == "batch" && !inserted {
				out = append(out, scrapeClusterProcessor)
				inserted = true
			}
			out = append(out, v)
		}
		if !inserted {
			out = append(out, scrapeClusterProcessor)
		}
		p["processors"] = out
	}
	if stamped {
		// The agent pulls only its own node's pods (DiscoverNodePods; an
		// address it would pull from every node is refused), so every
		// scraped row's pod runs on K8S_NODE_NAME. A scrape row otherwise
		// carries no k8s.node.name, and an exporter's own host label is
		// often not a node name.
		processors[scrapeClusterProcessor] = map[string]any{"attributes": []any{
			map[string]any{"key": ClusterNameAttribute, "value": s.KubernetesInventory.Cluster, "action": "upsert"},
			map[string]any{"key": NodeNameAttribute, "value": "${env:K8S_NODE_NAME}", "action": "upsert"},
		}}
	}
}

// renderClusterCollector renders the cluster collector's ServiceAccount,
// read-only ClusterRole + binding, ConfigMap and one-replica Deployment.
func renderClusterCollector(s ObservabilityAgentSpec, c KubernetesInventory, ctx ObservabilityContext) ([]runtime.Object, error) {
	cfg, err := s.ClusterCollectorConfig(c)
	if err != nil {
		return nil, fmt.Errorf("cluster collector: %w", err)
	}
	name, ns := ClusterCollectorName, ctx.Namespace
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
	limit := clusterCollectorMemoryMiB
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
