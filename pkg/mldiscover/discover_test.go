package mldiscover

import (
	"bytes"
	"encoding/json"
	"flag"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// update rewrites testdata/*.inventory.golden.json from the current output:
//
//	go test ./pkg/mldiscover -run TestGolden -update
var update = flag.Bool("update", false, "rewrite testdata/*.inventory.golden.json from Discover's output")

var fixtures = []string{
	"gke-gpu-operator",
	"gke-dcgm-duplicate",
	"eks-spot-training",
	"bare-fake-gpu",
	"mixed-vendors",
	"no-accelerators",
	"kubeadm-traced",
	"aks-operator-custom-counters",
	"probed-forge-stack",
}

func loadSnapshot(t *testing.T, name string) *Snapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if s.Schema != SnapshotSchema {
		t.Fatalf("%s: schema %q, want %q", name, s.Schema, SnapshotSchema)
	}
	return &s
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// refsDoc is what the two-phase read and the probe would ask for.
type refsDoc struct {
	ConfigRefs   []ObjectRef   `json:"config_refs"`
	ProbeTargets []ProbeTarget `json:"probe_targets"`
}

func TestGoldenRefs(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			s := loadSnapshot(t, name)
			inv := Discover(s)
			first := *s
			first.ConfigMaps, first.Probes = nil, nil
			doc := refsDoc{ConfigRefs: ConfigRefs(&first), ProbeTargets: ProbeTargets(s, inv)}
			if doc.ConfigRefs == nil {
				doc.ConfigRefs = []ObjectRef{}
			}
			if doc.ProbeTargets == nil {
				doc.ProbeTargets = []ProbeTarget{}
			}
			compareGolden(t, filepath.Join("testdata", name+".refs.golden.json"), marshal(t, doc))
		})
	}
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; run `go test ./pkg/mldiscover -run TestGolden -update` and review the diff.\n--- got ---\n%s", path, got)
	}
}

func TestGolden(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			got := marshal(t, Discover(loadSnapshot(t, name)))
			path := filepath.Join("testdata", name+".inventory.golden.json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s (run with -update to create it): %v", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs; run `go test ./pkg/mldiscover -run TestGolden -update` and review the diff.\n--- got ---\n%s", path, got)
			}
		})
	}
}

// shuffle reorders every list of a snapshot, as a different API server
// (or a different page order) would return it.
func shuffle(s *Snapshot, seed int64) {
	r := rand.New(rand.NewSource(seed))
	sh := func(n int, swap func(i, j int)) { r.Shuffle(n, swap) }
	sh(len(s.Nodes), func(i, j int) { s.Nodes[i], s.Nodes[j] = s.Nodes[j], s.Nodes[i] })
	sh(len(s.Pods), func(i, j int) { s.Pods[i], s.Pods[j] = s.Pods[j], s.Pods[i] })
	sh(len(s.DaemonSets), func(i, j int) { s.DaemonSets[i], s.DaemonSets[j] = s.DaemonSets[j], s.DaemonSets[i] })
	sh(len(s.Deployments), func(i, j int) { s.Deployments[i], s.Deployments[j] = s.Deployments[j], s.Deployments[i] })
	sh(len(s.StatefulSets), func(i, j int) { s.StatefulSets[i], s.StatefulSets[j] = s.StatefulSets[j], s.StatefulSets[i] })
	sh(len(s.Jobs), func(i, j int) { s.Jobs[i], s.Jobs[j] = s.Jobs[j], s.Jobs[i] })
	sh(len(s.HPAs), func(i, j int) { s.HPAs[i], s.HPAs[j] = s.HPAs[j], s.HPAs[i] })
	sh(len(s.CRDs), func(i, j int) { s.CRDs[i], s.CRDs[j] = s.CRDs[j], s.CRDs[i] })
	sh(len(s.APIGroups), func(i, j int) { s.APIGroups[i], s.APIGroups[j] = s.APIGroups[j], s.APIGroups[i] })
	sh(len(s.ReadErrors), func(i, j int) { s.ReadErrors[i], s.ReadErrors[j] = s.ReadErrors[j], s.ReadErrors[i] })
	sh(len(s.ConfigMaps), func(i, j int) { s.ConfigMaps[i], s.ConfigMaps[j] = s.ConfigMaps[j], s.ConfigMaps[i] })
	sh(len(s.Probes), func(i, j int) { s.Probes[i], s.Probes[j] = s.Probes[j], s.Probes[i] })
	for i := range s.Nodes {
		c := s.Nodes[i].Status.Conditions
		sh(len(c), func(a, b int) { c[a], c[b] = c[b], c[a] })
	}
}

func TestDiscoverIsDeterministic(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			want := marshal(t, Discover(loadSnapshot(t, name)))
			for seed := int64(1); seed <= 5; seed++ {
				s := loadSnapshot(t, name)
				shuffle(s, seed)
				if got := marshal(t, Discover(s)); !bytes.Equal(got, want) {
					t.Fatalf("seed %d: shuffled input changed the inventory:\n%s", seed, got)
				}
			}
		})
	}
}

func TestResourceVendor(t *testing.T) {
	cases := map[string]string{
		"nvidia.com/gpu":                VendorNVIDIA,
		"nvidia.com/gpu.shared":         VendorNVIDIA,
		"nvidia.com/mig-1g.10gb":        VendorNVIDIA,
		"amd.com/gpu":                   VendorAMD,
		"google.com/tpu":                VendorTPU,
		"aws.amazon.com/neuron":         VendorNeuron,
		"aws.amazon.com/neuroncore":     VendorNeuron,
		"aws.amazon.com/neurondevice":   VendorNeuron,
		"habana.ai/gaudi":               VendorGaudi,
		"example.com/fpga":              VendorGeneric,
		"xilinx.com/fpga-xilinx_u250":   VendorGeneric,
		"cpu":                           "",
		"memory":                        "",
		"ephemeral-storage":             "",
		"hugepages-2Mi":                 "",
		"pods":                          "",
		"kubernetes.io/batch-cpu":       "",
		"scheduling.kubernetes.io/foo":  "",
		"attachable-volumes-aws-ebs":    "",
		"rdma/rdma_shared_device_a":     "",
		"vpc.amazonaws.com/efa":         "",
		"nvidia.com/hostdev":            "",
		"devices.kubevirt.io/kvm":       "",
		"smarter-devices/fuse":          "",
		"networking.gke.io/vpc-network": "",
	}
	for name, want := range cases {
		if got := ResourceVendor(name); got != want {
			t.Errorf("ResourceVendor(%q) = %q, want %q", name, got, want)
		}
	}
}

func node(name string, labels map[string]string, capacity, allocatable corev1.ResourceList) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Capacity: capacity, Allocatable: allocatable},
	}
}

func rl(kv ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		out[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return out
}

func TestNodeAcceleratorOf(t *testing.T) {
	cases := []struct {
		name string
		node corev1.Node
		want NodeAccelerator
		ok   bool
	}{
		{"nvidia", node("n", nil, rl("nvidia.com/gpu", "8"), rl("nvidia.com/gpu", "7")),
			NodeAccelerator{VendorNVIDIA, ResourceNVIDIAGPU, []string{ResourceNVIDIAGPU}, 8, 7, DetectedByResource}, true},
		{"mig mixed", node("n", nil, rl("nvidia.com/mig-1g.10gb", "4", "nvidia.com/mig-3g.40gb", "1"), rl("nvidia.com/mig-1g.10gb", "4", "nvidia.com/mig-3g.40gb", "1")),
			NodeAccelerator{VendorNVIDIA, "nvidia.com/mig-1g.10gb", []string{"nvidia.com/mig-1g.10gb", "nvidia.com/mig-3g.40gb"}, 5, 5, DetectedByResource}, true},
		{"neuron prefers cores", node("n", nil, rl("aws.amazon.com/neuron", "16", "aws.amazon.com/neuroncore", "32"), rl("aws.amazon.com/neuron", "16", "aws.amazon.com/neuroncore", "32")),
			NodeAccelerator{VendorNeuron, ResourceNeuronCore, []string{ResourceNeuronCore}, 32, 32, DetectedByResource}, true},
		{"nvidia before generic", node("n", nil, rl("example.com/fpga", "1", "nvidia.com/gpu", "1"), rl("nvidia.com/gpu", "1")),
			NodeAccelerator{VendorNVIDIA, ResourceNVIDIAGPU, []string{ResourceNVIDIAGPU}, 1, 1, DetectedByResource}, true},
		{"label only", node("n", map[string]string{LabelGFDPresent: "true"}, rl("cpu", "4"), rl("cpu", "4")),
			NodeAccelerator{VendorNVIDIA, ResourceNVIDIAGPU, []string{ResourceNVIDIAGPU}, 0, 0, DetectedByLabel}, true},
		{"zero capacity is no device", node("n", nil, rl("nvidia.com/gpu", "0"), rl("nvidia.com/gpu", "0")), NodeAccelerator{}, false},
		{"nic only", node("n", nil, rl("rdma/hca", "1k"), rl("rdma/hca", "1k")), NodeAccelerator{}, false},
		{"cpu node", node("n", nil, rl("cpu", "4", "memory", "1Gi"), rl("cpu", "4")), NodeAccelerator{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := NodeAcceleratorOf(&c.node)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, %v; want %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestNodeSharingAndDriver(t *testing.T) {
	cases := []struct {
		labels   map[string]string
		capacity corev1.ResourceList
		sharing  string
	}{
		{map[string]string{LabelGFDSharingStrategy: "time-slicing", LabelGFDReplicas: "4"}, nil, SharingTimeSlicing},
		{map[string]string{LabelGFDReplicas: "2"}, nil, SharingTimeSlicing},
		{map[string]string{LabelGFDReplicas: "1"}, nil, ""},
		{map[string]string{LabelGFDSharingStrategy: "mps"}, nil, SharingMPS},
		{map[string]string{LabelGKEGPUSharing: "time-sharing"}, nil, SharingTimeSlicing},
		{map[string]string{LabelMIGConfig: "all-disabled"}, nil, ""},
		{map[string]string{LabelMIGConfig: "all-1g.10gb", LabelMIGConfigState: "success"}, nil, SharingMIG},
		{map[string]string{LabelMIGConfig: "all-1g.10gb", LabelMIGConfigState: "failed"}, nil, ""},
		{map[string]string{LabelGFDProduct: "NVIDIA-A100-SXM4-40GB-MIG-1g.5gb"}, nil, SharingMIG},
		{nil, rl("nvidia.com/mig-1g.10gb", "7"), SharingMIG},
	}
	for _, c := range cases {
		n := node("n", c.labels, c.capacity, nil)
		if got := NodeSharing(&n); got != c.sharing {
			t.Errorf("NodeSharing(%v, %v) = %q, want %q", c.labels, c.capacity, got, c.sharing)
		}
	}
	if got := DriverVersion(map[string]string{LabelCUDADriverMajor: "550", LabelCUDADriverMinor: "54", LabelCUDADriverRev: "15"}); got != "550.54.15" {
		t.Errorf("DriverVersion = %q", got)
	}
	if got := DriverVersion(map[string]string{LabelCUDADriverFull: "560.35.03", LabelCUDADriverMajor: "560"}); got != "560.35.03" {
		t.Errorf("DriverVersion full = %q", got)
	}
	if got := DriverVersion(map[string]string{LabelCUDADriverMajor: "535"}); got != "535" {
		t.Errorf("DriverVersion major only = %q", got)
	}
}

func TestPodAccelerators(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{Limits: rl("nvidia.com/gpu", "4")}}},
		Containers: []corev1.Container{
			{Name: "a", Resources: corev1.ResourceRequirements{Limits: rl("nvidia.com/gpu", "1", "cpu", "2")}},
			{Name: "b", Resources: corev1.ResourceRequirements{Requests: rl("nvidia.com/gpu", "2")}},
			{Name: "c", Resources: corev1.ResourceRequirements{Limits: rl("example.com/fpga", "1")}},
		},
	}
	got := PodSpecAccelerators(&spec)
	want := map[string]int64{"nvidia.com/gpu": 4, "example.com/fpga": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := PodSpecAccelerators(&corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Limits: rl("cpu", "1")}}}}); got != nil {
		t.Errorf("cpu-only pod: got %v", got)
	}
}

func TestComponentStatusAndOwner(t *testing.T) {
	s := &Snapshot{
		CRDs: []string{"clusterqueues.kueue.x-k8s.io", "jobsets.jobset.x-k8s.io"},
		Deployments: []appsv1.Deployment{
			{ObjectMeta: metav1.ObjectMeta{Name: "jobset-controller-manager", Namespace: "forge-ml", Labels: map[string]string{LabelComponentOwner: "acme.prod", LabelHelmChart: "jobset-0.13.1"}},
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "registry.k8s.io/jobset/jobset:v0.13.1"}}}}}},
		},
	}
	inv := Discover(s)
	if inv.Has(KindKueue) {
		t.Errorf("kueue with CRDs and no controller must not be present")
	}
	q := inv.Find(KindKueue)
	if len(q) != 1 || q[0].Status != StatusCRDOnly {
		t.Fatalf("kueue: %+v", q)
	}
	if len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "kueue") {
		t.Errorf("warnings: %v", inv.Warnings)
	}
	js := inv.Find(KindJobSet)
	if len(js) != 1 || js[0].Status != StatusPresent || js[0].Owner != OwnerForge || js[0].ChartVersion != "0.13.1" || js[0].Version != "v0.13.1" {
		t.Errorf("jobset: %+v", js)
	}
}

func TestChartVersion(t *testing.T) {
	cases := map[string]string{
		"dcgm-exporter-4.9.0":          "4.9.0",
		"node-problem-detector-2.3.14": "2.3.14",
		"gpu-operator-v24.9.2":         "v24.9.2",
		"kueue-0.20.7-rc.1":            "0.20.7-rc.1",
		"kube-state-metrics-5.27.0":    "5.27.0",
		"":                             "",
		"not-a-chart":                  "",
	}
	for in, want := range cases {
		if got := chartVersion(in); got != want {
			t.Errorf("chartVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageParsing(t *testing.T) {
	cases := []struct{ image, repo, tag string }{
		{"nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04", "nvcr.io/nvidia/k8s/dcgm-exporter", "3.3.9-3.6.1-ubuntu22.04"},
		{"localhost:5000/vllm", "localhost:5000/vllm", ""},
		{"localhost:5000/vllm:v1", "localhost:5000/vllm", "v1"},
		{"gke.gcr.io/x@sha256:abc", "gke.gcr.io/x", ""},
		{"Repo/Image:Tag@sha256:abc", "repo/image", "Tag"},
	}
	for _, c := range cases {
		if got := imageRepo(c.image); got != c.repo {
			t.Errorf("imageRepo(%q) = %q, want %q", c.image, got, c.repo)
		}
		if got := ImageTag(c.image); got != c.tag {
			t.Errorf("ImageTag(%q) = %q, want %q", c.image, got, c.tag)
		}
	}
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*dcgm-exporter*", "nvcr.io/nvidia/k8s/dcgm-exporter", true},
		{"*/dcgm", "nvcr.io/nvidia/cloud-native/dcgm", true},
		{"*/dcgm", "nvcr.io/nvidia/k8s/dcgm-exporter", false},
		{"*/gpu-operator", "nvcr.io/nvidia/gpu-operator", true},
		{"*/gpu-operator", "nvcr.io/nvidia/cloud-native/gpu-operator-validator", false},
		{"*dragonfly*dfdaemon*", "dragonflyoss/dfdaemon", true},
		{"exact", "exact", true},
	} {
		if got := glob(c.pattern, c.s); got != c.want {
			t.Errorf("glob(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}

func TestNodeCoverageSimulatesDaemonSets(t *testing.T) {
	gpu := node("gpu", map[string]string{"gpu": "yes"}, rl("nvidia.com/gpu", "1"), rl("nvidia.com/gpu", "1"))
	gpu.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}}
	plain := node("plain", nil, nil, nil)
	ds := func(name string, sel map[string]string, tol []corev1.Toleration) appsv1.DaemonSet {
		return appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "m"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: sel, Tolerations: tol}}}}
	}
	s := &Snapshot{Nodes: []corev1.Node{gpu, plain}, DaemonSets: []appsv1.DaemonSet{
		ds("untolerated", nil, nil),
		ds("tolerant", nil, []corev1.Toleration{{Operator: corev1.TolerationOpExists}}),
		ds("selected", map[string]string{"gpu": "yes"}, []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists}}),
	}}
	for name, want := range map[string][]string{
		"untolerated": {"plain"},
		"tolerant":    {"gpu", "plain"},
		"selected":    {"gpu"},
	} {
		got := NodeCoverage(s, Component{Namespace: "m", Workload: "DaemonSet/" + name})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	if got := NodeCoverage(s, Component{Vendor: VendorNVIDIA}); !reflect.DeepEqual(got, []string{"gpu"}) {
		t.Errorf("workload-less component: got %v", got)
	}
}
