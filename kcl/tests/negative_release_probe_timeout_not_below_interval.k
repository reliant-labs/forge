"""
Negative test — forge.Probe refuses a timeout as long as one interval: at 12 a minute a 5s request would overlap the next.
"""

# expect: shorter than one interval

import forge

_p = forge.Probe {
    rate_per_min = 12
    timeout = "5s"
}
