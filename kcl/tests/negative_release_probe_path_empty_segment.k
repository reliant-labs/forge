"""
Negative test — forge.Probe refuses a path with an empty segment: '//a' would
read as a host to some URL parsers.
"""

# expect: '//'
# expect: //evil.example

import forge

_p = forge.Probe {
    paths = ["//evil.example"]
}
