"""
Forge KCL — Gateway API manifest builders.

Pure render functions that take typed Gateway / HTTPRoute / GRPCRoute
entities and emit the corresponding gateway.networking.k8s.io/v1
manifests. The render layer (render.k) walks these out of the Bundle
and inlines them into render_manifests' output list.

KCL package-identity note: same workaround as lib/services.k — lambda
params are typed `any` so the parent-package schemas are accepted as
duck-typed inputs. Callers are responsible for passing
already-validated entities.
"""

_managed_labels = lambda name: str -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = "forge-ingress"
    }
}

# Map a GatewayListener.protocol to the Gateway API spec value.
# H2C maps to Gateway API's HTTP protocol (HTTP/2 cleartext is
# expressed via the listener's underlying behavior; specific
# controllers — Traefik in particular — pick up h2c through the
# combination of HTTP + a GRPCRoute attachment).
_listener_protocol = lambda p: str -> str {
    "HTTPS" if p == "HTTPS" else "HTTP"
}

_render_listener = lambda l: any, gw_tls: any -> any {
    {
        name = l.name
        port = l.port
        protocol = _listener_protocol(l.protocol)
        if l.protocol == "HTTPS" and gw_tls:
            tls = {
                mode = "Terminate"
                certificateRefs = [
                    {
                        kind = "Secret"
                        name = gw_tls.secret_name
                    }
                ]
            }
        allowedRoutes = {namespaces = {"from" = "Same"}}
    }
}

# Render a Gateway resource. The optional cert-manager Certificate
# annotation lives on the Gateway when TLS is in cert_manager mode,
# mirroring cert-manager's gateway-shim path. mkcert mode skips the
# annotation — the Secret is populated host-side instead.
render_gateway = lambda gw: any, namespace: str -> [any] {
    _labels = _managed_labels(gw.name)
    _annotations = {
        "cert-manager.io/cluster-issuer" = gw.tls.cert_issuer
    } if gw.tls and gw.tls.mode == "cert_manager" else {}
    [{
        apiVersion = "gateway.networking.k8s.io/v1"
        kind = "Gateway"
        metadata = {
            name = gw.name
            namespace = namespace
            labels = _labels
            if _annotations:
                annotations = _annotations
        }
        spec = {
            gatewayClassName = gw.gateway_class_name
            listeners = [_render_listener(l, gw.tls) for l in gw.listeners]
        }
    }]
}

# Render a cert-manager Certificate for a Gateway with TLS. The
# Certificate populates the Secret referenced by the Gateway's HTTPS
# listener(s); cert-manager watches Certificate, issues against the
# named ClusterIssuer, and writes the resulting cert+key into the
# Secret the Gateway's listener references via tls.certificateRefs.
#
# mkcert-mode gateways skip this manifest entirely — `forge dev cluster
# up` populates the Secret host-side via the mkcert binary, so cert-
# manager doesn't need to know.
#
# When gw.host == "" the Certificate is emitted WITHOUT dnsNames; that
# config is invalid (cert-manager will surface the error at apply
# time) but failing the KCL render here would block render of every
# other manifest — defer to the cluster.
render_certificate = lambda gw: any, namespace: str -> [any] {
    _labels = _managed_labels(gw.name)
    [{
        apiVersion = "cert-manager.io/v1"
        kind = "Certificate"
        metadata = {
            name = "${gw.name}-tls"
            namespace = namespace
            labels = _labels
        }
        spec = {
            secretName = gw.tls.secret_name
            issuerRef = {
                name = gw.tls.cert_issuer
                kind = "ClusterIssuer"
                group = "cert-manager.io"
            }
            if gw.host:
                dnsNames = [gw.host]
        }
    }] if gw.tls and gw.tls.mode == "cert_manager" else []
}

# Effective hostname for a route — per-route override wins over the
# Gateway's default host.
_route_host = lambda route: any, gw: any -> str {
    route.host if route.host else gw.host
}

# Effective path: listener.path_prefix + route.path. Empty result
# collapses to "/" so the path-match isn't an empty string.
_route_path = lambda route: any, listener: any -> str {
    _prefix = listener.path_prefix
    _p = route.path
    _full = "${_prefix}${_p}" if _prefix or _p else ""
    _full if _full else "/"
}

# Resolve a route's target listener from the parent gateway, or None
# if the route names a listener that doesn't exist (KCL schema check
# catches that case earlier — this is defensive).
_lookup_listener = lambda gw: any, listener_name: str -> any {
    [l for l in gw.listeners if l.name == listener_name][0]
}

# Resolve a route's parent gateway by name. Returns None if the
# referenced gateway doesn't exist in this env (caller short-circuits).
_lookup_gateway = lambda gateways: [any], gateway_name: str -> any {
    _matches = [g for g in gateways if g.name == gateway_name]
    _matches[0] if _matches else None
}

render_http_route = lambda route: any, gateways: [any], namespace: str -> [any] {
    _gw = _lookup_gateway(gateways, route.gateway)
    _listener = _lookup_listener(_gw, route.listener) if _gw else None
    _host = _route_host(route, _gw) if _gw else ""
    _path = _route_path(route, _listener) if _listener else "/"
    _labels = _managed_labels(route.name)
    [{
        apiVersion = "gateway.networking.k8s.io/v1"
        kind = "HTTPRoute"
        metadata = {
            name = route.name
            namespace = namespace
            labels = _labels
        }
        spec = {
            parentRefs = [
                {
                    name = route.gateway
                    sectionName = route.listener
                }
            ]
            if _host:
                hostnames = [_host]
            rules = [
                {
                    matches = [
                        {
                            path = {
                                type = "PathPrefix"
                                value = _path
                            }
                        }
                    ]
                    backendRefs = [
                        {
                            name = route.service
                            port = route.port
                        }
                    ]
                }
            ]
        }
    }] if _gw and _listener else []
}

render_grpc_route = lambda route: any, gateways: [any], namespace: str -> [any] {
    _gw = _lookup_gateway(gateways, route.gateway)
    _listener = _lookup_listener(_gw, route.listener) if _gw else None
    _host = _route_host(route, _gw) if _gw else ""
    _labels = _managed_labels(route.name)
    # GRPCRoute path matches a gRPC service+method. Empty path means
    # match-everything; explicit path is interpreted as a method match
    # when prefixed with "/".
    [{
        apiVersion = "gateway.networking.k8s.io/v1"
        kind = "GRPCRoute"
        metadata = {
            name = route.name
            namespace = namespace
            labels = _labels
        }
        spec = {
            parentRefs = [
                {
                    name = route.gateway
                    sectionName = route.listener
                }
            ]
            if _host:
                hostnames = [_host]
            rules = [
                {
                    backendRefs = [
                        {
                            name = route.service
                            port = route.port
                        }
                    ]
                }
            ]
        }
    }] if _gw and _listener else []
}
