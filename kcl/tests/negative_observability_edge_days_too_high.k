"""
Negative test — ObservabilityRetention.edge_days above its cap.
"""

# expect: edge_days
# expect: 1..400

import forge

_r = forge.ObservabilityRetention {edge_days = 401}
