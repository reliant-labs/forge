package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// mlAgentlessEnv is v1.13.0's input shape: an Observability.telemetry entry
// scraping an ML stack's cluster that no forge workload runs on, in a
// topology that refuses that cluster as an extra target. connected_clusters
// declares the cluster, so the telemetry validation accepts the target
// (forge env render refuses a target on a cluster the env does not name);
// it does not make the cluster one the env deploys to.
const mlAgentlessEnv = `import forge
import forge.workloads as fw

_policy = "POLICY"
_gpu = "gke_acme_us-central1_gpu"
_target = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev", connected_cluster = "acme" if _policy == "token" else Undefined}
output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = _policy if _policy in ["local", "ephemeral"] else Undefined
    applier = "flux" if _policy == "flux" else Undefined
    control_plane = forge.ControlPlane {} if _policy == "token" else Undefined
    cluster_target = _target
    connected_clusters = {"gke_acme_us-central1_gpu": "gpu"}
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _target}}] if _policy == "token" else []
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com", token_from_control_plane = _policy == "token"}
        telemetry = [forge.Telemetry {
            service_name = "prometheus", scope = "platform", cluster = _gpu
            namespaces = ["monitoring"], selector = {app = "prometheus"}
            metrics = [forge.MetricsScrape {port = 9090}]
        }]
    }
    ml_stacks = [forge.MLStack {
        cluster = forge.ClusterTarget {cluster = _gpu, namespace = "forge-ml"}
        monitoring = [forge.Adopt {kind = "prometheus", namespace = "monitoring"}]
    }]
})
`

// An input that rendered on v1.13.0 still renders: a scrape of an ML stack's
// cluster with no forge workload places no node agent there (so no lifecycle,
// Flux or credential check sees a new target), and forge env render warns
// that the scrape is not collected, naming the opt-in. kcl/tests
// positive_ml_telemetry_v1130_compat.k pins the KCL half.
func TestMLScrapeOnAgentlessStackClusterRendersAndWarns(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: renders KCL four times; runs in task test")
	}
	kclplugin.Register()
	for _, policy := range []string{"local", "ephemeral", "flux", "token"} {
		t.Run(policy, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"kcl.mod": "[package]\nname = \"mlagentless\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
				"main.k":  strings.Replace(mlAgentlessEnv, "POLICY", policy, 1),
			}
			for f, c := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := kclrender.Run(dir, dir, nil)
			if err != nil {
				t.Fatalf("a v1.13.0 input no longer renders: %v", err)
			}
			var raw struct {
				Manifests []struct {
					Kind     string `json:"kind"`
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"manifests"`
			}
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			for _, m := range raw.Manifests {
				if m.Kind == "ObservabilityAgent" && m.Metadata.Labels["forge.dev/cluster"] == "gke_acme_us-central1_gpu" {
					t.Errorf("an agent was placed on the stack's cluster, which v1.13.0 did not")
				}
			}
			entities, err := parseKCLEntities(out)
			if err != nil {
				t.Fatal(err)
			}
			var errOut bytes.Buffer
			if err := checkEnvTelemetry(&errOut, "dev", entities); err != nil {
				t.Fatalf("telemetry check refused the env: %v", err)
			}
			got := errOut.String()
			for _, want := range []string{
				`warning: ml stack "gke_acme_us-central1_gpu": Observability.telemetry "prometheus" scrapes cluster "gke_acme_us-central1_gpu", where forge runs no node agent`,
				"so it is not collected",
				"Set node_agent = True on the forge.MLStack",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("render warnings lack %q:\n%s", want, got)
				}
			}
		})
	}
}

// One warning per stack, listing every uncollected entry; a stack whose
// scrapes are collected (or that has none) warns nothing.
func TestMLScrapeUncollectedWarnsPerStack(t *testing.T) {
	w := mlUncollectedTelemetry(&KCLEntities{MLStacks: []MLStackOutput{
		{Cluster: "gpu", UncollectedTelemetry: []string{"dcgm", "prometheus"}},
		{Cluster: "gpu-b"},
	}})
	if len(w) != 1 || !strings.Contains(w[0], `ml stack "gpu": Observability.telemetry "dcgm", "prometheus" scrapes cluster "gpu"`) {
		t.Fatalf("warnings = %q", w)
	}
	if w := mlUncollectedTelemetry(nil); w != nil {
		t.Errorf("nil entities warned: %q", w)
	}
	var s MLStackOutput
	if err := json.Unmarshal([]byte(`{"cluster":"gpu","uncollected_telemetry":["prometheus"]}`), &s); err != nil || len(s.UncollectedTelemetry) != 1 {
		t.Errorf("uncollected_telemetry not decoded: %+v %v", s, err)
	}
	b, err := json.Marshal(MLStackOutput{Cluster: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("uncollected_telemetry")) {
		t.Errorf("a stack with nothing uncollected encodes uncollected_telemetry: %s", b)
	}
}
