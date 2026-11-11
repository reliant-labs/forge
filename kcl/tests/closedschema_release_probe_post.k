"""
Closed schema — forge.Probe.method is GET or HEAD. A probe is requested many
times a minute, forever, by the platform, so a method that can change state
(POST, PUT, DELETE) is not a value the schema has.
"""

# expect: POST
# expect: GET

import forge

_p = forge.Probe {
    method = "POST"
}
