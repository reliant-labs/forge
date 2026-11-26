"""
Positive test — a forge.MLStack lowers through existing delivery.

WHAT THIS PINS
  * an Install becomes a `bundled_charts` entry on the stack's cluster (the
    ClusterTarget's context), carrying the labels forge stamps on every object
    the chart renders: forge.dev/component-owner = <project>.<env> and
    forge.dev/component = ml-<kind>;
  * an Adopt is an inventory assertion: it renders no `telemetry` target,
    observability on or off (a scrape is an Observability.telemetry entry of
    its own, positive_ml_stack_one_component.k);
  * `output.ml_stacks` projects the declared stack: every component with its
    slot, mode and kind, `observed`, the accelerators and the decisions;
  * Accelerators.resource defaults from the vendor.
"""

import forge
import forge.workloads as fw

_gpu = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "forge-ml", platform = "amd64"}

_stack = forge.MLStack {
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
            values = {"metrics": {"enabled": True}}
        }
    }
    queue = forge.Adopt {kind = "kueue", namespace = "kueue-system"}
    training = [forge.Adopt {kind = "jobset"}]
    serving = [forge.Adopt {kind = "kserve"}]
    monitoring = [forge.Adopt {kind = "kube-state-metrics", namespace = "monitoring"}]
    decisions = [forge.RecommendationDecision {
        id = "kubelet-tracing-off", decision = "dismiss"
        reason = "GKE does not expose kubelet tracing config", expires = "2027-06-01"
    }]
}

_bundle = lambda obs: bool -> forge.Bundle {
    forge.Bundle {
        project = "acme"
        env = "prod"
        if obs:
            observability = forge.Observability {destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}}
        workloads = [fw.Workload {name = "api", image = "registry.example.com/acme/api:1", runtime = forge.OnCluster {target = _gpu}}]
        ml_stacks = [_stack]
    }
}

_out = forge.render(_bundle(True))
_dark = forge.render(_bundle(False))

# Install -> a bundle chart on the stack's cluster, labelled as forge's.
_charts = _out.bundled_charts
assert_one_chart = len(_charts) == 1 and _charts[0].name == "node-problem-detector"
assert_chart_pinned = _charts[0].repo == "https://charts.deliveryhero.io/" and _charts[0].chart == "node-problem-detector" and _charts[0].version == "2.3.14"
assert_chart_on_stack_cluster = _charts[0].cluster == "gke_acme_us-central1_gpu"
assert_chart_namespace = _charts[0].namespace == "forge-ml"
assert_chart_values = _charts[0].values == {"metrics": {"enabled": True}}
assert_chart_labels = _charts[0].labels == {"forge.dev/component-owner": "acme.prod", "forge.dev/component": "ml-node-problem-detector"}
assert_chart_not_bootstrap = len(_out.helm_charts) == 0

# Adopts render no telemetry target, with observability on or off.
assert_adopt_no_scrape = "telemetry" not in _out

assert_adopt_no_scrape_dark = "telemetry" not in _dark
assert_chart_without_observability = len(_dark.bundled_charts) == 1

# output.ml_stacks: the declared stack, read by tools.
_m = _out.ml_stacks[0]
assert_ml_one_stack = len(_out.ml_stacks) == 1
assert_ml_cluster = _m.cluster == "gke_acme_us-central1_gpu" and _m.namespace == "forge-ml"
assert_ml_observed = _m.observed == ["gpu-operator", "dcgm-exporter", "kueue"] and _m.observed_at == "2026-11-25"
assert_ml_accelerators = _m.accelerators == [{"vendor": "nvidia", "resource": "nvidia.com/gpu", "node_selector": {}}]
assert_ml_slots = [[c.slot, c.mode, c.kind] for c in _m.components] == [
    ["accelerators", "adopt", "dcgm-exporter"]
    ["node_health", "install", "node-problem-detector"]
    ["queue", "adopt", "kueue"]
    ["training", "adopt", "jobset"]
    ["serving", "adopt", "kserve"]
    ["monitoring", "adopt", "kube-state-metrics"]
]
assert_ml_adopt_detail = _m.components[0] == {
    "slot": "accelerators", "vendor": "nvidia", "mode": "adopt", "kind": "dcgm-exporter", "namespace": "gpu-operator"
    "version": "3.3.9", "note": "", "chart": "", "reason": ""
}
assert_ml_install_detail = _m.components[1].chart == "node-problem-detector" and _m.components[1].version == "2.3.14" and _m.components[1].namespace == "forge-ml" and _m.components[1].vendor == ""
assert_ml_same_without_observability = _dark.ml_stacks == _out.ml_stacks
assert_ml_decisions = _m.decisions == [{"id": "kubelet-tracing-off", "decision": "dismiss", "reason": "GKE does not expose kubelet tracing config", "expires": "2027-06-01"}]

# Accelerator resources default by vendor; generic names its own.
assert_resource_defaults = [forge.Accelerators {vendor = v}.resource for v in ["nvidia", "amd", "tpu", "neuron", "gaudi"]] == ["nvidia.com/gpu", "amd.com/gpu", "google.com/tpu", "aws.amazon.com/neuroncore", "habana.ai/gaudi"]
assert_resource_generic = forge.Accelerators {vendor = "generic", resource = "example.com/npu"}.resource == "example.com/npu"
