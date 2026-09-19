"""
Negative test — SimpleBackend rejects a field it does not declare.

This is the CLOSED-SCHEMA property, and for this tier it is the PRIMARY
enforcement mechanism rather than a nicety. SimpleBackend is the hosted
entry tier: it must not honour node placement, cluster-scoped RBAC,
arbitrary security contexts, raw manifest passthrough, or a replica
count. The way to reject those is to not declare them — an absent field
needs no allowlist to maintain, cannot drift out of sync with one, and
KCL refuses it for free with a message naming the schema.

`replicas` is the canonical example here, and unlike StaticSite's
`public_access` it is a field a reader would REASONABLY expect to exist
— which is exactly why its absence has to be pinned rather than assumed.
SimpleBackend renders a ReadWriteOnce PVC for `storage_gib`, and a
multi-replica Deployment mounting one RWO volume either wedges on rolling
update or refuses to schedule. Since the tier cannot express a
StatefulSet, a shared storage class, or a Recreate strategy, a `replicas`
knob would offer a setting that silently breaks precisely the workloads
that set `storage_gib`. Horizontal scale is the next tier up, where those
are expressible.

NOT named `negative_*`: KCL rejects this at COMPILE time ("Cannot add
member 'replicas' to schema 'SimpleBackend'"), which is a stronger
rejection than the `check:` block failures the negative_* harness looks
for — and that harness deliberately refuses to accept a CompileError, so
a typo'd fixture cannot pass vacuously. Weakening it to admit this file
would blunt that guard for every other fixture. Its dedicated test is
TestKCLModule_SimpleBackendSchemaIsClosed in
internal/templates/kcl_module_test.go, alongside StaticSite's.

This fixture fails if someone later adds an escape-hatch passthrough that
would make arbitrary keys legal again.
"""

import forge

_sb = forge.RenderedWorkload {
    name = "api"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/app:v1"
        ports = [8080]
        replicas = 3
    }
}
