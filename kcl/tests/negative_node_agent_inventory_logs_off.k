"""Negative test — a forge.NodeAgent's own kubernetes_inventory ships logs, so
it is refused with logs switched off, as the env-wide one is
(negative_observability_kubernetes_inventory_logs_off.k)."""
# expect: Observability.node_agents on cluster gpu declare kubernetes_inventory
import forge

_obs = forge.Observability {
    destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
    signals = forge.ObservabilitySignals {logs = False}
    node_agents = [forge.NodeAgent {
        target = forge.ClusterTarget {cluster = "gpu", namespace = "forge-ml"}
        kubernetes_inventory = forge.KubernetesInventory {}
    }]
}
