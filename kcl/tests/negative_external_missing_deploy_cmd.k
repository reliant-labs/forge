"""
Negative test — External requires deploy_cmd. Missing it should fail
KCL's check block (External.deploy_cmd is required).
"""

import forge

_svc = forge.RenderedWorkload {
    name = "bad"
    deploy = forge.External {
        # deploy_cmd intentionally missing — should fail the check.
    }
}
