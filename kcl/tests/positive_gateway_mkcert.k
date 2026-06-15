"""
Positive test — mkcert-mode Gateway emits the Gateway resource but
NOT a cert-manager Certificate. The Secret is populated host-side by
`forge dev cluster up`, so cert-manager doesn't need to know about
the TLS at all (no annotation, no Certificate manifest).
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
        # mkcert Gateway — should emit Gateway, no Certificate.
        forge.Gateway {
            name = "public"
            host = "example.localhost"
            listeners = [
                forge.GatewayListener {
                    name = "https"
                    port = 18443
                    protocol = "HTTPS"
                }
            ]
            tls = forge.GatewayTLS {
                mode = "mkcert"
                secret_name = "example-dev-tls"
            }
        }
    ]
}

_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = "latest"
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)

_gateways = [m for m in manifests if m.kind == "Gateway"]
_certs = [m for m in manifests if m.kind == "Certificate"]

# JSON contract projection — render(bundle) emits the TLS block with mode.
contract = forge.render(_bundle)
_tls = contract.gateways[0].tls

# ── Invariants ─────────────────────────────────────────────────────

# Gateway resource present, NO Certificate.
assert_gw_count = len(_gateways) == 1
assert_no_cert = len(_certs) == 0

# Listener carries the TLS secretRef — Gateway API still terminates TLS
# in-cluster; only the cert ORIGIN differs from cert-manager mode.
_listener = _gateways[0].spec.listeners[0]
assert_listener_proto = _listener.protocol == "HTTPS"
assert_listener_tls_mode = _listener.tls.mode == "Terminate"
assert_listener_tls_secret = _listener.tls.certificateRefs[0].name == "example-dev-tls"

# mkcert Gateway has NO cert-manager annotation — the manifest exists
# without the annotations key, or it exists but lacks the cert-manager
# entry. The render path emits an empty dict; metadata.get("annotations")
# returns None/{} either way.
assert_no_cert_manager_annotation = not _gateways[0].metadata.get("annotations")

# JSON contract surfaces the mode so audit / doctor tooling can
# distinguish mkcert from cert-manager Gateways without re-deriving.
assert_tls_mode = _tls.mode == "mkcert"
assert_tls_secret = _tls.secret_name == "example-dev-tls"
# cert_issuer should project as null (KCL emits None → JSON null) when
# unset on mkcert gateways.
assert_tls_no_issuer = _tls.cert_issuer == None
