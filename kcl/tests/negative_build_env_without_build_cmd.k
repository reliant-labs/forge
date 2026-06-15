"""
Negative test — `build_env` without `build_cmd` must fail at KCL
validation. Same reasoning as build_cwd: declaring env vars for a
non-existent command is a typo, not a legitimate shape.
"""

import forge

_svc = forge.Service {
    name = "bad"
    image = "bad"
    build_env = {"FOO" = "bar"}
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5051"
    }
}
