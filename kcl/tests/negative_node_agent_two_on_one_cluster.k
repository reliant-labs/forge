"""
Negative test — two forge.NodeAgents target one cluster. A cluster runs one
node agent, so it has one declaration: the same target twice, and two
targets of one kube context that differ only in namespace, are both refused
(nothing could say whose overrides win).
"""
# kcl-args: case=same
# kcl-args: case=namespace
# expect: Observability.node_agents declares two forge.NodeAgents on cluster gpu:

import forge

_case = option("case", default="same")
_gpu = forge.ClusterTarget {cluster = "gpu", namespace = "forge-ml"}
output = forge.render(forge.Bundle {
    project = "demo"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        node_agents = [
            forge.NodeAgent {target = _gpu, memory_limit_mib = 512}
            forge.NodeAgent {
                target = _gpu if _case == "same" else forge.ClusterTarget {cluster = "gpu", namespace = "monitoring"}
                kubernetes_inventory = forge.KubernetesInventory {}
            }
        ]
    }
})
