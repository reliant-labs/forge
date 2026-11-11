"""
Negative test — forge.Probe refuses a path that climbs out with '..'.
"""

# expect: '..'
# expect: /a/../admin

import forge

_p = forge.Probe {
    paths = ["/a/../admin"]
}
