"""
Negative test — product_events_days is at most 365.
"""

# expect: product_events_days must be 1..365

import forge

output = forge.render(forge.Bundle {
    project = "demo"
    observability = forge.Observability {
        local_port = 4318
        retention = forge.ObservabilityRetention {product_events_days = 366}
    }
})
