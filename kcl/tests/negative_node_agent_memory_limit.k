"""Negative test — a forge.NodeAgent's memory_limit_mib override is held to
the floor ObservabilityAgent.memory_limit_mib is."""
# expect: NodeAgent.memory_limit_mib must be >= 64
import forge

_agent = forge.NodeAgent {
    target = forge.ClusterTarget {cluster = "gpu", namespace = "forge-ml"}
    memory_limit_mib = 32
}
