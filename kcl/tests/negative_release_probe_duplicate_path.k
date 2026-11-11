"""
Negative test — forge.Probe refuses a path named twice.
"""

# expect: names a path twice

import forge

_p = forge.Probe {
    paths = ["/ok", "/ok"]
}
