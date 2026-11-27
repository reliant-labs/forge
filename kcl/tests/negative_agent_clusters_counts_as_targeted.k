"""Agent placement participates in lifecycle and single-cluster Flux checks:
a cluster Observability.agent.clusters names gets forge's agent, so that
cluster is one the env targets, and a local, ephemeral or single-cluster Flux
env refuses it when it is not a cluster that env may deploy to. The same env
without the entry renders as on v1.13.0 (positive_ml_telemetry_v1130_compat.k):
the opt-in is what makes the cluster a target."""
# kcl-args: policy=local
# kcl-args: policy=ephemeral
# kcl-args: policy=flux
# expect: gke_acme_us-central1_gpu
import forge

_policy = option("policy", default="local")
output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = _policy if _policy != "flux" else Undefined
    applier = "flux" if _policy == "flux" else Undefined
    cluster_target = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev"}
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        agent = forge.ObservabilityAgent {clusters = ["gke_acme_us-central1_gpu"]}
        telemetry = [forge.Telemetry {
            service_name = "prometheus", scope = "platform", cluster = "gke_acme_us-central1_gpu"
            namespaces = ["monitoring"], selector = {app = "prometheus"}
            metrics = [forge.MetricsScrape {port = 9090}]
        }]
    }
    ml_stacks = [forge.MLStack {
        cluster = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "forge-ml"}
        monitoring = [forge.Adopt {kind = "prometheus", namespace = "monitoring"}]
    }]
})
