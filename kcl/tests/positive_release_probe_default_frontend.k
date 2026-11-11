"""
Positive test — a hosted env probes every hosted static frontend with NO
declaration, and no workload unless it declares a probe.

  * a hosted frontend gets the default probe of its root: GET /, 4 a minute,
    10s, source=default, every default written out and expect_status left out
    (2xx and 3xx);
  * a frontend's base_path is honoured: `/admin` (or `/admin/`) is probed at
    `/admin/`;
  * a hosted workload that exposes a port but declares nothing is not probed
    (release probes are opt-in for workloads, unlike uptime checks);
  * the list is sorted by target.
"""

import forge
import forge.workloads as fw

_site = forge.Frontend {
    name = "web"
    image = "ghcr.io/acme/web"
    path = "web"
    type = "vite"
    public_dir = "dist"
    runtime = forge.OnHosted {}
}

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    workloads = [fw.Workload {
        name = "api"
        image = "ghcr.io/acme/api:v1"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
        runtime = forge.OnHosted {}
    }]
    frontends = [
        _site
        _site | {name = "admin", path = "admin", base_path = "/admin"}
        _site | {name = "docs", path = "docs", base_path = "/docs/"}
    ]
})

_by = {p.target: p for p in output.release_probes}

assert_sorted_frontends_only = [p.target for p in output.release_probes] == ["admin", "docs", "web"]
assert_default_entry = _by.web == {
    "target": "web"
    "kind": "frontend"
    "paths": ["/"]
    "method": "GET"
    "rate_per_min": 4
    "timeout_seconds": 10
    "source": "default"
}
assert_base_path_honoured = _by.admin.paths == ["/admin/"] and _by.admin.source == "default"
assert_base_path_trailing_slash_once = _by.docs.paths == ["/docs/"]
assert_undeclared_workload_unprobed = "api" not in _by
