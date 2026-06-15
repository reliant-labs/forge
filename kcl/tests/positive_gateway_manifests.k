"""
Positive test — Gateway API resources render to the correct
gateway.networking.k8s.io/v1 manifests.

Asserts that render_manifests emits a Gateway with the expected
listeners and an HTTPRoute with the expected parentRefs +
backendRefs.
"""

import forge

_k8s = forge.K8sCluster {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5050"
    ports = [8080]
}

_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "api"
            image = "example"
            deploy = _k8s
        }
    ]
    gateways = [
        forge.Gateway {
            name = "public"
            host = "api.example.com"
            listeners = [
                forge.GatewayListener {
                    name = "http"
                    port = 18080
                    protocol = "HTTP"
                }
            ]
        }
    ]
    http_routes = [
        forge.HTTPRoute {
            name = "api"
            gateway = "public"
            listener = "http"
            service = "api"
            port = 8080
            path = "/v1"
        }
    ]
}

_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = "latest"
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)

# Find the Gateway + HTTPRoute manifests by kind.
_gateways = [m for m in manifests if m.kind == "Gateway"]
_routes = [m for m in manifests if m.kind == "HTTPRoute"]

# ── Invariants ─────────────────────────────────────────────────────

# Exactly one Gateway + one HTTPRoute rendered.
assert_gw_count = len(_gateways) == 1
assert_route_count = len(_routes) == 1

# Gateway carries the expected metadata + spec.
assert_gw_api = _gateways[0].apiVersion == "gateway.networking.k8s.io/v1"
assert_gw_name = _gateways[0].metadata.name == "public"
assert_gw_ns = _gateways[0].metadata.namespace == "example-dev"
assert_gw_class = _gateways[0].spec.gatewayClassName == "traefik"
assert_gw_listener_port = _gateways[0].spec.listeners[0].port == 18080
assert_gw_listener_proto = _gateways[0].spec.listeners[0].protocol == "HTTP"

# HTTPRoute references the gateway by name + listener section.
assert_route_api = _routes[0].apiVersion == "gateway.networking.k8s.io/v1"
assert_route_parent_name = _routes[0].spec.parentRefs[0].name == "public"
assert_route_parent_section = _routes[0].spec.parentRefs[0].sectionName == "http"
assert_route_hostname = _routes[0].spec.hostnames[0] == "api.example.com"
assert_route_path_value = _routes[0].spec.rules[0].matches[0].path.value == "/v1"
assert_route_path_type = _routes[0].spec.rules[0].matches[0].path.type == "PathPrefix"
assert_route_backend_name = _routes[0].spec.rules[0].backendRefs[0].name == "api"
assert_route_backend_port = _routes[0].spec.rules[0].backendRefs[0].port == 8080
