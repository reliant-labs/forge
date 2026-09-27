"""
Positive test — forge.WorkloadURL on a HOSTED env stays a REFERENCE.

The control plane allocates hostnames, so nothing in the render knows a
workload's URL. The reference is carried into the wire shape unchanged —
the StaticSite spec's runtimeConfig and the SimpleBackend env's workloadURL —
for the control plane to resolve. The resolved document is EMPTY: forge
writes no config.js for a hosted StaticSite (the operator does, after every
sync), which is what lets one release artifact promote across envs.
"""

import forge
import forge.tiers

_bundle = forge.Bundle {
    project = "acme"
    env = "prod"
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.SimpleBackend {
            spec = tiers.SimpleBackend {
                image = "ghcr.io/acme/api:v1"
                ports = [8080]
                network = "public"
                env = [tiers.EnvVar { name = "CORS_ORIGINS", workloadURL = forge.WorkloadURL { workload = "web" } }]
            }
        }
    }]
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        runtime_config = {
            API_URL = forge.WorkloadURL { workload = "api" }
            APP_NAME = "acme"
        }
        deploy = forge.StaticSite { public_dir = "dist" }
    }]
}

_out = forge.render(_bundle)
_fe = _out.frontends[0]
assert_ref_kept = _fe.runtime_config_spec.API_URL == {"workloadURL": {"name": "api"}}
assert_literal_wire = _fe.runtime_config_spec.APP_NAME == {"value": "acme"}
assert_nothing_resolved = len(_fe.runtime_config) == 0
_env = _out.services[0].deploy.spec.env[0]
assert_env_ref_kept = _env.workloadURL.name == "web"
assert_env_no_value = "value" not in _env
# A hosted env renders no local manifests at all.
assert_no_manifests = len(forge.render_manifests(_bundle, "t", {}, False)) == 0
