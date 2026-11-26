"""ML agent placement participates in lifecycle and single-cluster Flux checks:
an Observability.telemetry scrape of a stack's cluster puts forge's agent there,
so that cluster is one the env targets."""
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
