"""
Negative test — StaticSite rejects a field it does not declare.

This is the CLOSED-SCHEMA property, and it is load-bearing for the
hosted tier rather than incidental. The way to reject configuration a
hosted tier must not honour is to not put it on the schema at all: an
absent field needs no allowlist to maintain, cannot drift out of sync
with one, and KCL refuses it for free with a message naming the schema.

NOT named `negative_*`: KCL rejects this at COMPILE time ("Cannot add
member 'public_access' to schema 'StaticSite'"), which is a stronger
rejection than the `check:` block failures the negative_* harness looks
for — and that harness deliberately refuses to accept a CompileError, so
a typo'd fixture cannot pass vacuously. Weakening it to admit this file
would blunt that guard for every other fixture. Its dedicated test is
TestKCLModule_StaticSiteSchemaIsClosed in
internal/templates/kcl_module_test.go.

`public_access` is the canonical example — who may read the bucket is
the bucket owner's policy, not something a `forge.StaticSite` deploy
block gets to assert. This fixture fails if someone later adds an
escape-hatch passthrough that would make arbitrary keys legal again.
"""

import forge

_fe = forge.Frontend {
    name = "web"
    path = "web"
    deploy = forge.StaticSite {
        bucket = "gs://b"
        public_dir = "dist"
        public_access = True
    }
}
