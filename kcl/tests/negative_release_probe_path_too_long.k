"""
Negative test — forge.Probe refuses a path over 256 characters.
"""

# expect: at most 256 characters

import forge

_p = forge.Probe {
    paths = ["/" + "a" * 256]
}
