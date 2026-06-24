"""
Positive test — the re-exported Gateway/route builders and first-class
`cluster` attribution.

Exercises:

  * The public re-exports `forge.render_gateway` / `forge.render_http_route`
    / `forge.render_grpc_route` render the SAME manifest a consumer would
    otherwise hand-reimplement, AND the same one render_manifests emits.
  * Setting `cluster = "<name>"` on a Gateway / HTTPRoute / GRPCRoute
    stamps a `forge.dev/cluster: <name>` label on the rendered manifest —
    the first-class routing key forge's Go scoper reads to pin the
    manifest to that cluster.
  * A Gateway WITHOUT `cluster` carries no such label (unchanged routing).
"""

import forge

_public = forge.Gateway {
    name = "cp-public"
    host = "api.example.com"
    cluster = "control-plane"
    listeners = [
        forge.GatewayListener {
            name = "http"
            port = 18080
            protocol = "HTTP"
        }
        forge.GatewayListener {
            name = "grpc"
            port = 19190
            protocol = "H2C"
        }
    ]
}

_route_http = forge.HTTPRoute {
    name = "cp-api"
    gateway = "cp-public"
    listener = "http"
    service = "admin-server"
    port = 8080
    cluster = "control-plane"
}

_route_grpc = forge.GRPCRoute {
    name = "cp-daemon"
    gateway = "cp-public"
    listener = "grpc"
    service = "daemon-gateway"
    port = 19190
    cluster = "control-plane"
}

# A gateway with NO cluster attribution — must carry no routing label.
_unpinned = forge.Gateway {
    name = "shared"
    listeners = [
        forge.GatewayListener {
            name = "http"
            port = 28080
            protocol = "HTTP"
        }
    ]
}

# ── Re-exported builders render directly ────────────────────────────
_gw_manifests = forge.render_gateway(_public, "cp-ns")
_http_manifests = forge.render_http_route(_route_http, [_public], "cp-ns")
_grpc_manifests = forge.render_grpc_route(_route_grpc, [_public], "cp-ns")
_unpinned_manifests = forge.render_gateway(_unpinned, "cp-ns")

_routing_key = "forge.dev/cluster"

# ── Re-exports match render_manifests' output ───────────────────────
# render_manifests walks the same lib builders; the re-exported builder
# must produce a byte-identical Gateway manifest.
_k8s = forge.K8sCluster {
    cluster = "control-plane"
    namespace = "cp-ns"
    registry = "localhost:5050"
    ports = [8080]
}
_bundle = forge.Bundle {
    gateways = [_public]
    http_routes = [_route_http]
    grpc_routes = [_route_grpc]
}
_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = "latest"
    network_policies = False
}
_full = forge.render_manifests(_bundle, _env)
_full_gw = [m for m in _full if m.kind == "Gateway"][0]

# ── Invariants ──────────────────────────────────────────────────────

# Re-exported render_gateway emits exactly one Gateway.
assert_gw_count = len(_gw_manifests) == 1
assert_gw_kind = _gw_manifests[0].kind == "Gateway"
assert_gw_name = _gw_manifests[0].metadata.name == "cp-public"
assert_gw_ns = _gw_manifests[0].metadata.namespace == "cp-ns"

# The re-exported builder == the in-bundle render (same manifest).
assert_reexport_matches_bundle = _gw_manifests[0] == _full_gw

# cluster attribution stamps the routing label on every ingress kind.
assert_gw_cluster_label = _gw_manifests[0].metadata.labels[_routing_key] == "control-plane"
assert_http_cluster_label = _http_manifests[0].metadata.labels[_routing_key] == "control-plane"
assert_grpc_cluster_label = _grpc_manifests[0].metadata.labels[_routing_key] == "control-plane"

# The managed app label is still present (routing is additive).
assert_gw_app_label = _gw_manifests[0].metadata.labels["app.kubernetes.io/name"] == "cp-public"

# An unpinned gateway carries NO routing label — unchanged behaviour.
assert_unpinned_no_label = _routing_key not in _unpinned_manifests[0].metadata.labels
