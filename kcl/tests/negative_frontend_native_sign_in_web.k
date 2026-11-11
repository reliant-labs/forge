"""
Negative test — native_sign_in only means something on a `rn` frontend.
"""

# expect: native_sign_in applies only to a 'rn' frontend

import forge

output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = "local"
    frontends = [forge.Frontend {name = "web", path = "frontends/web", type = "vite", port = 5173, native_sign_in = False, runtime = forge.OnHost {}}]
})
