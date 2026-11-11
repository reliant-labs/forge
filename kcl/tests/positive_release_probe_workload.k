"""
Positive test — a declared release probe is rendered as written, with every
default spelled out:

  * a hosted workload that exposes a port and declares a probe is probed with
    its paths (in order: the request rotation), method, rate, timeout in
    seconds and expect_status (sorted), source=user, kind=workload;
  * a hosted frontend's declaration replaces its default probe (source=user,
    kind=frontend), with the defaults it did not set written out;
  * a hosted workload that exposes a port but declares nothing is not probed.
"""

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    workloads = [
        fw.Workload {
            name = "api"
            image = "ghcr.io/acme/api:v1"
            ports = [fw.Port {name = "http", port = 8080, expose = True}]
            release_probe = forge.Probe {
                paths = ["/ok", "/flow-health"]
                method = "HEAD"
                rate_per_min = 6
                timeout = "5s"
                expect_status = ["204", "200"]
            }
            runtime = forge.OnHosted {}
        }
        fw.Workload {
            name = "billing"
            image = "ghcr.io/acme/billing:v1"
            ports = [fw.Port {name = "http", port = 8080, expose = True}]
            runtime = forge.OnHosted {}
        }
    ]
    frontends = [forge.Frontend {
        name = "web"
        image = "ghcr.io/acme/web"
        path = "web"
        type = "vite"
        public_dir = "dist"
        release_probe = forge.Probe {paths = ["/", "/pricing"]}
        runtime = forge.OnHosted {}
    }]
})

_by = {p.target: p for p in output.release_probes}

assert_targets = [p.target for p in output.release_probes] == ["api", "web"]
assert_workload_entry = _by.api == {
    "target": "api"
    "kind": "workload"
    "paths": ["/ok", "/flow-health"]
    "method": "HEAD"
    "rate_per_min": 6
    "timeout_seconds": 5
    "expect_status": ["200", "204"]
    "source": "user"
}
assert_frontend_declaration_replaces_default = _by.web == {
    "target": "web"
    "kind": "frontend"
    "paths": ["/", "/pricing"]
    "method": "GET"
    "rate_per_min": 4
    "timeout_seconds": 10
    "source": "user"
}
assert_undeclared_workload_unprobed = "billing" not in _by
