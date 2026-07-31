"""
Positive test — the NetworkPolicy bundle allows the egress a forge app
actually needs, and names the ingress source it actually has.

The gap this pins closed: `default-deny` on Ingress+Egress with allow
rules for DNS and same-namespace traffic ONLY. A forge app dials a
managed Postgres (5432, normally OUTSIDE the cluster), an OTLP
collector in another namespace, and HTTPS for JWKS / object storage /
third-party APIs — all three were blackholed, so every environment with
network policies on (staging and prod, by default) could not reach its
own database. The one ingress rule named `ingress-nginx`, a namespace
no forge cluster runs anything in.
"""

import forge.components as fc

_components = [
    fc.Server {
        name = "api"
        ports = [fc.ComponentPort {name = "http", port = 8080, expose = True}]
    }
]

_env = fc.ComponentEnv {
    namespace = "demo-prod"
    project = "demo"
    registry = "ghcr.io/demo"
    image_tag = "v1"
    network_policies = True
    egress_ports = [443, 5432]
    telemetry_namespace = "observability"
    ingress_namespace = "envoy-gateway-system"
}

manifests = fc.render_components(_components, _env)

_policies = [m for m in manifests if m.kind == "NetworkPolicy"]
_by_name = {p.metadata.name: p for p in _policies}

# The posture is still deny-by-default — the allowances are additive.
assert_default_deny = "default-deny" in _by_name and \
    _by_name["default-deny"].spec.policyTypes == ["Ingress", "Egress"]

# Egress to the database port must exist, or a managed Postgres is
# blackholed and the app cannot start.
_external = _by_name["allow-egress-external"]
_external_ports = [p.port for p in _external.spec.egress[0].ports]
assert_database_egress = 5432 in _external_ports
assert_https_egress = 443 in _external_ports
# Port-scoped, not allow-all: forge cannot know a managed database's
# address, but it can refuse to open every port to reach it.
assert_egress_is_port_scoped = len(_external.spec.egress[0].ports) == 2

# Telemetry reaches the collector's namespace on the OTLP ports.
_otlp = [p.port for p in _by_name["allow-egress-telemetry"].spec.egress[0].ports]
assert_otlp_egress = 4317 in _otlp and 4318 in _otlp

# Ingress is allowed from the namespace the Gateway controller actually
# runs in. `forge cluster up` installs Envoy Gateway; a rule naming a
# namespace nothing runs in is a rule that denies ingress.
# `from` is a keyword, so the rule's source list is read by subscript.
_ingress_from = _by_name["allow-ingress-controller"].spec.ingress[0]["from"][0]
assert_ingress_from_gateway_ns = \
    _ingress_from.namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "envoy-gateway-system"

# An UNDECLARED namespace omits its policy rather than rendering one
# against a namespace that may not exist.
_bare_env = fc.ComponentEnv {
    namespace = "demo-bare"
    project = "demo"
    network_policies = True
    egress_ports = []
}
_bare = [m.metadata.name for m in fc.render_components(_components, _bare_env) if m.kind == "NetworkPolicy"]
assert_undeclared_omits_policies = \
    "allow-ingress-controller" not in _bare and \
    "allow-egress-telemetry" not in _bare and \
    "allow-egress-external" not in _bare
assert_bare_keeps_the_baseline = "default-deny" in _bare and "allow-dns" in _bare and "allow-internal" in _bare
