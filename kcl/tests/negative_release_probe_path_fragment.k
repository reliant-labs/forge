"""
Negative test — forge.Probe refuses a path with a fragment.
"""

# expect: fragment
# expect: /ok#top

import forge

_p = forge.Probe {
    paths = ["/ok#top"]
}
