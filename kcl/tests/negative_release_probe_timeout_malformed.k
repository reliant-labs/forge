"""
Negative test — forge.Probe refuses a timeout that is not a number and s or m.
"""

# expect: Probe.timeout '10' must be

import forge

_p = forge.Probe {
    timeout = "10"
}
