"""
Negative test — an Android certificate fingerprint is SHA-256 colon-hex (or
"debug"), never a SHA-1 or a free string.
"""

# expect: SHA-256 certificate fingerprints

import forge

_fe = forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", android_cert_fingerprints = {dev = ["AA:BB:CC"]}}
