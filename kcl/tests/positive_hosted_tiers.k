"""
Positive test — a HOSTED env: tiers declared with NO target coordinates.

A Bundle that declares `control_plane` deploys its tiers through that control
plane, which allocates the namespace and owns the cluster. So:

  1. SimpleBackend and ManagedDatabase carry no cluster/namespace.
  2. The ENTITY output still carries the specs (the hosted provider publishes
     them) and the control_plane declaration.
  3. render_manifests is EMPTY: forge applies nothing to any cluster for a
     hosted env — the platform renders the CR.
"""

import forge
import forge.tiers

_bundle = forge.Bundle {
    project = "acme"
    env = "hosted"
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.SimpleBackend {
            spec = tiers.SimpleBackend {
                image = "ghcr.io/acme/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
                ports = [8080]
                network = "public"
            }
        }
    }]
    databases = [forge.ManagedDatabase { name = "orders" }]
}

_out = forge.render(_bundle)
_manifests = forge.render_manifests(_bundle, "t", {}, False)

assert_entity_type = _out.services[0].deploy.type == "simple-backend"
assert_entity_no_cluster = not _out.services[0].deploy.cluster
assert_entity_no_namespace = not _out.services[0].deploy.namespace
assert_entity_spec_port = _out.services[0].deploy.spec.ports == [8080]
assert_db_entity = _out.databases[0].name == "orders"
assert_control_plane = _out.control_plane.endpoint == "https://api.example.com"
assert_no_local_manifests = len(_manifests) == 0
