"""An MLStack gets its cluster collector on its cluster's agent record, with
defaults; collector.enabled = False, no destination, or an external
collector renders none."""
import forge

_target = forge.ClusterTarget {cluster = "gpu", namespace = "forge-ml", connected_cluster = "gpu-connected"}
_obs = forge.Observability {
    destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com", token_from_control_plane = True}
}
_bundle = lambda stack: forge.MLStack, obs: forge.Observability -> any {
    forge.render(forge.Bundle {
        project = "acme"
        env = "prod"
        control_plane = forge.ControlPlane {}
        observability = obs
        ml_stacks = [stack]
    })
}
_agents = lambda out: any -> [any] {
    [m for m in out.manifests if m.kind == "ObservabilityAgent"]
}

# No scrape at all: the collector alone places the agent on the cluster.
_out = _bundle(forge.MLStack {cluster = _target}, _obs)
_a = _agents(_out)
assert_agent = len(_a) == 1 and _a[0].metadata.labels["forge.dev/cluster"] == "gpu"
assert_defaults = _a[0].spec.mlCollector == {"cluster": "gpu", "nodeInterval": "60s", "inventoryInterval": "300s", "events": True}
assert_output = _out.ml_stacks[0].collector == {"rendered": True, "node_interval": "60s", "inventory_interval": "300s", "events": True}

_tuned = _agents(_bundle(forge.MLStack {
    cluster = _target
    collector = forge.MLCollector {node_interval = "30s", inventory_interval = "10m", events = False}
}, _obs))
assert_tuned = _tuned[0].spec.mlCollector == {"cluster": "gpu", "nodeInterval": "30s", "inventoryInterval": "10m", "events": False}

# Disabled, with a scrape: the agent stays for the scrape, with no collector.
_off = _bundle(forge.MLStack {
    cluster = _target
    collector = forge.MLCollector {enabled = False}
    monitoring = [forge.Adopt {kind = "prometheus", namespace = "monitoring", selector = {app = "prometheus"}, port = 9090}]
}, _obs)
assert_off = len(_agents(_off)) == 1 and "mlCollector" not in _agents(_off)[0].spec
assert_off_output = _off.ml_stacks[0].collector.rendered == False

# Disabled and nothing scraped: no agent at all (and so no token to deliver).
assert_off_no_agent = len(_agents(_bundle(forge.MLStack {cluster = _target, collector = forge.MLCollector {enabled = False}}, forge.Observability {
    destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
}))) == 0

# An external collector owns the env's collection: forge renders none.
_ext = _bundle(forge.MLStack {cluster = _target}, forge.Observability {
    destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
    telemetry_collector = forge.TelemetryCollector {namespace = "otel", pod_labels = {app = "otel"}}
})
assert_external = len(_agents(_ext)) == 0 and _ext.ml_stacks[0].collector.rendered == False

# Observability off: none.
_dark = _bundle(forge.MLStack {cluster = _target}, forge.Observability {enabled = False})
assert_dark = len(_agents(_dark)) == 0 and _dark.ml_stacks[0].collector.rendered == False
