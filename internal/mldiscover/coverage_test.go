package mldiscover

import (
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseDCGMCounters(t *testing.T) {
	csv := `# Format
# DCGM FIELD, Prometheus metric type, help message
DCGM_FI_DEV_GPU_UTIL,      gauge, GPU utilization (in %).
  DCGM_FI_PROF_SM_ACTIVE , gauge, x
# DCGM_FI_DEV_XID_ERRORS, gauge, commented out
DCGM_FI_DEV_DEC_UTIL ,     gauge, Decoder utilization (in %).
REDACTED
not,a,field

DCGM_FI_DEV_GPU_UTIL, gauge, duplicate
`
	want := []string{"DCGM_FI_DEV_DEC_UTIL", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_PROF_SM_ACTIVE"}
	if got := ParseDCGMCounters(csv); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := ParseDCGMCounters(""); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
}

func TestParseDCGMExporterConfig(t *testing.T) {
	cfg, ok := ParseDCGMExporterConfig(`version: 2
metrics:
  - name: default
    file: /etc/dcgm-exporter/chart/metrics/default-counters.csv
    every: 30s
    enrichment: {kubernetes: auto}
    fields:
      - {name: DCGM_FI_DEV_GPU_TEMP_CELSIUS, prometheus_type: gauge, help: x}
      - {name: REDACTED}
`)
	if !ok || !reflect.DeepEqual(cfg.Fields, []string{"DCGM_FI_DEV_GPU_TEMP_CELSIUS"}) || cfg.Kubernetes != "auto" ||
		!reflect.DeepEqual(cfg.Files, []string{"/etc/dcgm-exporter/chart/metrics/default-counters.csv"}) {
		t.Errorf("got %+v, %v", cfg, ok)
	}
	if _, ok := ParseDCGMExporterConfig("receivers: {otlp: {}}"); ok {
		t.Error("a collector config is not a dcgm-exporter config")
	}
}

func TestDCGMDefaultFields(t *testing.T) {
	cases := []struct {
		tag, file, wantTag string
		has, lacks         string
		ok                 bool
	}{
		{"3.3.9-3.6.1-ubuntu22.04", DCGMDefaultCounters, "3.3.9-3.6.1", "DCGM_FI_PROF_PCIE_TX_BYTES", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", true},
		{"3.3.9-3.6.1-ubuntu22.04", DCGMDCPIncluded, "3.3.9-3.6.1", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_SM_ACTIVE", true},
		{"3.3.5-3.4.0-ubuntu22.04", DCGMDefaultCounters, "3.3.5-3.4.0", "DCGM_FI_DEV_PCIE_TX_THROUGHPUT", "DCGM_FI_PROF_PCIE_TX_BYTES", true},
		{"3.3.7-3.5.0-ubuntu22.04", DCGMDefaultCounters, "3.3.5-3.4.0", "DCGM_FI_DEV_XID_ERRORS", "", true},
		{"4.2.3-4.1.3-ubuntu22.04", DCGMDefaultCounters, "4.2.3-4.1.3", "DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_DEV_FB_RESERVED", true},
		{"4.8.4", DCGMDefaultCounters, "4.8.4", "DCGM_FI_DEV_FB_RESERVED", "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", true},
		{"4.6.0-4.8.3-distroless", DCGMDefaultCounters, "4.6.0-4.8.3", "DCGM_FI_DEV_FB_RESERVED", "", true},
		{"4.9.0", DCGMDCPIncluded, "4.9.0", "DCGM_FI_DEV_GPU_UTIL_RATIO", "DCGM_FI_DEV_GPU_UTIL", true},
		{"5.0.0", DCGMDefaultCounters, "4.9.0", "DCGM_FI_DEV_XID_ERROR", "", true},
		{"2.4.7-2.6.11", DCGMDefaultCounters, "", "", "", false},
		{"latest", DCGMDefaultCounters, "", "", "", false},
	}
	for _, c := range cases {
		fields, tag, ok := DCGMDefaultFields(c.tag, c.file)
		if ok != c.ok || tag != c.wantTag {
			t.Errorf("%s %s: tag %q ok %v, want %q %v", c.tag, c.file, tag, ok, c.wantTag, c.ok)
			continue
		}
		set := setOf(fields)
		if c.has != "" && !set[c.has] {
			t.Errorf("%s %s: lacks %s", c.tag, c.file, c.has)
		}
		if c.lacks != "" && set[c.lacks] {
			t.Errorf("%s %s: has %s", c.tag, c.file, c.lacks)
		}
	}
}

func TestDCGMPartsAcrossRenames(t *testing.T) {
	classic := []string{"DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_PROF_SM_ACTIVE", "DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL"}
	modern := []string{"DCGM_FI_DEV_XID_ERROR", "DCGM_FI_PROF_SM_UTIL_RATIO", "DCGM_FI_DEV_NVLINK_CRC_ERROR_TOTAL"}
	for _, fields := range [][]string{classic, modern} {
		if got := DCGMPartsPresent(SignalAccelXid, fields); !reflect.DeepEqual(got, []string{"XID_ERRORS"}) {
			t.Errorf("xid %v: %v", fields, got)
		}
		if got := DCGMPartsPresent(SignalAccelProfiling, fields); !reflect.DeepEqual(got, []string{"PROF_SM_ACTIVE"}) {
			t.Errorf("profiling %v: %v", fields, got)
		}
		if got := DCGMPartsPresent(SignalAccelNVLink, fields); !reflect.DeepEqual(got, []string{"NVLINK_ERRORS"}) {
			t.Errorf("nvlink %v: %v", fields, got)
		}
	}
	if UsesModernDCGMNames(classic) || !UsesModernDCGMNames(modern) {
		t.Error("UsesModernDCGMNames")
	}
	for sig := range DCGMSignalParts {
		if SignalCategory(sig) != CoverageCategoryAccelerator {
			t.Errorf("%s is not an accelerator signal", sig)
		}
	}
}

func TestParseNPDMonitor(t *testing.T) {
	gpu := `{"plugin":"kmsg","source":"gpu","conditions":[{"type":"GPUProblem","reason":"Ok"}],
	  "rules":[{"type":"permanent","condition":"GPUProblem","reason":"NVIDIAXid","pattern":"NVRM: Xid [(][^)]*[)]: (48|79),.*"}]}`
	m, ok := ParseNPDMonitor("/custom-config/gpu-xid.json", gpu)
	if !ok || m.Name != "gpu-xid" || !m.GPURule || !reflect.DeepEqual(m.Conditions, []string{"GPUProblem"}) || !reflect.DeepEqual(m.Reasons, []string{"NVIDIAXid"}) {
		t.Errorf("gpu monitor: %+v %v", m, ok)
	}
	kernel := `{"plugin":"kmsg","source":"kernel-monitor","conditions":[{"type":"KernelDeadlock"}],
	  "rules":[{"type":"temporary","reason":"OOMKilling","pattern":"Killed process .*"},{"type":"permanent","condition":"KernelDeadlock","reason":"DockerHung","pattern":"task docker:\\w+ blocked"}]}`
	m, ok = ParseNPDMonitor("kernel-monitor.json", kernel)
	if !ok || m.GPURule || !reflect.DeepEqual(m.Reasons, []string{"DockerHung", "OOMKilling"}) {
		t.Errorf("kernel monitor: %+v", m)
	}
	plugin := `{"plugin":"custom","source":"gpu-plugin","rules":[{"type":"permanent","condition":"GPUUnhealthy","reason":"SmiFailed","path":"/scripts/check_gpu.sh","args":["nvidia-smi"]}]}`
	if m, ok = ParseNPDMonitor("gpu-plugin.json", plugin); !ok || !m.GPURule {
		t.Errorf("custom plugin: %+v", m)
	}
	for _, bad := range []string{"", "REDACTED", "[]", `{"receivers":{}}`} {
		if _, ok := ParseNPDMonitor("x.json", bad); ok {
			t.Errorf("%q parsed as a monitor", bad)
		}
	}
}

func TestNPDArgs(t *testing.T) {
	args := []string{"/bin/sh", "-c", "exec /node-problem-detector --logtostderr --config.system-log-monitor=/config/kernel-monitor.json,/custom/gpu.json --config.custom-plugin-monitor /config/plugin.json --prometheus-address=0.0.0.0 --prometheus-port=20258"}
	want := []NPDMonitorRef{{NPDSystemLogMonitor, "/config/kernel-monitor.json"}, {NPDSystemLogMonitor, "/custom/gpu.json"}, {NPDCustomPluginMonitor, "/config/plugin.json"}}
	if got := NPDMonitorArgs(args); !reflect.DeepEqual(got, want) {
		t.Errorf("monitors: %v", got)
	}
	if got := NPDMonitorArgs([]string{"-system-log-monitors=/a.json"}); !reflect.DeepEqual(got, []NPDMonitorRef{{NPDSystemLogMonitor, "/a.json"}}) {
		t.Errorf("deprecated flag: %v", got)
	}
	if port, reach := NPDPrometheusEndpoint(args); port != 20258 || !reach {
		t.Errorf("endpoint %d %v", port, reach)
	}
	if port, reach := NPDPrometheusEndpoint(nil); port != NPDDefaultPrometheusPort || reach {
		t.Errorf("default endpoint %d %v (loopback by default)", port, reach)
	}
	if port, _ := NPDPrometheusEndpoint([]string{"--prometheus-port", "0"}); port != 0 {
		t.Errorf("disabled: %d", port)
	}
}

func TestParseCollectorConfig(t *testing.T) {
	cfg, ok := ParseCollectorConfig(`receivers:
  otlp/grpc: {protocols: {grpc: {}}}
  k8sobjects:
    objects: [{name: events, mode: watch}]
  filelog: {include: [/var/log/pods/*/*/*.log]}
  hostmetrics: {}
processors: {batch: {}, memory_limiter: {}}
exporters:
  otlphttp: {headers: {authorization: REDACTED}}
service:
  pipelines:
    traces: {receivers: [otlp/grpc], processors: [batch], exporters: [otlphttp]}
    logs/events: {receivers: [k8sobjects, filelog], exporters: [otlphttp]}
`)
	want := CollectorConfig{Receivers: []string{"filelog", "k8sobjects", "otlp"}, Processors: []string{"batch"}, Exporters: []string{"otlphttp"},
		Pipelines: []string{"logs", "traces"}, Settings: []string{settingFilelogPods, settingK8sObjectsEvents}}
	if !ok || !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
	// The v1beta1 CR carries config as an object; v1alpha1 as a string.
	obj := map[string]any{"receivers": map[string]any{"k8s_events": map[string]any{}}, "service": map[string]any{"pipelines": map[string]any{"logs": map[string]any{"receivers": []any{"k8s_events"}}}}}
	if cc, ok := CollectorConfigFromObject(obj); !ok || !reflect.DeepEqual(cc.Receivers, []string{"k8s_events"}) {
		t.Errorf("object: %+v", cc)
	}
	if cc, ok := CollectorConfigFromObject("receivers:\n  prometheus: {}\n"); !ok || !reflect.DeepEqual(cc.Receivers, []string{"prometheus"}) {
		t.Errorf("string: %+v", cc)
	}
	if _, ok := ParseCollectorConfig("DCGM_FI_DEV_GPU_UTIL, gauge, x"); ok {
		t.Error("a CSV is not a collector config")
	}
}

func TestParseAlloyConfig(t *testing.T) {
	cfg, ok := ParseAlloyConfig(`pyroscope.ebpf "default" {
  forward_to = [pyroscope.write.backend.receiver]
}
pyroscope.write "backend" { endpoint { url = "x" } }
otelcol.receiver.otlp "in" { grpc {} }
otelcol.processor.batch "b" {}
discovery.kubernetes "pods" { role = "pod" }
`)
	if !ok || !reflect.DeepEqual(cfg.Receivers, []string{"otelcol.receiver.otlp", "pyroscope.ebpf"}) ||
		!reflect.DeepEqual(cfg.Exporters, []string{"pyroscope.write"}) || !reflect.DeepEqual(cfg.Processors, []string{"otelcol.processor.batch"}) {
		t.Errorf("got %+v", cfg)
	}
	prov := collectorProvides(&ComponentConfig{Receivers: cfg.Receivers})
	if !prov[SignalContinuousProfiling] || !prov[SignalOTLPReceiver] {
		t.Errorf("provides %v", prov)
	}
	if _, ok := ParseAlloyConfig("logging { level = \"info\" }"); ok {
		t.Error("no components")
	}
}

func TestParseKubeletTracing(t *testing.T) {
	on, ok := ParseKubeletTracing("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ntracing:\n  endpoint: c:4317\n  samplingRatePerMillion: 100\n")
	if !ok || !on.Enabled || on.Endpoint != "c:4317" || on.SamplingRatePerMillion != 100 {
		t.Errorf("on: %+v %v", on, ok)
	}
	off, ok := ParseKubeletTracing("kind: KubeletConfiguration\ncgroupDriver: systemd\n")
	if !ok || off.Enabled {
		t.Errorf("off: %+v %v", off, ok)
	}
	if _, ok := ParseKubeletTracing("kind: ClusterConfiguration\n"); ok {
		t.Error("not a KubeletConfiguration")
	}
}

func TestParseKubeadmAPIServerArgs(t *testing.T) {
	v4 := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n  - name: tracing-config-file\n    value: /etc/t.yaml\n"
	if args, ok := ParseKubeadmAPIServerArgs(v4); !ok || args[APIServerTracingFlag] != "/etc/t.yaml" {
		t.Errorf("v1beta4: %v %v", args, ok)
	}
	v3 := "apiVersion: kubeadm.k8s.io/v1beta3\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    audit-log-path: /var/log/a\n"
	if args, ok := ParseKubeadmAPIServerArgs(v3); !ok || args["audit-log-path"] != "/var/log/a" || args[APIServerTracingFlag] != "" {
		t.Errorf("v1beta3: %v %v", args, ok)
	}
	if _, ok := ParseKubeadmAPIServerArgs("kind: KubeletConfiguration\n"); ok {
		t.Error("not a ClusterConfiguration")
	}
}

func TestFlags(t *testing.T) {
	args := []string{"kube-apiserver", "--tracing-config-file=/x", "-f", "/etc/c.csv", "--kubernetes"}
	if !FlagSet(args, "tracing-config-file") || FlagSet(args, "tracing") {
		t.Error("FlagSet")
	}
	if v, ok := FlagValue(args, "f", "collectors"); !ok || v != "/etc/c.csv" {
		t.Errorf("FlagValue -f: %q %v", v, ok)
	}
	if v, ok := FlagValue(args, "kubernetes"); !ok || v != "" {
		t.Errorf("FlagValue bare: %q %v", v, ok)
	}
}

// TestNodeHealthIgnoresControllerConditions: a controller's own condition
// (fake-gpu-operator's gpu-fractioning.nvidia.com/Ready) is not node
// health, and GPU rules are never "collected" by presence alone.
func TestNodeHealthIgnoresControllerConditions(t *testing.T) {
	inv := Discover(loadSnapshot(t, "bare-fake-gpu"))
	for _, r := range inv.Coverage {
		switch r.Signal {
		case SignalNodeHealth, SignalNodeHealthGPURules:
			if r.Status != CoverageMissing {
				t.Errorf("%s: %s (%s), want missing", r.Signal, r.Status, r.DeterminedBy)
			}
		}
	}
	if inv.Has(KindNodeProblemDetector) {
		t.Error("a controller condition made a node-problem-detector component")
	}
	// A GPU-named condition alone is not a GPU rule either.
	n := node("g", nil, rl("nvidia.com/gpu", "1"), rl("nvidia.com/gpu", "1"))
	n.Status.Conditions = []corev1.NodeCondition{{Type: "GPUProblem", Status: corev1.ConditionFalse}}
	inv = Discover(&Snapshot{Nodes: []corev1.Node{n}})
	for _, r := range inv.Coverage {
		if r.Signal == SignalNodeHealthGPURules && r.Status == CoverageCollected {
			t.Errorf("gpu-rules collected by presence: %+v", r)
		}
	}
}

func TestCoverageRowsAlwaysComplete(t *testing.T) {
	for _, name := range fixtures {
		inv := Discover(loadSnapshot(t, name))
		if len(inv.Coverage) != len(Signals()) {
			t.Fatalf("%s: %d rows, want %d", name, len(inv.Coverage), len(Signals()))
		}
		for i, r := range inv.Coverage {
			if r.Signal != Signals()[i] || r.Category == "" || r.Status == "" || r.DeterminedBy == "" {
				t.Errorf("%s: row %d incomplete: %+v", name, i, r)
			}
			if r.NodesCovered > r.NodesTotal {
				t.Errorf("%s %s: %d of %d nodes", name, r.Signal, r.NodesCovered, r.NodesTotal)
			}
			if _, ok := determinedRank[r.DeterminedBy]; !ok {
				t.Errorf("%s %s: determined_by %q", name, r.Signal, r.DeterminedBy)
			}
		}
	}
	empty := Discover(&Snapshot{})
	for _, r := range empty.Coverage {
		if strings.HasPrefix(r.Signal, "accel.") && r.Status != CoverageNotApplicable {
			t.Errorf("empty cluster %s: %s", r.Signal, r.Status)
		}
	}
}

// TestDCGMPrecedence: probe beats config beats version default beats
// presence, for one exporter.
func TestDCGMPrecedence(t *testing.T) {
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "dcgm-exporter", Namespace: "m"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "exporter", Image: "nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04",
				Args:         []string{"-f", "/etc/dcgm-exporter/custom.csv"},
				VolumeMounts: []corev1.VolumeMount{{Name: "m", MountPath: "/etc/dcgm-exporter/custom.csv", SubPath: "custom.csv"}}}},
			Volumes: []corev1.Volume{{Name: "m", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}}}},
		}}},
	}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dcgm-exporter-a", Namespace: "m", OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "dcgm-exporter"}}},
		Spec: corev1.PodSpec{NodeName: "g"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	cm := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "m"}, Data: map[string]string{"custom.csv": "DCGM_FI_DEV_GPU_UTIL, gauge, x\n"}}
	probe := MetricsProbe{Kind: KindDCGMExporter, Namespace: "m", Pod: "dcgm-exporter-a", Port: 9400, Path: "/metrics", Families: []string{"DCGM_FI_DEV_FB_USED", "go_goroutines"}}
	n := node("g", nil, rl("nvidia.com/gpu", "1"), rl("nvidia.com/gpu", "1"))
	base := func() *Snapshot {
		return &Snapshot{Nodes: []corev1.Node{n}, DaemonSets: []appsv1.DaemonSet{ds}, Pods: []corev1.Pod{pod}}
	}
	cfgOf := func(s *Snapshot) *ComponentConfig {
		for _, c := range Discover(s).Components {
			if c.Kind == KindDCGMExporter {
				return c.Config
			}
		}
		return nil
	}
	s := base()
	if c := cfgOf(s); c.DeterminedBy != DeterminedByPresence {
		t.Errorf("unread custom file: %+v", c)
	}
	s.ConfigMaps = []corev1.ConfigMap{cm}
	if c := cfgOf(s); c.DeterminedBy != DeterminedByConfig || !reflect.DeepEqual(c.Fields, []string{"DCGM_FI_DEV_GPU_UTIL"}) {
		t.Errorf("config: %+v", c)
	}
	s.Probes = []MetricsProbe{probe}
	if c := cfgOf(s); c.DeterminedBy != DeterminedByProbe || !reflect.DeepEqual(c.Fields, []string{"DCGM_FI_DEV_FB_USED"}) {
		t.Errorf("probe: %+v", c)
	}
	s = base()
	s.DaemonSets[0].Spec.Template.Spec.Containers[0].Args = nil
	if c := cfgOf(s); c.DeterminedBy != DeterminedByVersionDefault || !strings.Contains(c.Source, "3.3.9-3.6.1 etc/default-counters.csv") {
		t.Errorf("version default: %+v", c)
	}
	// ConfigRefs asks for the mounted ConfigMap, never kube-root-ca.crt.
	s = base()
	s.DaemonSets[0].Spec.Template.Spec.Volumes = append(s.DaemonSets[0].Spec.Template.Spec.Volumes,
		corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}}})
	refs := ConfigRefs(s)
	var names []string
	for _, r := range refs {
		names = append(names, r.Namespace+"/"+r.Name)
	}
	if !reflect.DeepEqual(names, []string{"kube-system/kubeadm-config", "kube-system/kubelet-config", "m/cm"}) {
		t.Errorf("refs: %v", names)
	}
	if pt := ProbeTargets(s, Discover(s)); len(pt) != 1 || pt[0].Pod != "dcgm-exporter-a" || pt[0].Port != 9400 {
		t.Errorf("probe targets: %+v", pt)
	}
}
