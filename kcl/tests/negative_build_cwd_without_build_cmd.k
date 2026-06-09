"""
Negative test — `build_cwd` without `build_cmd` must fail at KCL
validation. Setting a working directory for a non-existent command is
almost certainly a typo (probably forgot the build_cmd, or pasted only
half a block). The check raises so the user sees it early.
"""

import forge

_svc = forge.Service {
    name = "bad"
    image = "bad"
    build_cwd = "../somewhere"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5051"
    }
}
