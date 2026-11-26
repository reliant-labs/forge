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

func kubernetesInventorySpec() ObservabilityAgentSpec {
	s := agentSpec()
	s.HeadersSecret = nil
	s.TokenSecret = &ObservabilitySecretKey{Name: "forge-telemetry-token", Key: "token"}
	s.KubernetesInventory = &KubernetesInventory{Cluster: "k3d-gpu", NodeInterval: "60s", WorkloadInterval: "300s", Events: true}
	return s
}

// The agent with its cluster's Kubernetes inventory, goldened whole: the
// cluster collector's config (k8sobjects, the cluster stamp, the shared
// destination and token) and its read-only RBAC are a reviewed diff.
func TestRenderKubernetesInventoryGolden(t *testing.T) {
	got := agentYAML(t, kubernetesInventorySpec())
	path := filepath.Join("testdata", "observability_agent_kubernetes_inventory.golden.yaml")
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

func TestClusterCollectorConfig(t *testing.T) {
	s := kubernetesInventorySpec()
	cfgYAML, err := s.ClusterCollectorConfig(*s.KubernetesInventory)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	var objs []string
	for _, o := range cfg["receivers"].(map[string]any)["k8sobjects"].(map[string]any)["objects"].([]any) {
		m := o.(map[string]any)
		objs = append(objs, m["name"].(string)+"/"+m["mode"].(string))
	}
	if got := strings.Join(objs, ","); got != "nodes/pull,daemonsets/pull,deployments/pull,events/watch" {
		t.Errorf("objects = %s", got)
	}
	attrs := cfg["processors"].(map[string]any)["resource"].(map[string]any)["attributes"].([]any)
	if len(attrs) != 1 || attrs[0].(map[string]any)["key"] != ClusterNameAttribute || attrs[0].(map[string]any)["value"] != "k3d-gpu" {
		t.Errorf("resource attributes = %v, want only %s = k3d-gpu", attrs, ClusterNameAttribute)
	}
	if strings.Contains(cfgYAML, "file_storage") {
		t.Errorf("a Deployment has no host directory for a persistent queue:\n%s", cfgYAML)
	}
	if !strings.Contains(cfgYAML, "authenticator: bearertokenauth") {
		t.Errorf("exporter does not send the agent's token:\n%s", cfgYAML)
	}

	c := *s.KubernetesInventory
	c.Events = false
	noEvents, err := s.ClusterCollectorConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noEvents, "events") {
		t.Errorf("events = false still watches events:\n%s", noEvents)
	}
	c.NodeInterval = "sixty"
	if _, err := s.ClusterCollectorConfig(c); err == nil {
		t.Error("a bad interval was accepted")
	}
}

// The collector's ClusterRole reads only what it lists: no Secrets, no
// nodes/proxy, no write verb.
func TestClusterCollectorRBACIsReadOnly(t *testing.T) {
	objs, err := RenderObservabilityAgent(kubernetesInventorySpec(), agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range objs {
		if o.GetKind() != "ClusterRole" || o.GetName() != "forge-observability-"+ClusterCollectorName {
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
		t.Fatal("no cluster collector ClusterRole rendered")
	}
}

// Without KubernetesInventory the agent renders exactly what it did: no
// cluster collector objects, and the same agent config.
func TestNoClusterCollectorWithoutKubernetesInventory(t *testing.T) {
	objs, err := RenderObservabilityAgent(agentSpec(), agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if strings.Contains(o.GetName(), ClusterCollectorName) {
			t.Errorf("rendered %s %s", o.GetKind(), o.GetName())
		}
	}
}

func scrapedAgentSpec() ObservabilityAgentSpec {
	s := agentSpec()
	targets := []telemetry.Target{{
		ServiceName: "dcgm-exporter", Scope: telemetry.ScopePlatform, Source: "workload:gpu",
		Attachment: telemetry.AttachObservability, Runtime: telemetry.RuntimeCluster, Cluster: "k3d-gpu",
		Selector: map[string]string{"app": "nvidia-dcgm-exporter"}, Namespaces: []string{"gpu-operator"},
		Metrics: []telemetry.MetricsScrape{{Port: 9400, Path: "/metrics", Interval: "30s"}},
	}}
	s.Telemetry = &targets
	return s
}

// With KubernetesInventory, every scrape pipeline of the node agent
// carries the cluster and the node, before batch.
func TestScrapeCarriesClusterAndNodeWithKubernetesInventory(t *testing.T) {
	s := scrapedAgentSpec()
	s.KubernetesInventory = &KubernetesInventory{Cluster: "k3d-gpu", NodeInterval: "60s", WorkloadInterval: "300s", Events: true}
	cfgYAML, err := s.AgentConfigFor(agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg["service"].(map[string]any)["pipelines"].(map[string]any)["metrics/telemetry_dcgm-exporter"].(map[string]any)
	procs := p["processors"].([]any)
	if procs[len(procs)-1] != "batch" || procs[len(procs)-2] != scrapeClusterProcessor {
		t.Errorf("processors = %v", procs)
	}
	attrs := cfg["processors"].(map[string]any)[scrapeClusterProcessor].(map[string]any)["attributes"].([]any)
	got := map[string]any{}
	for _, a := range attrs {
		got[a.(map[string]any)["key"].(string)] = a.(map[string]any)["value"]
	}
	if got[ClusterNameAttribute] != "k3d-gpu" || got[NodeNameAttribute] != "${env:K8S_NODE_NAME}" || len(got) != 2 {
		t.Errorf("%s attributes = %v", scrapeClusterProcessor, got)
	}
}

// Without it, the node agent's scrapes are left as they were.
func TestScrapeUnstampedWithoutKubernetesInventory(t *testing.T) {
	cfgYAML, err := scrapedAgentSpec().AgentConfigFor(agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfgYAML, scrapeClusterProcessor) || strings.Contains(cfgYAML, "key: "+ClusterNameAttribute) {
		t.Errorf("scrape stamped without KubernetesInventory:\n%s", cfgYAML)
	}
}

// The agent record's `kubernetesInventory` key, exactly as kcl/render.k
// writes it (kcl/tests/positive_observability_kubernetes_inventory.k pins
// the KCL side of the same literal), decodes whole.
func TestKubernetesInventoryDecodesTheRenderedRecord(t *testing.T) {
	var s ObservabilityAgentSpec
	rec := `{"kubernetesInventory": {"cluster": "gpu", "nodeInterval": "60s", "workloadInterval": "300s", "events": true}}`
	if err := yaml.Unmarshal([]byte(rec), &s); err != nil {
		t.Fatal(err)
	}
	want := KubernetesInventory{Cluster: "gpu", NodeInterval: "60s", WorkloadInterval: "300s", Events: true}
	if s.KubernetesInventory == nil || *s.KubernetesInventory != want {
		t.Errorf("decoded %+v, want %+v", s.KubernetesInventory, want)
	}
}
