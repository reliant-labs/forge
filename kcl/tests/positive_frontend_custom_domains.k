"""
Positive test — custom domains on an OnHosted frontend and on a workload's
exposed port: hounders' real declaration.

The frontend's `domains` and the port's `domains` are the SAME field on the
two halves of one env. Both survive the render verbatim, in the order
declared: an apex and its `www` are two names, and each is verified
separately by the platform.
"""

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "hounders"
    control_plane = forge.ControlPlane {}
    workloads = [fw.Workload {
        name = "membership"
        image = "ghcr.io/hounders/membership:v1"
        ports = [fw.Port {name = "http", port = 8080, expose = True, domains = ["api.hounders.club"]}]
        runtime = forge.OnHosted {}
    }]
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        type = "vite"
        domains = ["hounders.club", "www.hounders.club"]
        runtime = forge.OnHosted {}
    }]
})

_f = output.frontends[0]
assert_frontend_domains = _f.domains == ["hounders.club", "www.hounders.club"]
_p = output.workloads[0].spec.ports[0]
assert_port_domains = _p.domains == ["api.hounders.club"]
# A frontend that declares none renders an empty list, never a missing key:
# "no custom domain" and "dropped on the floor" must not look alike.
assert_default_empty = forge.render(forge.Bundle {
    project = "hounders"
    control_plane = forge.ControlPlane {}
    frontends = [forge.Frontend {name = "plain", path = "plain", runtime = forge.OnHosted {}}]
}).frontends[0].domains == []
