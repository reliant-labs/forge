"""
Positive test — scraping an ML component through Observability.telemetry,
the way v1.14.0 documents, beside the deprecated Adopt scrape fields.

WHAT THIS PINS
  * an Adopt without scrape fields renders nothing in the cluster: only the
    `ml_stacks` projection;
  * that Adopt plus a plain Observability.telemetry scrape renders exactly
    that one scrape (on the agent), and the stack adds only its projection;
  * the new path renders the SAME scrape target as the deprecated path
    (Adopt selector + port + allow): cluster, namespaces, selector, scope,
    port and allow are equal, and only service_name and source differ;
  * both paths at once never double-scrape: a Telemetry of the derived
    service name (ml-<kind>) replaces the derived scrape (v1.13.0's rule),
    and the component still projects that service_name.
"""

import json
import forge
import forge.workloads as fw

_gpu = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu", namespace = "acme-prod", platform = "amd64"}

_bundle = lambda stacks: [forge.MLStack], tel: [forge.Telemetry] -> forge.Bundle {
    forge.Bundle {
        project = "acme"
        env = "prod"
        observability = forge.Observability {destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}, telemetry = tel}
        workloads = [fw.Workload {name = "api", image = "registry.example.com/acme/api:1", ports = [fw.Port {name = "http", port = 8080}], runtime = forge.OnCluster {target = _gpu}}]
        ml_stacks = stacks
    }
}
_drop = lambda out: any, keys: [str] -> any {
    {k: v for k, v in out if k not in keys}
}
_scrapes = lambda out: any -> [str] {
    [t.service_name for t in out.telemetry] if "telemetry" in out else []
}

_without = forge.render(_bundle([], []))

_inventory = forge.MLStack {
    cluster = _gpu
    accelerators = [forge.Accelerators {vendor = "nvidia", metrics = forge.Adopt {kind = "dcgm-exporter", namespace = "gpu-operator"}}]
}
_allow = ["DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED"]
_deprecated = forge.MLStack {
    cluster = _gpu
    accelerators = [forge.Accelerators {vendor = "nvidia", metrics = forge.Adopt {
        kind = "dcgm-exporter", namespace = "gpu-operator"
        selector = {"app": "nvidia-dcgm-exporter"}, port = 9400, allow = _allow
    }}]
}
_scrape = lambda name: str -> forge.Telemetry {
    forge.Telemetry {
        service_name = name, scope = "platform", cluster = "gke_acme_us-central1_gpu"
        namespaces = ["gpu-operator"], selector = {"app": "nvidia-dcgm-exporter"}
        metrics = [forge.MetricsScrape {port = 9400, allow = "|".join(_allow)}]
    }
}

# An Adopt alone: an inventory assertion.
_adopted = forge.render(_bundle([_inventory], []))
assert_adopt_lowers_to_nothing = json.encode(_drop(_adopted, ["ml_stacks"])) == json.encode(_without)

# The Adopt plus an Observability.telemetry scrape: that scrape only.
_new = forge.render(_bundle([_inventory], [_scrape("dcgm-exporter")]))
_scrape_only = forge.render(_bundle([], [_scrape("dcgm-exporter")]))
assert_new_one_scrape = _scrapes(_new) == ["dcgm-exporter"]
assert_new_is_the_envs_scrape = json.encode(_drop(_new, ["ml_stacks"])) == json.encode(_scrape_only)
assert_new_on_agent = any m in _new.manifests { m.kind == "ObservabilityAgent" and "telemetry" in m.spec and [t.service_name for t in m.spec.telemetry] == ["dcgm-exporter"] }
assert_new_projects_no_own_scrape = _new.ml_stacks[0].components[0].port == 0 and _new.ml_stacks[0].components[0].service_name == ""

# The deprecated path still renders its derived scrape (v1.13.0).
_old = forge.render(_bundle([_deprecated], []))
assert_old_one_scrape = _scrapes(_old) == ["ml-dcgm-exporter"]

# The same scrape target either way: only service_name and source differ.
_target = lambda t: any -> any {
    {k: v for k, v in t if k not in ["service_name", "source"]}
}
assert_same_target = json.encode(_target(_new.telemetry[0])) == json.encode(_target(_old.telemetry[0]))
assert_sources = _new.telemetry[0].source == "observability" and _old.telemetry[0].source == "ml-stack:gke_acme_us-central1_gpu"

# Both paths at once: a Telemetry of the derived name replaces the derived
# scrape, so the exporter is scraped once.
_both = forge.render(_bundle([_deprecated], [_scrape("ml-dcgm-exporter")]))
assert_both_scrape_once = _scrapes(_both) == ["ml-dcgm-exporter"] and _both.telemetry[0].source == "observability"
assert_both_agent_scrapes_once = all m in _both.manifests { m.kind != "ObservabilityAgent" or [t.service_name for t in m.spec.telemetry] == ["ml-dcgm-exporter"] }
assert_both_still_projected = _both.ml_stacks[0].components[0].service_name == "ml-dcgm-exporter"
