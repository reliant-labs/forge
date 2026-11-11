"""
Negative test — forge.Probe refuses an expect_status that is not a status.
"""

# expect: Probe.expect_status
# expect: must be statuses like

import forge

_p = forge.Probe {
    expect_status = ["ok"]
}
