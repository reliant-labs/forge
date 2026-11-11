"""
Negative test — forge.Probe refuses a rate of zero (turn a probe off with enabled = False).
"""

# expect: rate_per_min 0 must be 1 to 12

import forge

_p = forge.Probe {
    rate_per_min = 0
}
