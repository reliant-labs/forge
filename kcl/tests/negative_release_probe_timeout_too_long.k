"""
Negative test — forge.Probe refuses a timeout above 30s.
"""

# expect: Probe.timeout '31s' must be 1s to 30s

import forge

_p = forge.Probe {
    rate_per_min = 1
    timeout = "31s"
}
