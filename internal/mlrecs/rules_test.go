package mlrecs

import (
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/reliant-labs/forge/internal/mldiscover"
)

var at = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

func rl(kv ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		out[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return out
}

type nodeOpt func(*corev1.Node)

func withLabels(kv ...string) nodeOpt {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for i := 0; i < len(kv); i += 2 {
			n.Labels[kv[i]] = kv[i+1]
		}
	}
}

func withAllocatable(list corev1.ResourceList) nodeOpt {
	return func(n *corev1.Node) { n.Status.Allocatable = list }
}

func withImage(name string, size int64) nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Images = append(n.Status.Images, corev1.ContainerImage{Names: []string{name}, SizeBytes: size})
	}
}

func withConditions(types ...string) nodeOpt {
	return func(n *corev1.Node) {
		for _, t := range types {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeConditionType(t), Status: corev1.ConditionFalse})
		}
	}
}

func withRuntime(rt string) nodeOpt {
	return func(n *corev1.Node) { n.Status.NodeInfo.ContainerRuntimeVersion = rt }
}

// gpuNode is a Ready node with n × resource.
func gpuNode(name, res, n string, opts ...nodeOpt) corev1.Node {
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status: corev1.NodeStatus{
			Capacity: rl(res, n), Allocatable: rl(res, n),
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{ContainerRuntimeVersion: "containerd://2.1.4", KubeletVersion: "v1.35.0"},
		},
	}
	for _, o := range opts {
		o(&node)
	}
	return node
}

func ctr(name, image string, gpus string, args ...string) corev1.Container {
	c := corev1.Container{Name: name, Image: image, Args: args}
	if gpus != "" {
		c.Resources.Limits = rl(mldiscover.ResourceNVIDIAGPU, gpus)
	}
	return c
}

// accelPod is a running pod owned by a ReplicaSet of Deployment owner.
func accelPod(ns, owner, name, node string, c ...corev1.Container) corev1.Pod {
	ctrl := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": owner},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: owner + "-abc12", Controller: &ctrl}}},
		Spec:   corev1.PodSpec{NodeName: node, Containers: c},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func deployment(ns, name, image string, labels map[string]string, c ...corev1.Container) appsv1.Deployment {
	if len(c) == 0 {
		c = []corev1.Container{{Name: name, Image: image}}
	}
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{Containers: c}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}

func daemonSet(ns, name, image string, tolerateAll bool) appsv1.DaemonSet {
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image, Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9400}}}}}},
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 1},
	}
	if tolerateAll {
		ds.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	return ds
}

func gpuJob(ns, name string, labels map[string]string, done bool) batchv1.Job {
	j := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("t", "train:1", "1")}}}},
	}
	if done {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

func snap(mut func(s *mldiscover.Snapshot)) *mldiscover.Snapshot {
	s := &mldiscover.Snapshot{Schema: mldiscover.SnapshotSchema, Context: "test", ServerVersion: "v1.35.0", CollectedAt: at}
	mut(s)
	return s
}

func eval(s *mldiscover.Snapshot, decisions ...Decision) []Finding {
	return Evaluate(s, mldiscover.Discover(s), Options{Decisions: decisions})
}

func byID(fs []Finding, id string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// npd makes the cluster have node health, so tests of other rules are not
// noisy.
var npd = withConditions("KernelDeadlock")

func TestRules(t *testing.T) {
	gke := withLabels(mldiscover.LabelGKENodePool, "gpu")
	cases := []struct {
		name  string
		id    string
		s     *mldiscover.Snapshot
		want  int
		check func(t *testing.T, f Finding)
	}{
		{"telemetry missing: install the exporter", "accel-telemetry-missing", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixKCL || !strings.HasPrefix(f.Fix.KCLDiff, "--- a/ml.k\n+++ b/ml.k\n") || !strings.Contains(f.Fix.KCLDiff, "+            metrics = forge.Install {") {
				t.Errorf("fix: %+v", f.Fix)
			}
		}},
		{"telemetry missing: their operator has the exporter off", "accel-telemetry-missing", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.CRDs = []string{"clusterpolicies.nvidia.com"}
			s.CustomObjects = map[string][]map[string]any{"clusterpolicies.nvidia.com": {{"metadata": map[string]any{"name": "cluster-policy"}, "spec": map[string]any{"dcgmExporter": map[string]any{"enabled": false}}}}}
			s.Deployments = []appsv1.Deployment{deployment("gpu-operator", "gpu-operator", "nvcr.io/nvidia/gpu-operator:v24.9.2", nil)}
		}), 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixManual || !strings.Contains(f.Fix.Summary, "ClusterPolicy") || f.Fix.KCLDiff != "" {
				t.Errorf("fix: %+v", f.Fix)
			}
		}},
		{"telemetry: exporter covers the node", "accel-telemetry-missing", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.DaemonSets = []appsv1.DaemonSet{daemonSet("monitoring", "dcgm-exporter", "nvcr.io/nvidia/k8s/dcgm-exporter:4.2.3", true)}
		}), 0, nil},
		{"telemetry: partial coverage extends theirs", "accel-telemetry-missing", snap(func(s *mldiscover.Snapshot) {
			tainted := gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd)
			tainted.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}}
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd), tainted}
			s.DaemonSets = []appsv1.DaemonSet{daemonSet("monitoring", "dcgm-exporter", "nvcr.io/nvidia/k8s/dcgm-exporter:4.2.3", false)}
		}), 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixManual || !strings.Contains(f.Fix.Summary, "Extend") || !strings.Contains(strings.Join(f.Evidence, "\n"), "Node/g2") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"telemetry: TPU is Cloud Monitoring's", "accel-telemetry-missing", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("t1", mldiscover.ResourceTPU, "4", npd)}
		}), 0, nil},
		{"telemetry: AMD is manual", "accel-telemetry-missing", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("a1", mldiscover.ResourceAMDGPU, "8", npd)}
		}), 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixManual || f.Vendor != mldiscover.VendorAMD || f.Severity != SeverityMedium {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"duplicate exporters on one node", "accel-exporter-duplicate", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "1", npd)}
			s.DaemonSets = []appsv1.DaemonSet{
				daemonSet("monitoring", "dcgm-exporter", "nvcr.io/nvidia/k8s/dcgm-exporter:4.2.3", true),
				daemonSet(mldiscover.NamespaceGKEManagedSystem, "dcgm-exporter", "gke.gcr.io/gke-managed-dcgm-exporter:3.3.5", true),
			}
		}), 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixManual || !strings.Contains(f.Fix.Summary, "Keep DaemonSet/gke-managed-system/dcgm-exporter") || !strings.Contains(f.Fix.Summary, "remove DaemonSet/monitoring/dcgm-exporter") {
				t.Errorf("fix: %s", f.Fix.Summary)
			}
		}},
		{"operator exporter beside its own host engine is one install", "accel-exporter-duplicate", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "1", npd)}
			ex := daemonSet("gpu-operator", "nvidia-dcgm-exporter", "nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9", true)
			he := daemonSet("gpu-operator", "nvidia-dcgm", "nvcr.io/nvidia/cloud-native/dcgm:3.3.9", true)
			s.DaemonSets = []appsv1.DaemonSet{ex, he}
		}), 0, nil},
		{"bad accelerator", "bad-accelerator", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withAllocatable(rl(mldiscover.ResourceNVIDIAGPU, "7")))}
		}), 1, func(t *testing.T, f Finding) {
			if !strings.Contains(f.Fix.Summary, "kubectl cordon g1") {
				t.Errorf("fix: %s", f.Fix.Summary)
			}
		}},
		{"healthy accelerators", "bad-accelerator", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 0, nil},
		{"mig failed", "mig-config-failed", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelMIGConfigState, "failed"))}
		}), 1, nil},
		{"mig ok", "mig-config-failed", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelMIGConfigState, "success"))}
		}), 0, nil},
		{"driver skew", "driver-skew", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{
				gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p", mldiscover.LabelCUDADriverMajor, "550")),
				gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p", mldiscover.LabelCUDADriverMajor, "535")),
			}
		}), 1, nil},
		{"one driver per pool", "driver-skew", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{
				gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "a", mldiscover.LabelCUDADriverMajor, "550")),
				gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "b", mldiscover.LabelCUDADriverMajor, "535")),
			}
		}), 0, nil},
		{"no node health on EKS", "no-node-health", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", withLabels(mldiscover.LabelEKSNodeGroup, "p"))}
		}), 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixKCL || !strings.HasPrefix(f.Fix.Summary, "First option: the EKS node monitoring agent") || !strings.Contains(f.Fix.KCLDiff, "+    node_health = forge.Install {") {
				t.Errorf("fix: %+v", f.Fix)
			}
		}},
		{"NPD conditions are node health", "no-node-health", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 0, nil},
		{"EKS agent conditions are node health", "no-node-health", snap(func(s *mldiscover.Snapshot) {
			n := gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8")
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: "AcceleratedHardwareReady", Status: corev1.ConditionTrue})
			s.Nodes = []corev1.Node{n}
		}), 0, nil},
		{"jobs bypass the queue", "gpu-jobs-bypass-queue", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.Jobs = []batchv1.Job{gpuJob("ml", "train", nil, false), gpuJob("ml", "old", nil, true)}
		}), 1, func(t *testing.T, f Finding) {
			if len(f.Evidence) != 1 || !strings.Contains(f.Fix.KCLDiff, "+    queue = forge.Install {") || !strings.Contains(f.Fix.KCLDiff, `+            kind = "jobset"`) {
				t.Errorf("finding: %+v\n%s", f.Evidence, f.Fix.KCLDiff)
			}
		}},
		{"jobs queued through Kueue", "gpu-jobs-bypass-queue", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.CRDs = []string{"clusterqueues.kueue.x-k8s.io"}
			s.Deployments = []appsv1.Deployment{deployment("kueue-system", "kueue-controller-manager", "registry.k8s.io/kueue/kueue:v0.20.7", nil)}
			s.Jobs = []batchv1.Job{gpuJob("ml", "train", map[string]string{labelKueueQueue: "q"}, false)}
		}), 0, nil},
		{"job not queued", "job-not-queued", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.CRDs = []string{"clusterqueues.kueue.x-k8s.io"}
			s.Deployments = []appsv1.Deployment{deployment("kueue-system", "kueue-controller-manager", "registry.k8s.io/kueue/kueue:v0.20.7", nil)}
			s.Jobs = []batchv1.Job{gpuJob("ml", "a", nil, false), gpuJob("ml", "b", map[string]string{labelKueueQueue: "q"}, false)}
		}), 1, func(t *testing.T, f Finding) {
			if len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0], "Job/ml/a") {
				t.Errorf("evidence: %v", f.Evidence)
			}
		}},
		{"spot on EKS with no handler", "spot-no-termination-handling", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p", "eks.amazonaws.com/capacityType", "SPOT"))}
		}), 1, nil},
		{"spot on EKS with NTH", "spot-no-termination-handling", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p", "eks.amazonaws.com/capacityType", "SPOT"))}
			s.DaemonSets = []appsv1.DaemonSet{daemonSet("kube-system", "aws-node-termination-handler", "public.ecr.aws/aws-ec2/aws-node-termination-handler:v1.24.0", true)}
		}), 0, nil},
		{"spot with Karpenter's interruption queue", "spot-no-termination-handling", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p", "karpenter.sh/capacity-type", "spot"))}
			d := deployment("karpenter", "karpenter", "public.ecr.aws/karpenter/controller:1.1.0", nil)
			d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "INTERRUPTION_QUEUE"}}
			s.Deployments = []appsv1.Deployment{d}
		}), 0, nil},
		{"spot on GKE is handled by the cloud", "spot-no-termination-handling", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, gke, withLabels("cloud.google.com/gke-spot", "true"))}
		}), 0, nil},
		{"shm too small", "shm-too-small", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))}
		}), 1, nil},
		{"shm mounted", "shm-too-small", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			c := ctr("srv", "srv:1", "1")
			c.VolumeMounts = []corev1.VolumeMount{{Name: "shm", MountPath: "/dev/shm"}}
			p := accelPod("ml", "srv", "srv-1", "g1", c)
			p.Spec.Volumes = []corev1.Volume{{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
			s.Pods = []corev1.Pod{p}
		}), 0, nil},
		{"vLLM with no autoscaler", "llm-scales-on-cpu", snap(func(s *mldiscover.Snapshot) {
			s.Deployments = []appsv1.Deployment{deployment("llm", "chat", "vllm/vllm-openai:v0.7.3", nil)}
		}), 1, nil},
		{"vLLM on a CPU HPA", "llm-scales-on-cpu", snap(func(s *mldiscover.Snapshot) {
			s.Deployments = []appsv1.Deployment{deployment("llm", "chat", "vllm/vllm-openai:v0.7.3", nil)}
			s.HPAs = []autoscalingv2.HorizontalPodAutoscaler{hpa("llm", "chat", autoscalingv2.MetricSpec{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU}})}
		}), 1, func(t *testing.T, f Finding) {
			if !strings.Contains(f.Evidence[0], "scales on cpu") {
				t.Errorf("evidence: %v", f.Evidence)
			}
		}},
		{"vLLM on queue depth", "llm-scales-on-cpu", snap(func(s *mldiscover.Snapshot) {
			s.Deployments = []appsv1.Deployment{deployment("llm", "chat", "vllm/vllm-openai:v0.7.3", nil)}
			s.HPAs = []autoscalingv2.HorizontalPodAutoscaler{hpa("llm", "chat", autoscalingv2.MetricSpec{Type: autoscalingv2.ExternalMetricSourceType,
				External: &autoscalingv2.ExternalMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: "s0-prometheus"}}})}
		}), 0, nil},
		{"model downloaded per pod", "model-downloaded-per-pod", snap(func(s *mldiscover.Snapshot) {
			p := accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))
			p.Spec.InitContainers = []corev1.Container{{Name: "fetch", Image: "amazon/aws-cli", Args: []string{"s3", "cp", "--recursive", "s3://models/llama", "/models"}}}
			s.Pods = []corev1.Pod{p}
		}), 1, nil},
		{"model in the image", "model-downloaded-per-pod", snap(func(s *mldiscover.Snapshot) {
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1", "--model", "/models/llama"))}
		}), 0, nil},
		{"HF download without the fast path", "hf-transfer-off", snap(func(s *mldiscover.Snapshot) {
			s.Pods = []corev1.Pod{accelPod("ml", "chat", "chat-1", "g1", ctr("vllm", "vllm/vllm-openai:v0.7.3", "1", "serve", "Qwen/Qwen2.5-7B-Instruct"))}
		}), 1, nil},
		{"HF download with HF_XET_HIGH_PERFORMANCE (value blanked by the collector)", "hf-transfer-off", snap(func(s *mldiscover.Snapshot) {
			c := ctr("vllm", "vllm/vllm-openai:v0.7.3", "1", "serve", "Qwen/Qwen2.5-7B-Instruct")
			c.Env = []corev1.EnvVar{{Name: envXetHighPerf}}
			s.Pods = []corev1.Pod{accelPod("ml", "chat", "chat-1", "g1", c)}
		}), 0, nil},
		{"weights in image", "weights-in-image", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withImage("docker.io/library/srv:1", 11_000_000_000))}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))}
		}), 1, func(t *testing.T, f Finding) {
			if !strings.Contains(f.Evidence[0], "11.0 GB") {
				t.Errorf("evidence: %v", f.Evidence)
			}
		}},
		{"slim image", "weights-in-image", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withImage("docker.io/library/srv:1", 9_000_000_000))}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))}
		}), 0, nil},
		{"fan-out without a pull cache", "no-p2p-cache", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd), gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			for i, n := range []string{"g1", "g1", "g2", "g2"} {
				s.Pods = append(s.Pods, accelPod("ml", "srv", "srv-"+string(rune('a'+i)), n, ctr("srv", "srv:1", "1")))
			}
		}), 1, func(t *testing.T, f Finding) {
			if !strings.Contains(f.Fix.KCLDiff, "+    pull_cache = forge.Install {") {
				t.Errorf("diff: %s", f.Fix.KCLDiff)
			}
		}},
		{"fan-out with Spegel", "no-p2p-cache", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd), gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.DaemonSets = []appsv1.DaemonSet{daemonSet("spegel", "spegel", "ghcr.io/spegel-org/spegel:v0.8.2", true)}
			for i, n := range []string{"g1", "g1", "g2", "g2"} {
				s.Pods = append(s.Pods, accelPod("ml", "srv", "srv-"+string(rune('a'+i)), n, ctr("srv", "srv:1", "1")))
			}
		}), 0, nil},
		{"four pods on one node", "no-p2p-cache", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			for i := 0; i < 4; i++ {
				s.Pods = append(s.Pods, accelPod("ml", "srv", "srv-"+string(rune('a'+i)), "g1", ctr("srv", "srv:1", "1")))
			}
		}), 0, nil},
		{"image streaming off", "image-streaming-off", snap(func(s *mldiscover.Snapshot) {
			s.Context = "gke_acme_us-central1_gpu"
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, gke)}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "us-docker.pkg.dev/acme/r/srv:1", "1"))}
		}), 1, func(t *testing.T, f Finding) {
			want := "gcloud container node-pools update gpu --cluster=gpu --location=us-central1 --project=acme --enable-image-streaming"
			if f.Fix.Kind != FixCommand || f.Fix.Command != want {
				t.Errorf("command: %q", f.Fix.Command)
			}
		}},
		{"image streaming on", "image-streaming-off", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, gke, withLabels(mldiscover.LabelGKEImageStreaming, "true"))}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "us-docker.pkg.dev/acme/r/srv:1", "1"))}
		}), 0, nil},
		{"image streaming: not an Artifact Registry image", "image-streaming-off", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, gke)}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "ghcr.io/acme/srv:1", "1"))}
		}), 0, nil},
		{"unallocated", "accel-unallocated", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 1, func(t *testing.T, f Finding) {
			if !strings.Contains(f.Title, "at snapshot time") {
				t.Errorf("title: %s", f.Title)
			}
		}},
		{"allocated", "accel-unallocated", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))}
		}), 0, nil},
		{"image not warm", "image-not-warm", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{
				gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p"), withImage("docker.io/library/srv:1", 1)),
				gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p")),
			}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))}
		}), 1, nil},
		{"image listed nowhere makes no claim", "image-not-warm", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{
				gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p")),
				gpuNode("g2", mldiscover.ResourceNVIDIAGPU, "8", npd, withLabels(mldiscover.LabelEKSNodeGroup, "p")),
			}
			s.Pods = []corev1.Pod{accelPod("ml", "srv", "srv-1", "g1", ctr("srv", "srv:1", "1"))}
		}), 0, nil},
		{"image volumes: old server", "image-volumes-unavailable", snap(func(s *mldiscover.Snapshot) {
			s.ServerVersion = "v1.34.2"
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 1, nil},
		{"image volumes: old containerd", "image-volumes-unavailable", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withRuntime("containerd://2.0.6"))}
		}), 1, func(t *testing.T, f Finding) {
			if f.Confidence != ConfidenceLikely {
				t.Errorf("confidence: %s", f.Confidence)
			}
		}},
		{"image volumes available", "image-volumes-unavailable", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 0, nil},
		{"kubelet tracing on a self-managed cluster", "kubelet-tracing-off", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		}), 1, func(t *testing.T, f Finding) {
			if f.Confidence != ConfidenceUnverified || !strings.Contains(f.Title, "may be off") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"kubelet tracing on GKE is not ours to judge", "kubelet-tracing-off", snap(func(s *mldiscover.Snapshot) {
			s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, gke)}
		}), 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := byID(eval(c.s), c.id)
			if len(got) != c.want {
				t.Fatalf("%s: got %d findings, want %d: %+v", c.id, len(got), c.want, got)
			}
			if c.check != nil && len(got) > 0 {
				c.check(t, got[0])
			}
		})
	}
}

func hpa(ns, target string, metrics ...autoscalingv2.MetricSpec) autoscalingv2.HorizontalPodAutoscaler {
	return autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: target, Namespace: ns},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: target},
			Metrics:        metrics,
		},
	}
}

func TestDecisions(t *testing.T) {
	s := snap(func(s *mldiscover.Snapshot) {
		s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd, withAllocatable(rl(mldiscover.ResourceNVIDIAGPU, "7")))}
	})
	cases := []struct {
		name      string
		decision  Decision
		dismissed bool
	}{
		{"unexpired", Decision{ID: "kubelet-tracing-off", Decision: "dismiss", Reason: "r", Expires: "2027-01-01"}, true},
		{"expires today still applies", Decision{ID: "kubelet-tracing-off", Reason: "r", Expires: "2026-11-25"}, true},
		{"expired yesterday", Decision{ID: "kubelet-tracing-off", Reason: "r", Expires: "2026-11-24"}, false},
		{"malformed date never applies", Decision{ID: "kubelet-tracing-off", Reason: "r", Expires: "soon"}, false},
		{"other id", Decision{ID: "driver-skew", Reason: "r", Expires: "2027-01-01"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := eval(s, c.decision)
			f := byID(fs, "kubelet-tracing-off")
			if len(f) != 1 {
				t.Fatalf("findings: %+v", fs)
			}
			if (f[0].Dismissed != nil) != c.dismissed {
				t.Fatalf("dismissed = %v, want %v", f[0].Dismissed, c.dismissed)
			}
			if c.dismissed && fs[len(fs)-1].ID != "kubelet-tracing-off" {
				t.Errorf("a dismissed finding sorts last: %v", fs[len(fs)-1].ID)
			}
			if !c.dismissed && fs[0].Severity != SeverityHigh {
				t.Errorf("findings sort by severity: first is %s", fs[0].Severity)
			}
		})
	}
	// Options.Now overrides the snapshot's clock.
	fs := Evaluate(s, mldiscover.Discover(s), Options{
		Decisions: []Decision{{ID: "kubelet-tracing-off", Reason: "r", Expires: "2026-12-01"}},
		Now:       time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC),
	})
	if f := byID(fs, "kubelet-tracing-off"); len(f) != 1 || f[0].Dismissed != nil {
		t.Errorf("Now past expiry must not dismiss: %+v", f)
	}
}

func TestDismissedSuggestionIsNotProposed(t *testing.T) {
	s := snap(func(s *mldiscover.Snapshot) {
		s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", withLabels(mldiscover.LabelEKSNodeGroup, "p"))}
	})
	inv := mldiscover.Discover(s)
	d := Decision{ID: "no-node-health", Reason: "auto repair is on", Expires: "2027-01-01"}
	fs := Evaluate(s, inv, Options{Decisions: []Decision{d}})
	f := byID(fs, "no-node-health")
	if len(f) != 1 || f[0].Dismissed == nil || f[0].Fix.KCLDiff != "" {
		t.Fatalf("finding: %+v", f)
	}
	mlk := Propose(inv, fs, ProposeOptions{Decisions: []Decision{d}})
	if strings.Contains(mlk, "node-problem-detector") {
		t.Errorf("a dismissed finding's install is proposed:\n%s", mlk)
	}
	if !strings.Contains(mlk, `id = "no-node-health"`) {
		t.Errorf("the decision is not written back:\n%s", mlk)
	}
}

func TestProposeIsIdempotent(t *testing.T) {
	for _, name := range fixtures {
		s := loadSnapshot(t, name)
		inv := mldiscover.Discover(s)
		fs := Evaluate(s, inv, Options{})
		if a, b := Propose(inv, fs, ProposeOptions{}), Propose(inv, fs, ProposeOptions{}); a != b {
			t.Fatalf("%s: two Propose calls differ", name)
		}
	}
}

func TestProposeKeepsForgeInstalls(t *testing.T) {
	inv := mldiscover.Discover(loadSnapshot(t, "mixed-vendors"))
	mlk := Propose(inv, nil, ProposeOptions{})
	for _, want := range []string{
		"queue = forge.Install {",
		`version = "0.20.7"`,
		"pull_cache = forge.Install {",
		`namespace = "forge-ml"`,
	} {
		if !strings.Contains(mlk, want) {
			t.Errorf("missing %q in:\n%s", want, mlk)
		}
	}
	// What forge installed is not "observed" (the KCL would refuse its own
	// Install), and forge's own node agent is not a component.
	obs := mlk[strings.Index(mlk, "observed = ["):strings.Index(mlk, "observed_at")]
	for _, kind := range []string{`"kueue"`, `"spegel"`, `"otel-collector"`} {
		if strings.Contains(obs, kind) {
			t.Errorf("%s in observed:\n%s", kind, obs)
		}
	}
}

func TestRulesCatalogue(t *testing.T) {
	idRE := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	seen := map[string]bool{}
	for _, r := range Rules() {
		if !idRE.MatchString(r.ID) || seen[r.ID] {
			t.Errorf("rule id %q is malformed or repeated", r.ID)
		}
		seen[r.ID] = true
		if _, ok := severityRank[r.Severity]; !ok || r.Detects == "" || r.Fix == "" {
			t.Errorf("rule %s: incomplete %+v", r.ID, r)
		}
	}
	// Every golden finding names a catalogued rule.
	for _, name := range fixtures {
		r := run(t, loadSnapshot(t, name), fixtureDecisions(name))
		for _, f := range r.findings {
			if !seen[f.ID] {
				t.Errorf("%s: finding %s is not in Rules()", name, f.ID)
			}
		}
	}
}

func TestChartsArePinned(t *testing.T) {
	for _, c := range Charts() {
		if c.Version == "" || c.Name == "" || c.Docs == "" || (c.Repo == "") == (c.OCI == "") || (c.Repo != "" && c.Chart == "") {
			t.Errorf("chart %s is incomplete: %+v", c.Kind, c)
		}
		if c.Delivery != DeliveryBundle && c.Delivery != DeliveryBootstrap {
			t.Errorf("chart %s: delivery %q", c.Kind, c.Delivery)
		}
		if c.Kind == mldiscover.KindKubeRay {
			t.Errorf("KubeRay is adopt-only by policy; no fix installs it")
		}
	}
	for _, kind := range []string{mldiscover.KindDCGMExporter, mldiscover.KindNodeProblemDetector, mldiscover.KindKueue, mldiscover.KindJobSet, mldiscover.KindSpegel} {
		if _, ok := ChartFor(kind); !ok {
			t.Errorf("no chart for %s, which a fix installs", kind)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	cases := map[string]string{
		"nginx":                       "docker.io/library/nginx:latest",
		"vllm/vllm-openai:v0.7.3":     "docker.io/vllm/vllm-openai:v0.7.3",
		"ghcr.io/acme/x:1":            "ghcr.io/acme/x:1",
		"localhost:5000/x":            "localhost:5000/x:latest",
		"gke.gcr.io/x@sha256:abc":     "gke.gcr.io/x@sha256:abc",
		"us-docker.pkg.dev/a/b/c:tag": "us-docker.pkg.dev/a/b/c:tag",
	}
	for in, want := range cases {
		if got := normalizeImage(in); got != want {
			t.Errorf("normalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
}
