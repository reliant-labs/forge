"""
Negative test — Cluster.registry_mirror, when set, must be "inherit".
Any other value (a typo, a stray path) should fail the check block.
"""

import forge

_c = forge.Cluster {
    name = "workload"
    network = "k3d-cp"
    registry_mirror = "copy"  # not "inherit"
}
