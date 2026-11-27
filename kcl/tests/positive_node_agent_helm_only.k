"""
forge.NodeAgent is not ML-specific: a cluster that runs only a HelmChart (no
forge workload, no forge.MLStack) gets forge's node agent when a NodeAgent
targets it, and that agent collects the Observability.telemetry entry that
scrapes the chart's pods there (the scrape is declared once, as a Telemetry;
the NodeAgent declares none). The NodeAgent reuses the chart's own
ClusterTarget, the env-wide kubernetes_inventory covers its cluster, and its
connected_cluster is recorded (output.node_agents) for the deploy's cluster
binding. Without the NodeAgent the cluster gets no agent (as on v1.13.0), and
the scrape is reported as uncollected (output.uncollected_telemetry), which
`forge env render` prints as a warning.
"""
import forge
import forge.workloads as fw

_hub = forge.ClusterTarget {cluster = "hub", namespace = "acme"}
_helm = forge.ClusterTarget {cluster = "helm-only", namespace = "monitoring", connected_cluster = "helm-only-connected"}
_bundle = lambda agents: [forge.NodeAgent] -> forge.Bundle {
    forge.Bundle {
        project = "acme"
        env = "prod"
        cluster_target = _hub
        workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _hub}}]
        helm_charts = [forge.HelmChart {
            name = "kube-prometheus-stack"
            chart = "kube-prometheus-stack"
            repo = "https://prometheus-community.github.io/helm-charts"
            version = "75.0.0"
            namespace = "monitoring"
            cluster = _helm
        }]
        observability = forge.Observability {
            destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
            agent = forge.ObservabilityAgent {kubernetes_inventory = forge.KubernetesInventory {}}
            node_agents = agents
            telemetry = [forge.Telemetry {
                service_name = "prometheus", scope = "platform", cluster = "helm-only"
                namespaces = ["monitoring"], selector = {"app.kubernetes.io/name": "prometheus"}
                metrics = [forge.MetricsScrape {port = 9090}]
            }]
        }
    }
}
_agent_on = lambda out: any, ctx: str -> [any] {
    [m for m in out.manifests if m.kind == "ObservabilityAgent" and m.metadata.labels["forge.dev/cluster"] == ctx]
}

_placed = forge.render(_bundle([forge.NodeAgent {target = _helm}]))
assert_agent_on_helm_cluster = len(_agent_on(_placed, "helm-only")) == 1
# The agent runs in Observability.agent.namespace, not the target's.
assert_agent_namespace = _agent_on(_placed, "helm-only")[0].metadata.namespace == "forge-observability"
assert_agent_collects_scrape = [t.service_name for t in _agent_on(_placed, "helm-only")[0].spec.telemetry] == ["prometheus"]
assert_inventory_on_helm_cluster = _agent_on(_placed, "helm-only")[0].spec.kubernetesInventory.cluster == "helm-only"
assert_workload_cluster_keeps_agent = len(_agent_on(_placed, "hub")) == 1
assert_placed_collected = "uncollected_telemetry" not in _placed
assert_no_ml_stacks = "ml_stacks" not in _placed
assert_binding_recorded = _placed.node_agents == [{"cluster": "helm-only", "connected_cluster": "helm-only-connected"}]
# It places the agent, so it does something with no overrides: no warning.
assert_placing_not_noop = "noop_node_agents" not in _placed

_none = forge.render(_bundle([]))
assert_none_no_agent = len(_agent_on(_none, "helm-only")) == 0 and len(_agent_on(_none, "hub")) == 1
assert_none_uncollected = _none.uncollected_telemetry == [{"service_name": "prometheus", "cluster": "helm-only"}]
assert_none_no_node_agents_key = "node_agents" not in _none
