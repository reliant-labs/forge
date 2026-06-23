"""
Positive tests for forge.Cluster — declared k3d clusters forge ensures
exist before deploying.

An env declares a LIST of clusters (Bundle.clusters). `forge up`
reconciles each (create-if-absent, no-op if present). Ownership is
IMPLICIT: a secondary cluster names the owner's network via `network`
and reuses its registry via `registry_mirror = "inherit"`. There is no
"primary" field anywhere.

Exercises:

  * An owner cluster (no network / no registry_mirror) projects with
    its name + defaulted servers/agents and null optional fields.
  * A secondary cluster projects network + registry_mirror="inherit" +
    api_port through to the JSON contract.
  * A config-driven cluster projects its config path.
  * No clusters declared => `clusters` is an EMPTY list (stable key).
  * Clusters NEVER appear in the k8s manifest stream
    (forge.render_manifests).
"""

import forge

_owner = forge.Cluster {
    name = "cp"
    api_port = 6443
}

_secondary = forge.Cluster {
    name = "workload"
    network = "k3d-cp"
    registry_mirror = "inherit"
    api_port = 6444
    agents = 2
}

_from_config = forge.Cluster {
    name = "configured"
    config = "deploy/k3d.workload.yaml"
}

_bundle = forge.Bundle {
    clusters = [_owner, _secondary, _from_config]
    services = [forge.Service {
        name = "api"
        image = "api:dev"
        deploy = forge.K8sCluster {
            cluster = "k3d-cp"
            namespace = "dev"
            registry = "localhost:5051"
        }
    }]
}

_no_clusters_bundle = forge.Bundle {
    services = []
}

_env = forge.RenderEnv {
    cluster = forge.K8sCluster {
        cluster = "k3d-cp"
        namespace = "dev"
        registry = "localhost:5051"
    }
}

output = forge.render(_bundle)
output_none = forge.render(_no_clusters_bundle)
manifests = forge.render_manifests(_bundle, _env)

# ── Invariants ─────────────────────────────────────────────────────

# Three clusters projected, in declaration order.
assert_count = len(output.clusters) == 3
assert_order = output.clusters[0].name == "cp" and \
    output.clusters[1].name == "workload" and \
    output.clusters[2].name == "configured"

# Owner cluster: defaulted servers=1, agents=0; network/registry_mirror null.
assert_owner_servers = output.clusters[0].servers == 1
assert_owner_agents = output.clusters[0].agents == 0
assert_owner_network_null = output.clusters[0].network == None
assert_owner_mirror_null = output.clusters[0].registry_mirror == None
assert_owner_api_port = output.clusters[0].api_port == 6443

# Secondary cluster: implicit ownership via network + registry_mirror.
assert_secondary_network = output.clusters[1].network == "k3d-cp"
assert_secondary_mirror = output.clusters[1].registry_mirror == "inherit"
assert_secondary_agents = output.clusters[1].agents == 2
assert_secondary_api_port = output.clusters[1].api_port == 6444

# Config-driven cluster: config path projects through.
assert_config = output.clusters[2].config == "deploy/k3d.workload.yaml"

# No clusters declared => stable empty list (not null garbage).
assert_none_empty = output_none.clusters == []

# Clusters are NEVER manifests — they're a host-side k3d reconcile, not
# a k8s object. No rendered manifest carries a Cluster kind.
assert_no_cluster_manifest = len([m for m in manifests if m.kind == "Cluster"]) == 0
