"""
Negative test — forge.Probe refuses a percent-encoded path: every client must
read it the same way.
"""

# expect: percent-encoding
# expect: /%2e%2e/admin

import forge

_p = forge.Probe {
    paths = ["/%2e%2e/admin"]
}
