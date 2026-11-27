"""
render contract — a forge.MLStack on a GPU cluster the team runs, declared
the v1.14.0 way. The NVIDIA exporter and Kueue are adopted (inventory
assertions, which render nothing in the cluster), and the exporter is
scraped by a plain Observability.telemetry entry; node-problem-detector is
installed as a bundle chart and JobSet as a bootstrap chart (it owns CRDs),
and one recommendation is dismissed. The bundle Install lands in
`bundled_charts` with its Namespace in `manifests`, the bootstrap one in
`helm_charts`, both on the stack's cluster with the labels forge stamps on
their objects; the scrape lands in `telemetry` (on that cluster's agent
record), and the declared stack in `ml_stacks`. Chart versions are
illustrative.

ml-stack-v1.13.0.k is the same env declared the v1.13.0 way (the scrape on
the Adopt, now deprecated). Its golden is v1.13.0's, frozen: the scrape
target is the same, only its service_name and source differ.

The DCGM metric names are the author's data, as `forge ml discover` writes
them: forge knows none of them.
"""
# kcl-args: env="prod"

import forge
import forge.workloads as fw

_gpu = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "acme-prod", platform = "amd64"}

output = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        telemetry = [forge.Telemetry {
            service_name = "dcgm-exporter", scope = "platform", cluster = "gke_acme_us-central1_gpu"
            namespaces = ["gpu-operator"], selector = {"app": "nvidia-dcgm-exporter"}
            metrics = [forge.MetricsScrape {port = 9400, allow = "DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED|DCGM_FI_DEV_XID_ERRORS"}]
        }]
    }
    workloads = [fw.Workload {
        name = "trainer-ui", image = "registry.example.com/acme/trainer-ui:1.0.0"
        ports = [fw.Port {name = "http", port = 8080}]
        runtime = forge.OnCluster {target = _gpu}
    }]
    ml_stacks = [forge.MLStack {
        cluster = _gpu
        observed = ["gpu-operator", "dcgm-exporter", "kueue"]
        observed_at = "2026-11-25"
        accelerators = [forge.Accelerators {
            vendor = "nvidia"
            metrics = forge.Adopt {kind = "dcgm-exporter", namespace = "gpu-operator", version = "3.3.9"}
        }]
        node_health = forge.Install {
            kind = "node-problem-detector"
            chart = forge.HelmChart {
                name = "node-problem-detector", repo = "https://charts.deliveryhero.io/"
                chart = "node-problem-detector", version = "2.3.14", namespace = "forge-ml", delivery = "bundle"
            }
        }
        queue = forge.Adopt {kind = "kueue", namespace = "kueue-system", version = "0.9.1"}
        training = [forge.Install {
            kind = "jobset"
            chart = forge.HelmChart {
                name = "jobset", oci = "oci://registry.k8s.io/jobset/charts/jobset"
                version = "0.10.1", namespace = "jobset-system"
            }
        }]
        decisions = [forge.RecommendationDecision {
            id = "kubelet-tracing-off", decision = "dismiss"
            reason = "GKE does not expose kubelet tracing config", expires = "2027-06-01"
        }]
    }]
})
