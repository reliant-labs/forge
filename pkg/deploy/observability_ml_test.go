package deploy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/reliant-labs/forge/pkg/telemetry"
)

func mlCollectorSpec() ObservabilityAgentSpec {
	s := agentSpec()
	s.HeadersSecret = nil
	s.TokenSecret = &ObservabilitySecretKey{Name: "forge-telemetry-token", Key: "token"}
	s.MLCollector = &MLClusterCollector{Cluster: "k3d-ml-gpu", NodeInterval: "60s", InventoryInterval: "300s", Events: true}
	return s
}

// The agent with the ML cluster collector, goldened whole: the collector's
// config (k8sobjects, the cluster stamp, the shared destination and token)
// and its read-only RBAC are a reviewed diff.
func TestRenderMLCollectorGolden(t *testing.T) {
	got := agentYAML(t, mlCollectorSpec())
	path := filepath.Join("testdata", "observability_agent_ml_collector.golden.yaml")
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with -update)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the render (-update to accept):\n--- got\n%s", path, got)
	}
}

func TestMLCollectorConfig(t *testing.T) {
	s := mlCollectorSpec()
	cfgYAML, err := s.MLCollectorConfig(*s.MLCollector)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	objs := cfg["receivers"].(map[string]any)["k8sobjects"].(map[string]any)["objects"].([]any)
	var names []string
	for _, o := range objs {
		names = append(names, o.(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "nodes,daemonsets,deployments,events" {
		t.Errorf("objects = %s", got)
	}
	if !strings.Contains(cfgYAML, "key: k8s.cluster.name") || !strings.Contains(cfgYAML, "value: k3d-ml-gpu") || !strings.Contains(cfgYAML, "key: forge.ml.collector") {
		t.Errorf("resource processor missing the cluster stamp:\n%s", cfgYAML)
	}
	if strings.Contains(cfgYAML, "file_storage") {
		t.Errorf("a Deployment has no host directory for a persistent queue:\n%s", cfgYAML)
	}
	if !strings.Contains(cfgYAML, "authenticator: bearertokenauth") {
		t.Errorf("exporter does not send the agent's token:\n%s", cfgYAML)
	}

	c := *s.MLCollector
	c.Events = false
	noEvents, err := s.MLCollectorConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noEvents, "events") {
		t.Errorf("events = false still watches events:\n%s", noEvents)
	}
	c.NodeInterval = "sixty"
	if _, err := s.MLCollectorConfig(c); err == nil {
		t.Error("a bad interval was accepted")
	}
}

// The collector's ClusterRole reads only what it lists: no Secrets, no
// nodes/proxy, no write verb.
func TestMLCollectorRBACIsReadOnly(t *testing.T) {
	objs, err := RenderObservabilityAgent(mlCollectorSpec(), agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range objs {
		if o.GetKind() != "ClusterRole" || o.GetName() != "forge-observability-"+MLCollectorName {
			continue
		}
		found = true
		b, _ := yaml.Marshal(o.Object)
		for _, bad := range []string{"secrets", "nodes/proxy", "create", "update", "patch", "delete"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("ClusterRole grants %q:\n%s", bad, b)
			}
		}
	}
	if !found {
		t.Fatal("no ML collector ClusterRole rendered")
	}
}

// Without MLCollector the agent renders no collector objects.
func TestNoMLCollectorWithoutDeclaration(t *testing.T) {
	objs, err := RenderObservabilityAgent(agentSpec(), agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if strings.Contains(o.GetName(), MLCollectorName) {
			t.Errorf("rendered %s %s", o.GetKind(), o.GetName())
		}
	}
}

// An ML scrape on the node agent carries the MLStack's cluster; other
// targets do not.
func TestMLScrapeCarriesClusterName(t *testing.T) {
	s := agentSpec()
	targets := []telemetry.Target{{
		ServiceName: "ml-dcgm-exporter", Scope: telemetry.ScopePlatform, Source: "ml-stack:k3d-ml-gpu",
		Attachment: telemetry.AttachObservability, Runtime: telemetry.RuntimeCluster, Cluster: "k3d-ml-gpu",
		Selector: map[string]string{"app": "nvidia-dcgm-exporter"}, Namespaces: []string{"gpu-operator"},
		Metrics: []telemetry.MetricsScrape{{Port: 9400, Path: "/metrics", Interval: "30s"}},
	}}
	s.Telemetry = &targets
	cfgYAML, err := s.AgentConfigFor(agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg["service"].(map[string]any)["pipelines"].(map[string]any)["metrics/telemetry_ml-dcgm-exporter"].(map[string]any)
	procs := p["processors"].([]any)
	if procs[len(procs)-1] != "batch" || procs[len(procs)-2] != "resource/ml_cluster/ml-dcgm-exporter" {
		t.Errorf("processors = %v", procs)
	}
	if !strings.Contains(cfgYAML, "value: ${env:K8S_NODE_NAME}") {
		t.Errorf("no node stamp:\n%s", cfgYAML)
	}
	if !strings.Contains(cfgYAML, "value: k3d-ml-gpu") {
		t.Errorf("no cluster stamp:\n%s", cfgYAML)
	}
}
