"""
Negative test — a Gateway with no listeners fails the schema check.
"""

import forge

_gw = forge.Gateway {
    name = "empty"
    listeners = []
}
