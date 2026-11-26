"""
Closed-schema test — forge.Adopt is an inventory assertion and has no scrape
fields.

`port` (and `selector`, `allow`, `service_name`) are what a reader carrying
an older draft reaches for. A scrape of an adopted component is a plain
Observability.telemetry entry (forge.Telemetry), not a field of the Adopt.
"""

# expect: port
# expect: Adopt

import forge

_a = forge.Adopt {kind = "dcgm-exporter", namespace = "gpu-operator", port = 9400}
