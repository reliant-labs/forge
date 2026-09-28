"""
Negative test — the LOCAL shape without its control plane: host + compose
workloads with forge.HostedSecrets but no control_plane. A LOCAL env's values
live in (and are pulled from) its control plane, so without one there is
nowhere to read them from. Refused at load, not at `forge env up`.
"""

# expect: secret_provider = forge.HostedSecrets {} requires control_plane

import forge
import forge.workloads as fw

_bundle = forge.Bundle {
    runtime = forge.OnHost {runner = "go-run"}
    workloads = [
        fw.Workload {name = "api", build = forge.GoBuild {cmd = "./cmd/api"}, args = ["server"]}
        fw.Workload {name = "postgres", runtime = forge.OnCompose {service = "postgres"}}
    ]
    secret_provider = forge.HostedSecrets {}
}

output = forge.render(_bundle)
