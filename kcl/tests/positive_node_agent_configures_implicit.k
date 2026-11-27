"""
A forge.NodeAgent on a cluster a forge workload already runs on configures
that cluster's implicit agent; it never places a second one. Its overrides
(image, memory_limit_mib, kubernetes_inventory) apply on its cluster alone,
every other agent cluster keeps Observability.agent's values, and the
cluster still gets one destination credential. A per-cluster
kubernetes_inventory replaces the env-wide one there, and `enabled = False`
turns the env-wide one off on that cluster. A NodeAgent with no override
there does nothing, and is listed in output.noop_node_agents for forge env
render's warning.
"""
import forge
import forge.workloads as fw

_gpu = forge.ClusterTarget {cluster = "gpu", namespace = "acme-prod", connected_cluster = "gpu-connected"}
_web = forge.ClusterTarget {cluster = "web", namespace = "acme-prod", connected_cluster = "web-connected"}
_bundle = lambda agent: forge.ObservabilityAgent, agents: [forge.NodeAgent], destination: bool = True -> any {
    forge.render(forge.Bundle {
        project = "acme"
        env = "prod"
        control_plane = forge.ControlPlane {} if destination else Undefined
        observability = forge.Observability {
            if destination:
                destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com", token_from_control_plane = True}
            agent = agent
            node_agents = agents
        }
        workloads = [
            fw.Workload {name = "trainer", image = "registry.example.com/acme/trainer:1", ports = [fw.Port {name = "http", port = 8080}], runtime = forge.OnCluster {target = _gpu}}
            fw.Workload {name = "api", image = "registry.example.com/acme/api:1", ports = [fw.Port {name = "http", port = 8080}], runtime = forge.OnCluster {target = _web}}
        ]
    })
}
_agents = lambda out: any -> [any] {
    [m for m in out.manifests if m.kind == "ObservabilityAgent"]
}
_agent = lambda out: any, cluster: str -> any {
    [m for m in out.manifests if m.kind == "ObservabilityAgent" and m.metadata.labels["forge.dev/cluster"] == cluster][0]
}

_implicit = _bundle(forge.ObservabilityAgent {}, [])

# The NodeAgent reuses the workload's own target: one agent per cluster,
# and only gpu's record changes.
_tuned = _bundle(forge.ObservabilityAgent {}, [forge.NodeAgent {
    target = _gpu
    image = "mirror.example.com/otel/opentelemetry-collector-contrib:0.1"
    memory_limit_mib = 1024
    kubernetes_inventory = forge.KubernetesInventory {node_interval = "30s"}
}])
assert_one_agent_per_cluster = sorted([m.metadata.labels["forge.dev/cluster"] for m in _agents(_tuned)]) == ["gpu", "web"]
assert_overrides_on_gpu = _agent(_tuned, "gpu").spec.image == "mirror.example.com/otel/opentelemetry-collector-contrib:0.1" \
    and _agent(_tuned, "gpu").spec.memoryLimitMiB == 1024 \
    and _agent(_tuned, "gpu").spec.kubernetesInventory == {"cluster": "gpu", "nodeInterval": "30s", "workloadInterval": "300s", "events": True}
assert_web_untouched = _agent(_tuned, "web") == _agent(_implicit, "web")
assert_one_credential_per_cluster = sorted([s.cluster + "=" + s.connected_cluster for s in _tuned.control_plane_secrets]) == ["gpu=gpu-connected", "web=web-connected"]
assert_manifests_otherwise_unchanged = len(_tuned.manifests) == len(_implicit.manifests)

assert_tuned_not_noop = "noop_node_agents" not in _tuned and "noop_node_agents" not in _implicit

# A NodeAgent with no overrides changes nothing: the implicit agent is
# already the agent it declares. It is listed in output.noop_node_agents,
# which forge env render warns for.
_bare = _bundle(forge.ObservabilityAgent {}, [forge.NodeAgent {target = _gpu}])
assert_bare_is_implicit = _bare.manifests == _implicit.manifests and _bare.control_plane_secrets == _implicit.control_plane_secrets
assert_bare_is_noop = _bare.noop_node_agents == ["gpu"] and len(_agents(_bare)) == 2

# One override is enough to make it do something: not listed.
_memory = _bundle(forge.ObservabilityAgent {}, [forge.NodeAgent {target = _gpu, memory_limit_mib = 512}])
assert_override_not_noop = "noop_node_agents" not in _memory and _agent(_memory, "gpu").spec.memoryLimitMiB == 512

# With no destination no agent renders anywhere: nothing to warn about.
assert_no_destination_not_noop = "noop_node_agents" not in _bundle(forge.ObservabilityAgent {}, [forge.NodeAgent {target = _gpu}], False)

# Env-wide inventory on, off on gpu alone.
_inv = forge.ObservabilityAgent {kubernetes_inventory = forge.KubernetesInventory {}}
_off_on_gpu = _bundle(_inv, [forge.NodeAgent {target = _gpu, kubernetes_inventory = forge.KubernetesInventory {enabled = False}}])
assert_inventory_off_on_gpu = "kubernetesInventory" not in _agent(_off_on_gpu, "gpu").spec
assert_inventory_on_web = _agent(_off_on_gpu, "web").spec.kubernetesInventory.cluster == "web"

# Env-wide inventory disabled is the same as unset.
assert_disabled_is_unset = _bundle(forge.ObservabilityAgent {kubernetes_inventory = forge.KubernetesInventory {enabled = False}}, []).manifests == _implicit.manifests
