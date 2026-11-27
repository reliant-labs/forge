"""
Negative test — ObservabilityRetention.edge_days below its floor.

Unset means the env's info-log retention; a stated value is 1..400.
"""

# expect: edge_days
# expect: 1..400

import forge

_r = forge.ObservabilityRetention {edge_days = 0}
