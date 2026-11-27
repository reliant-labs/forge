"""
An Observability.telemetry entry that scrapes an ML stack's cluster no forge
workload runs on renders as it did on v1.13.0: no node agent there, so the
cluster is not a target of the env and no check about targets changes. The
input is v1.13.0's shape exactly, in each topology that refuses an extra
target: a local and an ephemeral lifecycle (the cluster is not one forge
creates), a single-cluster Flux applier (it would be a second cluster), and
control-plane credential delivery (it names no connected cluster). The scrape
is reported as uncollected (output.ml_stacks uncollected_telemetry), which
`forge env render` prints as a warning.
"""
# kcl-args: policy=local | policy=ephemeral | policy=flux | policy=token
import forge
import forge.workloads as fw

_policy = option("policy", default="local")
_gpu = "gke_acme_us-central1_gpu"
_target = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev", connected_cluster = "acme" if _policy == "token" else Undefined}
output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = _policy if _policy in ["local", "ephemeral"] else Undefined
    applier = "flux" if _policy == "flux" else Undefined
    control_plane = forge.ControlPlane {} if _policy == "token" else Undefined
    cluster_target = _target
    # token_from_control_plane needs a cluster workload for the agent it
    # credentials.
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
assert_no_agent_on_stack_cluster = len([m for m in output.manifests if m.kind == "ObservabilityAgent" and m.metadata.labels["forge.dev/cluster"] == _gpu]) == 0
assert_no_credential_on_stack_cluster = len([s for s in output.control_plane_secrets if s.cluster == _gpu]) == 0
assert_scrape_still_declared = [t.service_name for t in output.telemetry] == ["prometheus"]
assert_reported_uncollected = output.ml_stacks[0].uncollected_telemetry == ["prometheus"]
