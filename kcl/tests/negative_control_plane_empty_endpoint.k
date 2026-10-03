"""
Negative test — an EXPLICITLY empty ControlPlane.endpoint.

Omitting `endpoint` means Reliant cloud (it defaults to
forge.RELIANT_CLOUD_ENDPOINT). Setting it to "" is neither that nor another
control plane, so it must be rejected at load rather than render a
declaration forge would later treat as absent.
"""

# expect: ControlPlane.endpoint must not be empty

import forge

_bundle = forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {endpoint = "", organization = "4f3c2b1a-0000-4000-8000-000000000001"}
}

output = forge.render(_bundle)
