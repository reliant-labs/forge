"""
Negative test — ShellBuild requires `cmd`.
"""
import forge
_s = forge.RenderedWorkload { name = "x", build = forge.ShellBuild {} }
output = forge.render(forge.Bundle { services = [_s] })
