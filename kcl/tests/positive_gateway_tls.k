"""
Positive test — Gateway with TLS emits both a Gateway resource (with
cert-manager.io/cluster-issuer annotation) AND a cert-manager
Certificate resource that fills the named Secret. Gateways without
TLS emit no Certificate.
"""

import forge

_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "api"
            image = "example"
            deploy = forge.K8sCluster {
                cluster = "k3d-example"
                namespace = "example-prod"
                registry = "localhost:5050"
                ports = [8080]
            }
        }
    ]
    gateways = [
        # TLS-bearing Gateway — should emit a Certificate.
        forge.Gateway {
            name = "public"
            host = "api.example.com"
            listeners = [
                forge.GatewayListener {
                    name = "https"
                    port = 443
                    protocol = "HTTPS"
                }
            ]
            tls = forge.GatewayTLS {
                cert_issuer = "letsencrypt-prod"
                secret_name = "api-tls"
            }
        },
        # No-TLS Gateway — should NOT emit a Certificate.
        forge.Gateway {
            name = "internal"
            host = "internal.example.com"
            listeners = [
                forge.GatewayListener {
                    name = "http"
                    port = 18081
                    protocol = "HTTP"
                }
            ]
        }
    ]
}

_env = forge.RenderEnv {
    namespace = "example-prod"
    image_registry = "localhost:5050"
    image_tag = "latest"
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)

_gateways = [m for m in manifests if m.kind == "Gateway"]
_certs = [m for m in manifests if m.kind == "Certificate"]

# ── Invariants ─────────────────────────────────────────────────────

# Two Gateways rendered, exactly one Certificate (only the TLS one).
assert_gw_count = len(_gateways) == 2
assert_cert_count = len(_certs) == 1

# Gateway with TLS carries the cert-manager cluster-issuer annotation.
_public_gw = [g for g in _gateways if g.metadata.name == "public"][0]
assert_gw_annotation = _public_gw.metadata.annotations["cert-manager.io/cluster-issuer"] == "letsencrypt-prod"

# Gateway without TLS has no cert-manager annotation.
_internal_gw = [g for g in _gateways if g.metadata.name == "internal"][0]
assert_internal_no_annotation = not _internal_gw.metadata.get("annotations")

# Certificate shape — apiVersion, naming, issuerRef, secretName, dnsNames.
_cert = _certs[0]
assert_cert_api = _cert.apiVersion == "cert-manager.io/v1"
assert_cert_name = _cert.metadata.name == "public-tls"
assert_cert_namespace = _cert.metadata.namespace == "example-prod"
assert_cert_secret = _cert.spec.secretName == "api-tls"
assert_cert_issuer_name = _cert.spec.issuerRef.name == "letsencrypt-prod"
assert_cert_issuer_kind = _cert.spec.issuerRef.kind == "ClusterIssuer"
assert_cert_issuer_group = _cert.spec.issuerRef.group == "cert-manager.io"
assert_cert_dns = _cert.spec.dnsNames[0] == "api.example.com"

# Forge-management labels propagate to the Certificate.
assert_cert_label_managed = _cert.metadata.labels["app.kubernetes.io/managed-by"] == "forge"
assert_cert_label_partof = _cert.metadata.labels["app.kubernetes.io/part-of"] == "forge-ingress"
