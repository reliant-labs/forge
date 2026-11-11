"""
Negative test — forge.Probe refuses a path with a query (the prober owns the query string).
"""

# expect: has no query
# expect: /ok?x=1

import forge

_p = forge.Probe {
    paths = ["/ok?x=1"]
}
