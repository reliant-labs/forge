"""
Positive test — SimpleBackend's `network` modes are a MANIFEST
difference, not a label.

The three modes have to differ in what is actually emitted, or they are
documentation. This fixture renders one backend per mode into one bundle
and asserts the object sets differ in exactly the declared way:

  * "none"    — NO Service object. The workload cannot be dialed by name
                by anything. Note what this is NOT: it is addressability,
                not containment, and it emits no NetworkPolicy — egress
                is the namespace's concern (lib/netpol.k), not one
                workload's deploy block. Conflating the two would let a
                operator read "none" as a security boundary it is not, so
                the absence of a NetworkPolicy here is asserted rather
                than merely unmentioned.
  * "private" — a ClusterIP Service, and no domain.
  * "public"  — the SAME ClusterIP Service, plus a domain that rides the
                entity contract for the platform's ingress to key on.
                forge creates no DNS and no route from it.

`domain` is checked in BOTH directions — required for "public", rejected
otherwise — by negative_simple_backend_public_without_domain.k and
negative_simple_backend_private_with_domain.k.

Every mode still gets its Deployment: `network` governs reachability, not
whether the workload runs.
"""

import forge

_none = forge.RenderedWorkload {
    name = "worker"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/worker:v2"
        network = "none"
    }
}

_private = forge.RenderedWorkload {
    name = "internal-api"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/internal:v2"
        ports = [9000]
        network = "private"
    }
}

_public = forge.RenderedWorkload {
    name = "web"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/web:v2"
        ports = [8080]
        network = "public"
        domain = "web-acme.reliantapps.dev"
    }
}

_bundle = forge.Bundle {
    project = "hosted"
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_none, _private, _public]
}

_out = forge.render(_bundle)
# network_policies = False: the generic namespace NetworkPolicy bundle is
# an env-wide toggle and would otherwise mask assert_none_emits_no_netpol.
_manifests = forge.render_manifests(_bundle, "latest", {}, False)

_services = [m.metadata.name for m in _manifests if m.kind == "Service"]
_deployments = [m.metadata.name for m in _manifests if m.kind == "Deployment"]

# ── every mode runs ────────────────────────────────────────────────────
assert_all_three_deploy = sorted(_deployments) == ["internal-api", "web", "worker"]

# ── "none" emits NO Service ────────────────────────────────────────────
assert_none_has_no_service = "worker" not in _services
assert_reachable_modes_have_services = sorted(_services) == ["internal-api", "web"]
# ...and NO NetworkPolicy: "none" is addressability, not containment.
assert_none_emits_no_netpol = len([m for m in _manifests if m.kind == "NetworkPolicy"]) == 0

# ── "private" and "public" render the SAME kind of Service ─────────────
_private_svc = [m for m in _manifests if m.kind == "Service" and m.metadata.name == "internal-api"][0]
_public_svc = [m for m in _manifests if m.kind == "Service" and m.metadata.name == "web"][0]
assert_private_port = _private_svc.spec.ports[0].port == 9000
assert_public_port = _public_svc.spec.ports[0].port == 8080
# No `type` key on either => ClusterIP (the k8s default). A regression that
# made "public" a LoadBalancer would provision a billable cloud LB per
# hosted workload, which is the whole reason ingress is a platform concern.
assert_public_is_not_load_balancer = "type" not in _public_svc.spec
assert_private_is_not_load_balancer = "type" not in _private_svc.spec

# ── the domain rides the ENTITY contract, not a manifest ───────────────
_by_name = {s.name: s for s in _out.services}
assert_public_domain = _by_name["web"].deploy.domain == "web-acme.reliantapps.dev"
assert_private_no_domain = _by_name["internal-api"].deploy.domain == None
assert_none_no_domain = _by_name["worker"].deploy.domain == None
# forge synthesizes no ingress from `domain` — a project wanting a real
# route declares an ordinary forge.HTTPRoute naming the service.
assert_no_httproute_synthesized = len([m for m in _manifests if m.kind == "HTTPRoute"]) == 0

# ── no storage declared => no PVC ──────────────────────────────────────
assert_no_pvc_without_storage = len([m for m in _manifests if m.kind == "PersistentVolumeClaim"]) == 0
