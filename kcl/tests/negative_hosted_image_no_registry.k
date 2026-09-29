"""
Negative test — a workload bound to forge.OnHosted must declare an image that
names its own registry host, for the same reason a cluster workload must: the
registry lives on the image.

The runbook differs, though. A hosted env could one day inherit a default from
the control plane's advertised image push base — but that base is not pushable
yet (control-plane ADR 0003 / #334), so forge does not invent one, and the
message says so rather than implying the author forgot a field that exists.
"""

# expect: names no registry host

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "app"
    control_plane = forge.ControlPlane {endpoint = "https://cp.example.com"}
    workloads = [fw.Workload {
        name = "api"
        image = "api"
        ports = [fw.Port {name = "http", port = 8080}]
        runtime = forge.OnHosted {}
    }]
})
