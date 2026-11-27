"""Agent placement participates in lifecycle and single-cluster Flux checks:
a cluster a forge.NodeAgent targets gets forge's agent, so that cluster is one
the env targets, and a local, ephemeral or single-cluster Flux env refuses it
when it is not a cluster that env may deploy to. The same env without the
NodeAgent renders as on v1.13.0 (positive_ml_telemetry_v1130_compat.k): the
declaration is what makes the cluster a target.
"""
# kcl-args: policy=local
# kcl-args: policy=ephemeral
# kcl-args: policy=flux
# expect: gke_acme_us-central1_gpu
import forge

_policy = option("policy", default="local")
_gpu = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "forge-ml"}
output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = _policy if _policy != "flux" else Undefined
    applier = "flux" if _policy == "flux" else Undefined
    cluster_target = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev"}
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        node_agents = [forge.NodeAgent {target = _gpu}]
        telemetry = [forge.Telemetry {
            service_name = "prometheus", scope = "platform", cluster = "gke_acme_us-central1_gpu"
            namespaces = ["monitoring"], selector = {app = "prometheus"}
            metrics = [forge.MetricsScrape {port = 9090}]
        }]
    }
    ml_stacks = [forge.MLStack {
        cluster = _gpu
        monitoring = [forge.Adopt {kind = "prometheus", namespace = "monitoring"}]
    }]
})
