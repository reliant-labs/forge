"""
Positive test — forge.WorkloadURL on a NON-hosted env resolves at RENDER time.

A self-hosted env declares every port and hostname itself, so the render
knows each workload's URL and lowers every reference to a literal:

  * a frontend's runtime_config reference to a HOST service resolves to
    http://localhost:<its declared listen port>;
  * a reference to a workload behind an HTTPRoute resolves to the route's
    URL through its Gateway listener (scheme, host, non-default port, path);
  * a SimpleBackend env var that names a dev-served frontend resolves to that
    frontend's port — and the manifest's forge.dev declaration record carries
    the VALUE, never the reference (pkg/deploy.Render refuses a reference);
  * literals pass through, including an explicit empty string.
"""

import forge
import forge.tiers

_bundle = forge.Bundle {
    project = "acme"
    env = "dev"
    cluster_target = forge.ClusterTarget { cluster = "k3d-acme", namespace = "acme-dev", registry = "localhost:5000" }
    services = [
        forge.RenderedWorkload {
            name = "api"
            deploy = forge.HostDeploy { listen_ports = [8085] }
        }
        forge.RenderedWorkload {
            name = "admin-api"
            deploy = forge.SimpleBackend {
                cluster = "k3d-acme"
                namespace = "acme-dev"
                spec = tiers.SimpleBackend {
                    image = "ghcr.io/acme/admin:v1"
                    ports = [8080]
                    env = [
                        tiers.EnvVar { name = "CORS_ORIGINS", workloadURL = forge.WorkloadURL { workload = "web" } }
                        tiers.EnvVar { name = "LOG_LEVEL", value = "info" }
                    ]
                }
            }
        }
    ]
    gateways = [forge.Gateway {
        name = "public"
        host = "acme.localhost"
        listeners = [forge.GatewayListener { name = "http", port = 8081, protocol = "HTTP" }]
    }]
    http_routes = [forge.HTTPRoute { name = "admin", gateway = "public", listener = "http", service = "admin-api", port = 8080, path = "/admin" }]
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        port = 5173
        runtime_config = {
            API_URL = forge.WorkloadURL { workload = "api" }
            ADMIN_URL = forge.WorkloadURL { workload = "admin-api" }
            APP_NAME = "acme"
            FEATURE_X = ""
        }
    }]
}

_out = forge.render(_bundle)
_fe = _out.frontends[0]
assert_api_url = _fe.runtime_config.API_URL == "http://localhost:8085"
assert_admin_url = _fe.runtime_config.ADMIN_URL == "http://acme.localhost:8081/admin"
assert_literal = _fe.runtime_config.APP_NAME == "acme"
assert_empty_literal = _fe.runtime_config.FEATURE_X == ""
# The declared shape rides along too (references kept), so tooling can show
# what was authored; nothing deploys it on a self-hosted env.
assert_spec_ref_kept = _fe.runtime_config_spec.API_URL.workloadURL.name == "api"

# The SimpleBackend's env reference is resolved in BOTH the entity contract
# and the manifest declaration record.
_sb = [s for s in _out.services if s.name == "admin-api"][0]
assert_entity_env_value = _sb.deploy.spec.env[0].value == "http://localhost:5173"
assert_entity_env_no_ref = "workloadURL" not in _sb.deploy.spec.env[0]
_record = [m for m in forge.render_manifests(_bundle, "t", {}, False) if m.kind == "SimpleBackend"][0]
assert_record_env_value = _record.spec.env[0].value == "http://localhost:5173"
assert_record_env_no_ref = "workloadURL" not in _record.spec.env[0]
assert_record_other_env = _record.spec.env[1].value == "info"
