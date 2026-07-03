"""
Positive test — K8sCluster.app_protocols projects a Kubernetes
`appProtocol` onto the matching Service port. This closes the
backend-protocol-negotiation gap for h2c/gRPC backends that sit behind a
cloud Gateway controller (e.g. a daemon-gateway): the controller reads
`appProtocol: kubernetes.io/h2c` off the Service port to speak h2c to the
backend instead of HTTP/1.1.

app_protocols is keyed by port number (as a string — KCL dict keys must
be strings). It is OPTIONAL and ADDITIVE:

  * A port listed in app_protocols gets `appProtocol: <value>` rendered
    onto its Service port.
  * A port NOT listed renders exactly as before — `{name, port,
    targetPort, protocol}` with NO appProtocol field.
  * app_protocols unset/empty => every Service port is byte-identical to
    the pre-appProtocol render (the no-regression invariant).

appProtocol is a Service-port-only concept: it is NEVER projected onto
the Deployment's containerPorts.
"""

import forge

# K8sCluster declaring appProtocol on 8080 (h2c) but NOT 9090. Two ports
# so the same render exercises both the with- and without-appProtocol
# branches of the per-port comprehension.
_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5051"
}

# Per-service deploy WITH app_protocols on 8080 (h2c) but NOT 9090. Two
# ports so the same render exercises both the with- and
# without-appProtocol branches of the per-port comprehension.
_deploy_proto = _target.deploy | {
    ports = [8080, 9090]
    app_protocols = {"8080" = "kubernetes.io/h2c"}
}

# Bare per-service deploy — no app_protocols — for the no-regression baseline.
_deploy_bare = _target.deploy | {ports = [8080, 9090]}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [forge.RenderedWorkload {name = "gw", image = "gw-img", deploy = _deploy_proto}]
}
_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)

_svc = [m for m in _manifests if m.kind == "Service" and m.metadata.name == "gw"][0]
_deploy = [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == "gw"][0]

_port_8080 = [p for p in _svc.spec.ports if p.port == 8080][0]
_port_9090 = [p for p in _svc.spec.ports if p.port == 9090][0]

# ── Case (b): a port WITH app_protocol renders the field ────────────

# The 8080 Service port carries appProtocol AND keeps every original
# field unchanged.
assert_8080_app_protocol = _port_8080.appProtocol == "kubernetes.io/h2c"
assert_8080_unchanged = _port_8080.name == "port-8080" and _port_8080.port == 8080 \
    and _port_8080.targetPort == 8080 and _port_8080.protocol == "TCP"

# A port not listed in app_protocols omits the field — even on the same
# service that has appProtocol on another port.
assert_9090_no_app_protocol = _port_9090.appProtocol == Undefined

# Service-port-only: appProtocol is NOT projected onto containerPorts.
_cport_8080 = [p for p in _deploy.spec.template.spec.containers[0].ports if p.containerPort == 8080][0]
assert_container_port_no_app_protocol = _cport_8080.appProtocol == Undefined

# ── Case (a): no app_protocols => byte-identical to the old render ──

_bundle_bare = forge.Bundle {
    cluster_target = _target
    services = [forge.RenderedWorkload {name = "gw", image = "gw-img", deploy = _deploy_bare}]
}
_manifests_bare = forge.render_manifests(_bundle_bare, "abc1234", {}, False)
_svc_bare = [m for m in _manifests_bare if m.kind == "Service" and m.metadata.name == "gw"][0]

# Every port renders exactly the pre-appProtocol shape — the four
# original fields and nothing more.
assert_bare_ports_unchanged = _svc_bare.spec.ports == [
    {name = "port-8080", port = 8080, targetPort = 8080, protocol = "TCP"}
    {name = "port-9090", port = 9090, targetPort = 9090, protocol = "TCP"}
]
