"""
Negative test — Operator.crds must be non-empty. An operator
without CRDs makes no sense (Service is the right type for that).
"""

import forge

_op = forge.Operator {
    name = "no-crds"
    image = "x"
    crds = []
}
