package mlrecs

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// fixtureFinding returns the findings of one id for a recorded fixture.
func fixtureFinding(t *testing.T, name, id string) []Finding {
	t.Helper()
	return byID(run(t, loadSnapshot(t, name), nil).findings, id)
}

func TestCoverageRulesOnFixtures(t *testing.T) {
	cases := []struct {
		fixture, id string
		want        int
		check       func(t *testing.T, f Finding)
	}{
		// A GPU Operator with a trimmed counters ConfigMap: certain, and
		// the fix names that ConfigMap and the exact lines.
		{"aks-operator-custom-counters", "dcgm-profiling-off", 1, func(t *testing.T, f Finding) {
			if f.Confidence != ConfidenceCertain || f.Fix.Kind != FixManual ||
				!strings.Contains(f.Fix.Summary, "ConfigMap/gpu-operator/custom-dcgm-metrics key dcgm-metrics.csv") ||
				!strings.Contains(f.Fix.Summary, "DCGM_FI_PROF_SM_ACTIVE, gauge, The ratio of cycles an SM has at least 1 warp assigned.") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"aks-operator-custom-counters", "accel-error-counters-off", 1, func(t *testing.T, f Finding) {
			if !reflect.DeepEqual(f.Signals, []string{mldiscover.SignalAccelECC, mldiscover.SignalAccelRowRemap, mldiscover.SignalAccelXid}) ||
				!strings.Contains(f.Fix.Summary, "DCGM_FI_DEV_XID_ERRORS, gauge") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"aks-operator-custom-counters", "npd-no-gpu-rules", 1, func(t *testing.T, f Finding) {
			if f.Confidence != ConfidenceCertain || f.Fix.Kind != FixManual || !strings.Contains(f.Fix.Summary, "NVRM: Xid") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"aks-operator-custom-counters", "kubelet-tracing-off", 0, nil}, // AKS: not ours to judge
		{"aks-operator-custom-counters", "collector-no-k8s-events", 1, nil},
		// The version default (GPU Operator's dcp-metrics-included.csv at
		// 3.6.1) has some profiling, not SM activity: likely.
		{"gke-gpu-operator", "dcgm-profiling-off", 1, func(t *testing.T, f Finding) {
			if f.Confidence != ConfidenceLikely || !strings.Contains(f.Fix.Summary, "ClusterPolicy spec.dcgmExporter.config.name") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"gke-gpu-operator", "dcgm-nvlink-off", 1, nil},
		{"gke-gpu-operator", "dcgm-pod-attribution-off", 0, nil}, // the operator sets DCGM_EXPORTER_KUBERNETES=true
		// Self-managed and fully traced: no tracing or profiling findings.
		{"kubeadm-traced", "kubelet-tracing-off", 0, nil},
		{"kubeadm-traced", "apiserver-tracing-off", 0, nil},
		{"kubeadm-traced", "no-continuous-profiling", 0, nil},
		{"kubeadm-traced", "no-otlp-receiver", 0, nil},
		{"kubeadm-traced", "collector-no-k8s-events", 0, nil},
		{"kubeadm-traced", "npd-no-gpu-rules", 0, nil},
		{"kubeadm-traced", "dcgm-profiling-off", 0, nil},
		{"kubeadm-traced", "inference-tracing-off", 0, nil},
		{"kubeadm-traced", "training-tracing-off", 0, nil},
		// forge installed the exporter and NPD: the fixes are KCL.
		{"probed-forge-stack", "dcgm-profiling-off", 1, func(t *testing.T, f Finding) {
			if f.Confidence != ConfidenceCertain || f.Fix.Kind != FixKCL || !strings.Contains(f.Fix.KCLDiff, `+                                            "name": "DCGM_FI_PROF_SM_UTIL_RATIO"`) {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"probed-forge-stack", "npd-no-gpu-rules", 1, func(t *testing.T, f Finding) {
			if f.Fix.Kind != FixKCL || !strings.Contains(f.Fix.KCLDiff, "gpu-xid-monitor.json") {
				t.Errorf("finding: %+v", f)
			}
		}},
		{"probed-forge-stack", "no-continuous-profiling", 0, nil},  // Alloy pyroscope.ebpf + the OTel eBPF profiler
		{"probed-forge-stack", "dcgm-pod-attribution-off", 0, nil}, // the probe saw the pod label
		// fake-gpu-operator: nothing readable, so no field-set finding.
		{"bare-fake-gpu", "dcgm-profiling-off", 0, nil},
		{"bare-fake-gpu", "no-node-health", 1, nil},
		{"bare-fake-gpu", "npd-no-gpu-rules", 0, nil},
		// GKE managed DCGM plus a standalone exporter: GKE's set is fixed.
		{"gke-dcgm-duplicate", "accel-error-counters-off", 1, func(t *testing.T, f Finding) {
			if !strings.Contains(f.Fix.Summary, "GKE's managed DCGM metric set is fixed") {
				t.Errorf("fix: %s", f.Fix.Summary)
			}
		}},
		{"no-accelerators", "no-continuous-profiling", 0, nil},
		{"no-accelerators", "no-otlp-receiver", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.fixture+"/"+c.id, func(t *testing.T) {
			got := fixtureFinding(t, c.fixture, c.id)
			if len(got) != c.want {
				t.Fatalf("got %d findings, want %d: %+v", len(got), c.want, got)
			}
			if c.check != nil && len(got) > 0 {
				c.check(t, got[0])
			}
		})
	}
}

func TestPodAttributionOff(t *testing.T) {
	s := snap(func(s *mldiscover.Snapshot) {
		s.Nodes = []corev1.Node{gpuNode("g1", mldiscover.ResourceNVIDIAGPU, "8", npd)}
		ds := daemonSet("monitoring", "dcgm-exporter", "nvcr.io/nvidia/k8s/dcgm-exporter:4.8.4", true)
		ds.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DCGM_EXPORTER_KUBERNETES", Value: "false"}}
		s.DaemonSets = append(s.DaemonSets, ds)
	})
	f := byID(eval(s), "dcgm-pod-attribution-off")
	if len(f) != 1 || f[0].Confidence != ConfidenceLikely || !strings.Contains(f[0].Fix.Summary, "DCGM_EXPORTER_KUBERNETES=true") {
		t.Fatalf("finding: %+v", f)
	}
	s.DaemonSets[0].Spec.Template.Spec.Containers[0].Env[0].Value = "true"
	if f := byID(eval(s), "dcgm-pod-attribution-off"); len(f) != 0 {
		t.Errorf("attribution on: %+v", f)
	}
}

func TestKubeletTracingFromConfig(t *testing.T) {
	s := loadSnapshot(t, "kubeadm-traced")
	for i := range s.ConfigMaps {
		if s.ConfigMaps[i].Name == mldiscover.ConfigMapKubeletConfig {
			s.ConfigMaps[i].Data["kubelet"] = "kind: KubeletConfiguration\ncgroupDriver: systemd\n"
		}
	}
	f := byID(run(t, s, nil).findings, "kubelet-tracing-off")
	if len(f) != 1 || f[0].Confidence != ConfidenceCertain || strings.Contains(f[0].Title, "unverified") {
		t.Fatalf("finding: %+v", f)
	}
	// With the kubelet config unreadable, the finding is unverified.
	s = loadSnapshot(t, "kubeadm-traced")
	s.ConfigMaps = nil
	f = byID(run(t, s, nil).findings, "kubelet-tracing-off")
	if len(f) != 1 || f[0].Confidence != ConfidenceUnverified {
		t.Fatalf("unreadable: %+v", f)
	}
	// The API server pod still shows its flag; drop it and the finding is
	// certain from args.
	s = loadSnapshot(t, "kubeadm-traced")
	s.ConfigMaps = nil
	for i := range s.Pods {
		if strings.HasPrefix(s.Pods[i].Name, "kube-apiserver-") {
			s.Pods[i].Spec.Containers[0].Command = []string{"kube-apiserver", "--authorization-mode=Node,RBAC"}
		}
	}
	f = byID(run(t, s, nil).findings, "apiserver-tracing-off")
	if len(f) != 1 || f[0].Confidence != ConfidenceLikely {
		t.Fatalf("apiserver: %+v", f)
	}
}

func TestLinkCoverage(t *testing.T) {
	s := loadSnapshot(t, "aks-operator-custom-counters")
	inv := mldiscover.Discover(s)
	fs := Evaluate(s, inv, Options{})
	before := marshal(t, inv.Coverage)
	linked := LinkCoverage(inv.Coverage, fs)
	if string(marshal(t, inv.Coverage)) != string(before) {
		t.Fatal("LinkCoverage modified its input")
	}
	got := map[string][]string{}
	for _, r := range linked {
		if len(r.Findings) > 0 {
			got[r.Signal] = r.Findings
		}
	}
	for sig, want := range map[string][]string{
		mldiscover.SignalAccelProfiling:     {"dcgm-profiling-off"},
		mldiscover.SignalAccelXid:           {"accel-error-counters-off"},
		mldiscover.SignalNodeHealthGPURules: {"npd-no-gpu-rules"},
	} {
		if !reflect.DeepEqual(got[sig], want) {
			t.Errorf("%s: %v, want %v", sig, got[sig], want)
		}
	}
	if len(linked) != len(mldiscover.Signals()) {
		t.Errorf("rows: %d", len(linked))
	}
	// Findings whose Signals were dropped (an old JSON) fall back to the
	// rule's.
	for i := range fs {
		fs[i].Signals = nil
	}
	if r := LinkCoverage(inv.Coverage, fs); !reflect.DeepEqual(rowFindings(r, mldiscover.SignalAccelProfiling), []string{"dcgm-profiling-off"}) {
		t.Errorf("rule fallback: %v", rowFindings(r, mldiscover.SignalAccelProfiling))
	}
}

func rowFindings(rows []mldiscover.SignalCoverage, sig string) []string {
	for _, r := range rows {
		if r.Signal == sig {
			return r.Findings
		}
	}
	return nil
}

func TestRuleSignalsAreKnown(t *testing.T) {
	known := map[string]bool{}
	for _, s := range mldiscover.Signals() {
		known[s] = true
	}
	for _, r := range Rules() {
		for _, s := range r.Signals {
			if !known[s] {
				t.Errorf("rule %s names unknown signal %s", r.ID, s)
			}
		}
	}
}

func TestConfidenceFrom(t *testing.T) {
	for by, want := range map[string]string{
		mldiscover.DeterminedByProbe: ConfidenceCertain, mldiscover.DeterminedByConfig: ConfidenceCertain,
		mldiscover.DeterminedByArgs: ConfidenceLikely, mldiscover.DeterminedByVersionDefault: ConfidenceLikely,
		mldiscover.DeterminedByPresence: ConfidenceUnverified, mldiscover.DeterminedByUnreadable: ConfidenceUnverified,
	} {
		if got := confidenceFrom(by); got != want {
			t.Errorf("%s: %s, want %s", by, got, want)
		}
	}
}
