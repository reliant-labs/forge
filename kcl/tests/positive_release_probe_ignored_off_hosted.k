"""
Positive test — a release probe is rendered only for a forge.OnHosted runtime
in an env the control plane probes. Everywhere else a declaration is ignored,
not refused, so one base declaration serves every env:

  * a frontend and a workload on forge.OnHost, declaring probes, in a hosted
    env: nothing;
  * a hosted frontend and workload in a local env (lifecycle): nothing;
  * the same in an ephemeral env (a preview): nothing.

(A hosted runtime with no control plane is refused by the Bundle itself, so
there is no such env to probe.)
"""

import forge
import forge.workloads as fw

_probe = forge.Probe {paths = ["/ok"]}

_api = fw.Workload {
    name = "api"
    image = "ghcr.io/acme/api:v1"
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
    release_probe = _probe
}

_web = forge.Frontend {
    name = "web"
    path = "web"
    type = "vite"
    public_dir = "dist"
}

_on_host = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    workloads = [_api | {command = ["./bin/api"], runtime = forge.OnHost {}}]
    frontends = [_web | {release_probe = _probe, runtime = forge.OnHost {}}]
})

_hosted_web = _web | {image = "ghcr.io/acme/web", runtime = forge.OnHosted {}}

_local = forge.render(forge.Bundle {
    project = "fixture"
    lifecycle = "local"
    control_plane = forge.ControlPlane {}
    workloads = [_api | {runtime = forge.OnHosted {}}]
    frontends = [_hosted_web]
})

_ephemeral = forge.render(forge.Bundle {
    project = "fixture"
    lifecycle = "ephemeral"
    control_plane = forge.ControlPlane {}
    workloads = [_api | {runtime = forge.OnHosted {}}]
    frontends = [_hosted_web]
})

assert_on_host_unprobed = "release_probes" not in _on_host
assert_local_unprobed = "release_probes" not in _local
assert_ephemeral_unprobed = "release_probes" not in _ephemeral
