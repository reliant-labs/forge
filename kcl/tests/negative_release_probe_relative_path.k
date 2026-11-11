"""
Negative test — forge.Probe refuses a path that does not start with '/'.
"""

# expect: each path starts with '/'
# expect: ok

import forge

_p = forge.Probe {
    paths = ["/", "ok"]
}
