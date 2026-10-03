"""
Negative test — a ManagedDatabase bound to the HOSTED runtime names a
namespace. The control plane allocates it; a namespace here would be a second,
unverified opinion about where the data lands. Refused, per database.
"""

# expect: ManagedDatabase.namespace must NOT be set

import forge

_bundle = forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {endpoint = "https://api.example.com", organization = "4f3c2b1a-0000-4000-8000-000000000001"}
    databases = [forge.ManagedDatabase {
        name = "orders"
        namespace = "acme-prod"
        runtime = forge.OnHosted {}
    }]
}

output = forge.render(_bundle)
