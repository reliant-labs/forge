"""
Negative test — GoBuild requires `cmd`. Omitting it must fail the
schema check (no hardcoded ./cmd fallback in the schema).
"""
import forge
_s = forge.Service { name = "x", build = forge.GoBuild {} }
output = forge.render(forge.Bundle { services = [_s] })
