"""
Positive test — `forge.Probe {enabled = False}` turns a probe off:

  * a hosted frontend's default probe is not rendered;
  * a hosted workload's declared probe is not rendered, and a disabled probe
    on a workload that exposes no port is not refused (there is nothing to
    probe, so nothing is misdeclared);
  * an env whose every probe is off renders no `release_probes` key at all.
"""

import forge
import forge.workloads as fw

_off = forge.Probe {enabled = False}

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    workloads = [
        fw.Workload {
            name = "api"
            image = "ghcr.io/acme/api:v1"
            ports = [fw.Port {name = "http", port = 8080, expose = True}]
            release_probe = _off
            runtime = forge.OnHosted {}
        }
        fw.Workload {
            name = "worker"
            image = "ghcr.io/acme/worker:v1"
            release_probe = _off
            runtime = forge.OnHosted {}
        }
    ]
    frontends = [forge.Frontend {
        name = "web"
        image = "ghcr.io/acme/web"
        path = "web"
        type = "vite"
        public_dir = "dist"
        release_probe = _off
        runtime = forge.OnHosted {}
    }]
})

assert_nothing_probed = "release_probes" not in output
