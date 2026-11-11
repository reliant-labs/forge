"""
Negative test — forge.Probe refuses a rate above the 12 a minute forge accepts.
"""

# expect: rate_per_min 13 must be 1 to 12

import forge

_p = forge.Probe {
    rate_per_min = 13
}
