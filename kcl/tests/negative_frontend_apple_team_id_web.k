"""
Negative test — platform signing identities are for a mobile frontend only.
"""

# expect: apply only to a 'rn' frontend

import forge

_fe = forge.Frontend {name = "web", path = "frontends/web", type = "nextjs", apple_team_id = "23P64LQTZD"}
