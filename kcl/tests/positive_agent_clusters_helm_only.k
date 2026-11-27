"""
Observability.agent.clusters is not ML-specific: a cluster that runs only a
HelmChart (no forge workload, no forge.MLStack) gets forge's node agent when
the env names it, and the agent collects the Observability.telemetry entry
that scrapes the chart's pods there, with kubernetes_inventory on that cluster
too. Without the entry the cluster gets no agent (as on v1.13.0), and the
scrape is reported as uncollected (output.uncollected_telemetry), which
`forge env render` prints as a warning.
"""
import forge
import forge.workloads as fw

_hub = forge.ClusterTarget {cluster = "hub", namespace = "acme"}
_helm = forge.ClusterTarget {cluster = "helm-only", namespace = "monitoring"}
_bundle = lambda agents: [str] -> forge.Bundle {
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
            agent = forge.ObservabilityAgent {
                clusters = agents
                kubernetes_inventory = forge.KubernetesInventory {}
            }
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

_named = forge.render(_bundle(["helm-only"]))
assert_agent_on_helm_cluster = len(_agent_on(_named, "helm-only")) == 1
assert_agent_collects_scrape = [t.service_name for t in _agent_on(_named, "helm-only")[0].spec.telemetry] == ["prometheus"]
assert_inventory_on_helm_cluster = _agent_on(_named, "helm-only")[0].spec.kubernetesInventory.cluster == "helm-only"
assert_workload_cluster_keeps_agent = len(_agent_on(_named, "hub")) == 1
assert_named_collected = "uncollected_telemetry" not in _named
assert_no_ml_stacks = "ml_stacks" not in _named

_unnamed = forge.render(_bundle([]))
assert_unnamed_no_agent = len(_agent_on(_unnamed, "helm-only")) == 0 and len(_agent_on(_unnamed, "hub")) == 1
assert_unnamed_uncollected = _unnamed.uncollected_telemetry == [{"service_name": "prometheus", "cluster": "helm-only"}]
