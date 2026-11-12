"""
Negative test — an Apple team id is ten upper-case letters and digits.
"""

# expect: ten-character Apple Developer team id

import forge

_fe = forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", apple_team_id = "team-1"}
