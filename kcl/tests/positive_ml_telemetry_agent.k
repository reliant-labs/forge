"""
An ML-only cluster that Observability.agent.clusters names gets an agent and
its destination credential, and the agent collects the Observability.telemetry
entries that scrape that kube context.

The stack supplies the ClusterTarget the agent is placed on (its
connected_cluster delivers the credential), since no forge workload runs
there. A cluster the env does not name gets no agent, scraped or not (a
scrape alone is reported as uncollected, as on v1.13.0 it placed no agent),
and one agent per named cluster carries only that cluster's scrape.
"""
import forge
import forge.workloads as fw

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
_bundle = lambda tel: [forge.Telemetry], stacks: [forge.MLStack], wls: [any], agents: [str] -> forge.Bundle {
    forge.Bundle {
        project = "acme"
        env = "prod"
        control_plane = forge.ControlPlane {}
        observability = forge.Observability {
            destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com", token_from_control_plane = True}
            agent = forge.ObservabilityAgent {clusters = agents}
            telemetry = tel
        }
        ml_stacks = stacks
        workloads = wls
    }
}
_out = forge.render(_bundle([_scrape("gpu")], [_stack], [], ["gpu"]))
_agents = [m for m in _out.manifests if m.kind == "ObservabilityAgent"]
assert_agent = len(_agents) == 1 and _agents[0].metadata.labels["forge.dev/cluster"] == "gpu"
assert_scrape = len(_agents[0].spec.telemetry) == 1 and _agents[0].spec.telemetry[0].service_name == "prometheus-gpu"
assert_credential = len(_out.control_plane_secrets) == 1 and _out.control_plane_secrets[0].cluster == "gpu" and _out.control_plane_secrets[0].connected_cluster == "gpu-connected"
assert_collected = "uncollected_telemetry" not in _out

# A cluster the env does not name gets no agent, scraped or not: a scrape
# alone is reported uncollected, and the Adopt alone renders nothing.
_bare = _stack
_scraped_only = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        telemetry = [_scrape("gpu")]
    }
    ml_stacks = [_bare]
})
assert_scrape_alone_no_agent = len([m for m in _scraped_only.manifests if m.kind == "ObservabilityAgent"]) == 0 and len(_scraped_only.control_plane_secrets) == 0
assert_scrape_alone_uncollected = _scraped_only.uncollected_telemetry == [{"service_name": "prometheus-gpu", "cluster": "gpu"}]
_unscraped = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}}
    ml_stacks = [_bare]
})
assert_unscraped_no_agent = len([m for m in _unscraped.manifests if m.kind == "ObservabilityAgent"]) == 0 and "telemetry" not in _unscraped and "uncollected_telemetry" not in _unscraped

# A forge workload on the stack's cluster runs the agent there already, so
# its scrape is collected without the opt-in; naming the cluster as well
# places no second agent.
_wl = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _target}}]
_on_cluster = forge.render(_bundle([_scrape("gpu")], [_bare], _wl, []))
assert_workload_agent_collects = len([m for m in _on_cluster.manifests if m.kind == "ObservabilityAgent"]) == 1 and "uncollected_telemetry" not in _on_cluster
_on_cluster_named = forge.render(_bundle([_scrape("gpu")], [_bare], _wl, ["gpu"]))
assert_named_workload_cluster_one_agent = len([m for m in _on_cluster_named.manifests if m.kind == "ObservabilityAgent"]) == 1 and len(_on_cluster_named.control_plane_secrets) == 1

# Two scraped stacks: one agent per cluster, each with its own scrape only.
_two = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        agent = forge.ObservabilityAgent {clusters = ["gpu", "gpu-b"]}
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
        agent = forge.ObservabilityAgent {clusters = ["gpu"]}
        telemetry = [_scrape("gpu")]
    }
    ml_stacks = [_stack]
})
assert_external_owns_scrape = len(_external.telemetry) == 1 and len([m for m in _external.manifests if m.kind == "ObservabilityAgent"]) == 0
assert_external_no_credential = len(_external.control_plane_secrets) == 0
# The env's own collector collects the scrape: nothing is uncollected.
assert_external_collects = "uncollected_telemetry" not in _external
