"""Only an emitted agent adds an ML cluster to apply targets: a cluster
Observability.agent.clusters names while the env's own collector owns
collection, observability is off, or there is no destination gets none; nor
does a stack's cluster that a telemetry entry scrapes without the entry
(v1.13.0's shape)."""
# kcl-args: mode=external
# kcl-args: mode=disabled
# kcl-args: mode=no-destination
# kcl-args: mode=no-opt-in
import forge

_mode = option("mode", default="external")
_outs = [forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = "local" if policy == "local" else Undefined
    applier = "flux" if policy == "flux" else Undefined
    cluster_target = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev"}
    observability = forge.Observability {
        enabled = _mode != "disabled"
        agent = forge.ObservabilityAgent {clusters = ["gke_acme_us-central1_gpu"] if _mode != "no-opt-in" else []}
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"} if _mode != "no-destination" else Undefined
        telemetry_collector = forge.TelemetryCollector {namespace = "otel", pod_labels = {app = "otel"}} if _mode == "external" else Undefined
        telemetry = [forge.Telemetry {
            service_name = "prometheus", scope = "platform", cluster = "gke_acme_us-central1_gpu"
            namespaces = ["monitoring"], selector = {app = "prometheus"}
            metrics = [forge.MetricsScrape {port = 9090}]
        }] if _mode != "disabled" else []
    }
    ml_stacks = [forge.MLStack {
        cluster = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "forge-ml"}
        monitoring = [forge.Adopt {kind = "prometheus", namespace = "monitoring"}]
    }]
}) for policy in ["local", "flux"]]
assert_no_remote_agent = all out in _outs {len([m for m in out.manifests if m.kind == "ObservabilityAgent" and m.metadata.labels["forge.dev/cluster"] == "gke_acme_us-central1_gpu"]) == 0}
