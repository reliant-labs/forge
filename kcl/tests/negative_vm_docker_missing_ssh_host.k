"""
Negative test — VMDocker requires ssh_host and image. Missing them
should fail KCL's check block.
"""

import forge

_svc = forge.Service {
    name = "bad"
    deploy = forge.VMDocker {
        # ssh_host and image both missing
    }
}
