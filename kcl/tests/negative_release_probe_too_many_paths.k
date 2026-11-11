"""
Negative test — forge.Probe refuses more than five paths.
"""

# expect: name 1 to 5 paths, not 6

import forge

_p = forge.Probe {
    paths = ["/a", "/b", "/c", "/d", "/e", "/f"]
}
