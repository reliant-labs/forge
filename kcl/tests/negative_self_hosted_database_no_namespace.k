"""
Negative test — a ManagedDatabase on a SELF-HOSTED env omits its namespace.
"""

import forge

_bundle = forge.Bundle {
    services = []
    databases = [forge.ManagedDatabase { name = "orders" }]
}

output = forge.render(_bundle)
