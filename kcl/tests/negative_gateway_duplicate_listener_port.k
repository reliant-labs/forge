"""
Negative test — two listeners on a Gateway sharing the same port
fail the schema check. The port→listener mapping must be 1:1 since
a single host port can't bind two protocols.
"""

import forge

_gw = forge.Gateway {
    name = "public"
    listeners = [
        forge.GatewayListener {
            name = "http"
            port = 18080
            protocol = "HTTP"
        }
        forge.GatewayListener {
            name = "grpc"
            port = 18080
            protocol = "H2C"
        }
    ]
}
