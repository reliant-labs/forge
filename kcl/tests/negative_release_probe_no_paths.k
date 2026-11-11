"""
Negative test — forge.Probe refuses no path at all.
"""

# expect: name 1 to 5 paths, not 0

import forge

_p = forge.Probe {
    paths = []
}
