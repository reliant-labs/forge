"""An MLCollector interval that is not a collector duration is refused."""
# expect: MLCollector.node_interval 'every minute' must be a duration like 60s
import forge

_s = forge.MLStack {
    cluster = forge.ClusterTarget {cluster = "gpu", namespace = "forge-ml"}
    collector = forge.MLCollector {node_interval = "every minute"}
}
