"""
Positive test — a stack of one component is valid and renders only that
component.

Nothing about an MLStack is all or nothing: only `cluster` is required, every
slot is optional, and a stack declares any subset of components. Each stack
here declares exactly one, and its render is the no-stack render plus that
component's own output and nothing for any kind it does not declare:

  * Kueue installed alone (delivery "bootstrap", the default): one
    `helm_charts` entry, labelled as forge's; no `bundled_charts`, no
    Namespace of forge's, no scrape;
  * Kueue adopted alone (no port): nothing in the cluster, only the
    `ml_stacks` projection;
  * the DCGM exporter adopted alone: nothing in the cluster, only the
    `ml_stacks` projection;
  * that Adopt plus an Observability.telemetry scrape of it: exactly that
    one scrape (and the agent it rides on), no chart of either delivery, no
    Namespace. The scrape is the env's, not the stack's.
"""

import json
import forge
import forge.workloads as fw

_gpu = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "acme-prod", platform = "amd64"}

_base = lambda stacks: [forge.MLStack] -> forge.Bundle {
    _with(stacks, [])
}
_with = lambda stacks: [forge.MLStack], tel: [forge.Telemetry] -> forge.Bundle {
    forge.Bundle {
        project = "acme"
        env = "prod"
        observability = forge.Observability {destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}, telemetry = tel}
        workloads = [fw.Workload {name = "api", image = "registry.example.com/acme/api:1", ports = [fw.Port {name = "http", port = 8080}], runtime = forge.OnCluster {target = _gpu}}]
        ml_stacks = stacks
    }
}

_without = forge.render(_base([]))

# The render with the named keys dropped: what is left must be the no-stack render.
_drop = lambda out: any, keys: [str] -> any {
    {k: v for k, v in out if k not in keys}
}
_namespaces = lambda out: any -> [str] {
    [m.metadata.name for m in out.manifests if m.kind == "Namespace"]
}
_scrapes = lambda out: any -> [str] {
    [t.service_name for t in out.telemetry] if "telemetry" in out else []
}

# Kueue installed alone.
_kueue_install = forge.render(_base([forge.MLStack {
    cluster = _gpu
    queue = forge.Install {
        kind = "kueue"
        chart = forge.HelmChart {name = "kueue", oci = "oci://registry.k8s.io/kueue/charts/kueue", version = "0.14.4", namespace = "kueue-system"}
    }
}]))
assert_kueue_install_one_chart = [h.name for h in _kueue_install.helm_charts] == ["kueue"]
assert_kueue_install_chart_on_stack_cluster = _kueue_install.helm_charts[0].cluster == "gke_acme_us-central1_gpu" and _kueue_install.helm_charts[0].cluster_target == True and _kueue_install.helm_charts[0].namespace == "kueue-system"
assert_kueue_install_labelled = _kueue_install.helm_charts[0].labels == {"forge.dev/component-owner": "acme.prod", "forge.dev/component": "ml-kueue"}
assert_kueue_install_nothing_bundled = "bundled_charts" not in _kueue_install
assert_kueue_install_no_namespace = _namespaces(_kueue_install) == _namespaces(_without)
assert_kueue_install_no_scrape = _scrapes(_kueue_install) == []
assert_kueue_install_only_its_chart = json.encode(_drop(_kueue_install, ["ml_stacks", "helm_charts"])) == json.encode(_drop(_without, ["helm_charts"]))
assert_kueue_install_projected = [[c.slot, c.mode, c.kind] for c in _kueue_install.ml_stacks[0].components] == [["queue", "install", "kueue"]]

# Kueue adopted alone, unscraped: an inventory assertion, nothing in the cluster.
_kueue_adopt = forge.render(_base([forge.MLStack {cluster = _gpu, queue = forge.Adopt {kind = "kueue", namespace = "kueue-system"}}]))
assert_kueue_adopt_lowers_to_nothing = json.encode(_drop(_kueue_adopt, ["ml_stacks"])) == json.encode(_without)
assert_kueue_adopt_projected = [[c.slot, c.mode, c.kind] for c in _kueue_adopt.ml_stacks[0].components] == [["queue", "adopt", "kueue"]]

# The DCGM exporter adopted alone: an inventory assertion.
_dcgm_stack = forge.MLStack {
    cluster = _gpu
    accelerators = [forge.Accelerators {vendor = "nvidia", metrics = forge.Adopt {kind = "dcgm-exporter", namespace = "gpu-operator"}}]
}
_dcgm = forge.render(_base([_dcgm_stack]))
assert_dcgm_adopt_lowers_to_nothing = json.encode(_drop(_dcgm, ["ml_stacks"])) == json.encode(_without)
assert_dcgm_projected = [[c.slot, c.vendor, c.mode, c.kind] for c in _dcgm.ml_stacks[0].components] == [["accelerators", "nvidia", "adopt", "dcgm-exporter"]]

# The same Adopt plus an Observability.telemetry scrape of it: that scrape only.
_dcgm_scrape = forge.Telemetry {
    service_name = "dcgm-exporter", scope = "platform", cluster = "gke_acme_us-central1_gpu"
    namespaces = ["gpu-operator"], selector = {"app": "nvidia-dcgm-exporter"}
    metrics = [forge.MetricsScrape {port = 9400, allow = "DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED"}]
}
_scraped = forge.render(_with([_dcgm_stack], [_dcgm_scrape]))
_scrape_only = forge.render(_with([], [_dcgm_scrape]))
assert_scraped_one_scrape = _scrapes(_scraped) == ["dcgm-exporter"]
_tel = _scraped.telemetry[0]
assert_scraped_target = _tel.source == "observability" and _tel.cluster == "gke_acme_us-central1_gpu" and _tel.namespaces == ["gpu-operator"] and _tel.selector == {"app": "nvidia-dcgm-exporter"} and _tel.metrics[0].port == 9400 and _tel.metrics[0].allow == "DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED"
assert_scraped_no_chart = "bundled_charts" not in _scraped and json.encode(_scraped.helm_charts if "helm_charts" in _scraped else []) == json.encode(_without.helm_charts if "helm_charts" in _without else [])
assert_scraped_no_namespace = _namespaces(_scraped) == _namespaces(_without)
# The stack adds only its projection to the env's own scrape.
assert_scraped_is_the_envs_scrape = json.encode(_drop(_scraped, ["ml_stacks"])) == json.encode(_scrape_only)
assert_scrape_on_agent = any m in _scraped.manifests { m.kind == "ObservabilityAgent" and "telemetry" in m.spec and [t.service_name for t in m.spec.telemetry] == ["dcgm-exporter"] }
