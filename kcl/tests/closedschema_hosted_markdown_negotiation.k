"""
Closed schema — markdown_negotiation is a Frontend field, never OnHosted's.

forge.OnHosted is the runtime workloads bind too and carries no site knobs,
so the field written on the runtime is refused by name at compile time.
"""

# expect: markdown_negotiation
# expect: OnHosted

import forge

_fe = forge.Frontend {
    name = "web"
    image = "ghcr.io/acme/web"
    path = "web"
    runtime = forge.OnHosted {markdown_negotiation = True}
}
