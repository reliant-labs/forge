"""
Negative test — HostDeploy.runner must be one of the four enum
values. Picking "bogus" should fail KCL's `check` block.

The test runner expects this file to produce a non-zero exit code
when run with `kcl run`.
"""

import forge

_svc = forge.Service {
    name = "bad-runner"
    image = "x"
    deploy = forge.HostDeploy {
        runner = "bogus-runner"
    }
}
