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
// that the scrape is not collected, naming the opt-in
// (Observability.agent.clusters). kcl/tests
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
				`warning: forge.Telemetry "prometheus" scrapes cluster "gke_acme_us-central1_gpu", where forge runs no node agent`,
				"so it is not collected",
				`Add "gke_acme_us-central1_gpu" to Observability.agent.clusters to run forge's node agent there`,
				`that makes "gke_acme_us-central1_gpu" a cluster the env deploys to`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("render warnings lack %q:\n%s", want, got)
				}
			}
		})
	}
}

// One warning per cluster, listing every uncollected target of it in order;
// nothing uncollected warns nothing, and the key is absent from a render
// without one.
func TestUncollectedTelemetryWarnsPerCluster(t *testing.T) {
	w := uncollectedTelemetryWarnings(&KCLEntities{UncollectedTelemetry: []UncollectedTelemetryOutput{
		{ServiceName: "dcgm", Cluster: "gpu"},
		{ServiceName: "kube-prometheus", Cluster: "helm-only"},
		{ServiceName: "prometheus", Cluster: "gpu"},
	}})
	if len(w) != 2 ||
		!strings.Contains(w[0], `forge.Telemetry "dcgm", "prometheus" scrape cluster "gpu"`) ||
		!strings.Contains(w[1], `forge.Telemetry "kube-prometheus" scrapes cluster "helm-only"`) ||
		!strings.Contains(w[1], `Add "helm-only" to Observability.agent.clusters`) {
		t.Fatalf("warnings = %q", w)
	}
	if w := uncollectedTelemetryWarnings(nil); w != nil {
		t.Errorf("nil entities warned: %q", w)
	}
	if w := uncollectedTelemetryWarnings(&KCLEntities{}); w != nil {
		t.Errorf("nothing uncollected warned: %q", w)
	}
	e, err := parseKCLEntities([]byte(`{"project":"acme","env":"dev","uncollected_telemetry":[{"service_name":"prometheus","cluster":"helm-only"}]}`))
	if err != nil || len(e.UncollectedTelemetry) != 1 || e.UncollectedTelemetry[0] != (UncollectedTelemetryOutput{ServiceName: "prometheus", Cluster: "helm-only"}) {
		t.Errorf("uncollected_telemetry not decoded: %+v %v", e, err)
	}
}

// helmOnlyEnv is a cluster that runs only a HelmChart: no forge workload and
// no forge.MLStack. AGENTS is Observability.agent.clusters.
const helmOnlyEnv = `import forge
import forge.workloads as fw

_hub = forge.ClusterTarget {cluster = "hub", namespace = "acme"}
output = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    cluster_target = _hub
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _hub}}]
    helm_charts = [forge.HelmChart {
        name = "kube-prometheus-stack", chart = "kube-prometheus-stack"
        repo = "https://prometheus-community.github.io/helm-charts", version = "75.0.0"
        namespace = "monitoring", cluster = forge.ClusterTarget {cluster = "helm-only", namespace = "monitoring"}
    }]
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        agent = forge.ObservabilityAgent {clusters = AGENTS}
        telemetry = [forge.Telemetry {
            service_name = "prometheus", scope = "platform", cluster = "helm-only"
            namespaces = ["monitoring"], selector = {app = "prometheus"}
            metrics = [forge.MetricsScrape {port = 9090}]
        }]
    }
})
`

// Observability.agent.clusters is not ML-specific: naming a Helm-only
// cluster puts forge's node agent there and the scrape is collected, with no
// warning; without it the render warns that the scrape is not collected,
// naming the setting. kcl/tests positive_agent_clusters_helm_only.k pins the
// KCL half.
func TestAgentClustersHelmOnlyCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: renders KCL twice; runs in task test")
	}
	kclplugin.Register()
	for _, tc := range []struct {
		agents    string
		wantAgent bool
	}{{`["helm-only"]`, true}, {`[]`, false}} {
		t.Run(tc.agents, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"kcl.mod": "[package]\nname = \"helmonly\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
				"main.k":  strings.Replace(helmOnlyEnv, "AGENTS", tc.agents, 1),
			}
			for f, c := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := kclrender.Run(dir, dir, nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			entities, err := parseKCLEntities(out)
			if err != nil {
				t.Fatal(err)
			}
			agent := false
			for _, c := range entities.ManifestClusters {
				if c.Cluster == "helm-only" {
					agent = true
				}
			}
			if agent != tc.wantAgent {
				t.Errorf("objects on helm-only = %v, want %v", agent, tc.wantAgent)
			}
			var errOut bytes.Buffer
			if err := checkEnvTelemetry(&errOut, "prod", entities); err != nil {
				t.Fatalf("telemetry check refused the env: %v", err)
			}
			warned := strings.Contains(errOut.String(), `forge.Telemetry "prometheus" scrapes cluster "helm-only"`) &&
				strings.Contains(errOut.String(), `Add "helm-only" to Observability.agent.clusters`)
			if warned == tc.wantAgent {
				t.Errorf("warned = %v with agent = %v:\n%s", warned, tc.wantAgent, errOut.String())
			}
		})
	}
}
