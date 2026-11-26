package mldiscover

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The kubeadm ConfigMaps discovery reads on a self-managed cluster.
const (
	ConfigMapKubeletConfig = "kubelet-config"
	ConfigMapKubeadmConfig = "kubeadm-config"
	NamespaceKubeSystem    = "kube-system"
)

// KubeletTracing is the `tracing` block of a KubeletConfiguration.
type KubeletTracing struct {
	// Enabled is set when the block is present (the kubelet then exports
	// spans to Endpoint, default localhost:4317).
	Enabled bool
	// Endpoint is the OTLP endpoint, "" for the default.
	Endpoint string
	// SamplingRatePerMillion is the configured rate, 0 when unset.
	SamplingRatePerMillion int
}

// ParseKubeletTracing reads the tracing block of a KubeletConfiguration
// (kubeadm's kube-system/kubelet-config, key "kubelet"). ok is false when
// the text is not a KubeletConfiguration.
func ParseKubeletTracing(text string) (KubeletTracing, bool) {
	var doc struct {
		Kind    string `json:"kind"`
		Tracing *struct {
			Endpoint               string `json:"endpoint"`
			SamplingRatePerMillion *int   `json:"samplingRatePerMillion"`
		} `json:"tracing"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || doc.Kind != "KubeletConfiguration" {
		return KubeletTracing{}, false
	}
	if doc.Tracing == nil {
		return KubeletTracing{}, true
	}
	t := KubeletTracing{Enabled: true, Endpoint: doc.Tracing.Endpoint}
	if doc.Tracing.SamplingRatePerMillion != nil {
		t.SamplingRatePerMillion = *doc.Tracing.SamplingRatePerMillion
	}
	return t, true
}

// APIServerTracingFlag is the kube-apiserver flag that turns tracing on.
const APIServerTracingFlag = "tracing-config-file"

// ParseKubeadmAPIServerArgs reads apiServer.extraArgs from a kubeadm
// ClusterConfiguration (kube-system/kubeadm-config, key
// "ClusterConfiguration"): a map in v1beta3, a list of {name, value} in
// v1beta4. ok is false when the text is not a ClusterConfiguration.
func ParseKubeadmAPIServerArgs(text string) (map[string]string, bool) {
	var doc struct {
		Kind      string `json:"kind"`
		APIServer struct {
			ExtraArgs any `json:"extraArgs"`
		} `json:"apiServer"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || doc.Kind != "ClusterConfiguration" {
		return nil, false
	}
	out := map[string]string{}
	switch t := doc.APIServer.ExtraArgs.(type) {
	case map[string]any:
		for k, v := range t {
			out[k] = toString(v)
		}
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				if name, ok := m["name"].(string); ok {
					out[name] = toString(m["value"])
				}
			}
		}
	}
	return out, true
}

// FlagSet reports whether a container's command and args set a flag
// (`--name=value`, `--name value`, or a single dash).
func FlagSet(args []string, name string) bool {
	for _, a := range args {
		for _, f := range strings.Fields(a) {
			n, _, _ := strings.Cut(longFlag(f), "=")
			if n == "--"+name {
				return true
			}
		}
	}
	return false
}

// FlagValue returns a flag's value from a container's command and args.
func FlagValue(args []string, names ...string) (string, bool) {
	var fields []string
	for _, a := range args {
		fields = append(fields, strings.Fields(a)...)
	}
	for i, f := range fields {
		n, v, has := strings.Cut(longFlag(f), "=")
		for _, name := range names {
			if n != "--"+strings.TrimLeft(name, "-") {
				continue
			}
			if has {
				return v, true
			}
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
				return fields[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// APIServerPods returns the kube-apiserver static (mirror) pods of a
// self-managed control plane, sorted by name.
func APIServerPods(s *Snapshot) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.Namespace != NamespaceKubeSystem {
			continue
		}
		if p.Labels["component"] == "kube-apiserver" || strings.HasPrefix(p.Name, "kube-apiserver-") {
			out = append(out, p)
		}
	}
	sortPods(out)
	return out
}

func sortPods(ps []*corev1.Pod) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && podKey(ps[j]) < podKey(ps[j-1]); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

// containerArgs is a container's command and args together.
func containerArgs(c *corev1.Container) []string {
	return append(append([]string{}, c.Command...), c.Args...)
}

// envNames lists a container's env var names.
func envNames(c *corev1.Container) map[string]bool {
	out := map[string]bool{}
	for _, e := range c.Env {
		out[e.Name] = true
	}
	return out
}

// envValue is a container env var's literal value: only the variables the
// collector keeps (DCGM_EXPORTER_COLLECTORS, DCGM_EXPORTER_KUBERNETES,
// DCGM_EXPORTER_INTERVAL) have one.
func envValue(c *corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func parseBool(s string) (bool, bool) {
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	return b, err == nil
}
