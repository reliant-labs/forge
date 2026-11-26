"""
A scraped ML-only cluster gets an agent and its destination credential.

The scrape is a plain Observability.telemetry entry on the stack's kube
context; the stack supplies the ClusterTarget the agent is placed on, since
no forge workload runs there. A stack nothing scrapes gets no agent, and one
agent per scraped cluster carries only that cluster's scrape.
"""
import forge

_target = forge.ClusterTarget {cluster = "gpu", namespace = "forge-ml", connected_cluster = "gpu-connected"}
_other = forge.ClusterTarget {cluster = "gpu-b", namespace = "forge-ml"}
_stack = forge.MLStack {
    cluster = _target
    monitoring = [forge.Adopt {kind = "prometheus", namespace = "monitoring"}]
}
_scrape = lambda ctx: str -> forge.Telemetry {
    forge.Telemetry {
        service_name = "prometheus-" + ctx, scope = "platform", cluster = ctx
        namespaces = ["monitoring"], selector = {app = "prometheus"}
        metrics = [forge.MetricsScrape {port = 9090}]
    }
}
_bundle = lambda tel: [forge.Telemetry] -> forge.Bundle {
    forge.Bundle {
        project = "acme"
        env = "prod"
        control_plane = forge.ControlPlane {}
        observability = forge.Observability {
            destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com", token_from_control_plane = True}
            telemetry = tel
        }
        ml_stacks = [_stack]
    }
}
_out = forge.render(_bundle([_scrape("gpu")]))
_agents = [m for m in _out.manifests if m.kind == "ObservabilityAgent"]
assert_agent = len(_agents) == 1 and _agents[0].metadata.labels["forge.dev/cluster"] == "gpu"
assert_scrape = len(_agents[0].spec.telemetry) == 1 and _agents[0].spec.telemetry[0].service_name == "prometheus-gpu"
assert_credential = len(_out.control_plane_secrets) == 1 and _out.control_plane_secrets[0].cluster == "gpu" and _out.control_plane_secrets[0].connected_cluster == "gpu-connected"

# An unscraped stack gets no agent: the Adopt alone renders nothing.
_unscraped = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}}
    ml_stacks = [_stack]
})
assert_unscraped_no_agent = len([m for m in _unscraped.manifests if m.kind == "ObservabilityAgent"]) == 0 and "telemetry" not in _unscraped

# Two scraped stacks: one agent per cluster, each with its own scrape only.
_two = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        telemetry = [_scrape("gpu"), _scrape("gpu-b")]
    }
    ml_stacks = [_stack, forge.MLStack {cluster = _other}]
})
_two_agents = [m for m in _two.manifests if m.kind == "ObservabilityAgent"]
assert_one_agent_per_cluster = sorted([m.metadata.labels["forge.dev/cluster"] for m in _two_agents]) == ["gpu", "gpu-b"]
assert_cluster_local_scrapes = all m in _two_agents { len(m.spec.telemetry) == 1 and m.spec.telemetry[0].cluster == m.metadata.labels["forge.dev/cluster"] }

_external = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        telemetry_collector = forge.TelemetryCollector {namespace = "otel", pod_labels = {app = "otel"}}
        telemetry = [_scrape("gpu")]
    }
    ml_stacks = [_stack]
})
assert_external_owns_scrape = len(_external.telemetry) == 1 and len([m for m in _external.manifests if m.kind == "ObservabilityAgent"]) == 0
assert_external_no_credential = len(_external.control_plane_secrets) == 0
