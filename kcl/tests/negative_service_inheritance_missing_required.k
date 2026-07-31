"""
Negative test — an app schema that inherits forge.RenderedWorkload can add its OWN
required fields, enforced at PARSE time.

BillingService(forge.RenderedWorkload) declares `region` with a `check:` that rejects
an empty value. Constructing it with `region = ""` must fail the check
block — proving the app's extra required-field semantics are enforced when
the value is loaded, exactly like forge's own checks. (A fully-OMITTED
`region` also fails — with KCL's `attribute 'region' is required` error —
but we drive the CHECK here so the harness's "Check failed" signal fires.)
"""

import forge

schema BillingService(forge.RenderedWorkload):
    region: str
    tier: "free" | "pro" = "free"

    check:
        region, "BillingService.region is required"

# region = "" sets the field but fails the check — the app's own
# required-field enforcement at parse time.
_svc = BillingService {
    name = "billing-api"
    image = "billing-api"
    region = ""
}

output = _svc.name
