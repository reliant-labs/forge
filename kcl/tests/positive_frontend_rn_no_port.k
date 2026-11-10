"""
Positive test — a native ('rn') frontend that declares no `port` renders with
NO port, so `forge env up` prefers Expo's 8081 (where a native Debug build
looks for Metro); a browser frontend keeps the 3000 default; a declared port is
rendered verbatim for either.
"""
# kcl-args: env="dev"

import forge

output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = "local"
    frontends = [
        forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", runtime = forge.OnHost {}}
        forge.Frontend {name = "pinned", path = "frontends/pinned", type = "rn", port = 8090, runtime = forge.OnHost {}}
        forge.Frontend {name = "web", path = "frontends/web", type = "vite", runtime = forge.OnHost {}}
    ]
})

_by_name = {f.name: f for f in output.frontends}

assert_rn_declares_no_port = _by_name["mobile"].port == None
assert_rn_declared_port_is_kept = _by_name["pinned"].port == 8090
assert_browser_default_is_3000 = _by_name["web"].port == 3000
