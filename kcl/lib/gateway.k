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

# cluster_routing_label is the dedicated wire key the Go multi-cluster
# scoper (internal/cluster.ScopeManifestsToGroup) reads to route a
# manifest to ONE cluster directly. When a Gateway / HTTPRoute /
# GRPCRoute carries `cluster = <forge.Cluster>`, the builder stamps
# `forge.dev/cluster: <context>` (the DENORMALIZED kubectl context,
# `k3d-<name>`) on the rendered manifest's labels; the scoper keeps that
# manifest ONLY on the named cluster and drops it from every other. This
# is the first-class replacement for the older label-piggyback trick
# (stamping `app.kubernetes.io/name = <some service>` so the manifest
# routes via that service's group). A manifest WITHOUT this label still
# routes by `app.kubernetes.io/name` exactly as before, so existing
# consumers are unaffected.
#
# The `cluster` field is a forge.Cluster REFERENCE (not a bare name), so
# the builder reads its `.context` — this is what removed the
# name-vs-context footgun (a consumer once had to pass `k3d-control-plane`
# as a string, which only worked by coincidence). `_cluster_context`
# extracts that context from the (duck-typed) Cluster instance, or "" when
# unset; `_cluster_label` stamps the label with it.
_cluster_routing_label_key = "forge.dev/cluster"

_cluster_context = lambda cluster: any -> str {
    cluster.context if cluster else ""
}

_cluster_label = lambda context: str -> {str: str} {
    {"${_cluster_routing_label_key}" = context} if context else {}
}

# Map a GatewayListener.protocol to the Gateway API spec value.
# H2C maps to Gateway API's HTTP protocol (HTTP/2 cleartext is
# expressed via the listener's underlying behavior; Envoy Gateway
# picks up h2c through the combination of HTTP + a GRPCRoute
# attachment and serves native gRPC bidi streams on it).
_listener_protocol = lambda p: str -> str {
    "HTTPS" if p == "HTTPS" else "HTTP"
}

_render_listener = lambda l: any, gw_tls: any, gw_host: str -> any {
    {
        name = l.name
        port = l.port
        protocol = _listener_protocol(l.protocol)
        # An HTTPS listener MUST carry a `hostname` when the gateway has a
        # default host: the cert-manager gateway-shim iterates the listener
        # blocks to build per-host Certificates and REJECTS the whole
        # gateway config ("spec.listeners[*].hostname: Required value: the
        # hostname cannot be empty") for an HTTPS listener with no hostname,
        # which in turn wedges the cloud Gateway controller's reconcile.
        # It is also what the LB uses for SNI cert selection. Plaintext HTTP
        # listeners stay hostname-less (they serve all hosts, e.g. the ACME
        # HTTP-01 solver route).
        if l.protocol == "HTTPS" and gw_host:
            hostname = gw_host
        # gke_certmap terminates TLS via a GCP Certificate Manager map
        # (stamped as an annotation on the Gateway, see render_gateway),
        # NOT a cluster Secret — so the HTTPS listener carries NO tls
        # block (no certificateRefs, no mode). The other modes reference
        # the Secret by name via certificateRefs.
        if l.protocol == "HTTPS" and gw_tls and gw_tls.mode != "gke_certmap":
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

# Render a Gateway resource.
#
# In cert_manager mode forge emits an EXPLICIT cert-manager Certificate
# (see render_certificate below) with full control over dnsNames /
# issuerRef. It therefore does NOT also stamp the
# `cert-manager.io/cluster-issuer` annotation on the Gateway: that
# annotation drives cert-manager's gateway-shim, which would auto-create
# a SECOND Certificate targeting the same Secret. With both present the
# shim's cert and forge's cert race two parallel ACME orders for one
# Secret — redundant churn at best, and the shim's stricter listener
# validation (every HTTPS listener must carry a hostname) can wedge the
# whole gateway reconcile. One mechanism only: the explicit Certificate.
#
# mkcert mode emits no Certificate and no annotation — the Secret is
# populated host-side by `forge dev cluster up`.
#
# gke_certmap mode stamps `networking.gke.io/certmap: <certmap>` on the
# Gateway: the GKE Gateway controller reads that annotation and binds the
# named GCP Certificate Manager map to the Gateway, which terminates TLS
# and serves the cert for each route hostname. No Certificate and no
# Secret are involved (the listener carries no tls block — see
# _render_listener).
render_gateway = lambda gw: any, namespace: str -> [any] {
    _labels = _managed_labels(gw.name) | _cluster_label(_cluster_context(gw.cluster))
    _annotations = {
        "networking.gke.io/certmap" = gw.tls.certmap
    } if gw.tls and gw.tls.mode == "gke_certmap" else {}
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
            # Pin the Gateway to specific load-balancer addresses when set
            # (e.g. a GKE reserved static IP by reservation name via a
            # NamedAddress) so the IP survives delete/recreate. Omitted
            # entirely when empty — emitting `addresses: []` would churn
            # existing gateways and let the controller assign ephemerally.
            if gw.addresses:
                addresses = [{type = a.type, value = a.value} for a in gw.addresses]
            listeners = [_render_listener(l, gw.tls, gw.host) for l in gw.listeners]
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
    _labels = _managed_labels(route.name) | _cluster_label(_cluster_context(route.cluster))
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
    _labels = _managed_labels(route.name) | _cluster_label(_cluster_context(route.cluster))
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
