"""
Negative test — SimpleBackend's spec rejects a field it does not declare.

The spec schema (tiers.SimpleBackend) is GENERATED from forge's Go type
pkg/deploy/v1alpha1.SimpleBackendSpec, and it is closed. That is the
enforcement mechanism: the way a hosted tier rejects configuration it must
not honour is to not declare the field, so there is no allowlist to maintain
and none to drift from the schema.

`replicas` is the case worth pinning, because a reader would reasonably
expect it on a deploy block. SimpleBackend renders a ReadWriteOnce PVC for
`storageGiB`, and a multi-replica Deployment mounting one RWO volume either
wedges on rolling update or refuses to schedule across nodes. A `replicas`
knob would offer a setting that silently breaks precisely the workloads that
set `storageGiB`.

KCL rejects an undeclared member at COMPILE time ("Cannot add member
'replicas' to schema 'SimpleBackend'"), so this is asserted by
TestKCLModule_SimpleBackendSchemaIsClosed in
internal/templates/kcl_module_test.go rather than as a negative_*.k fixture.
"""

import forge
import forge.tiers

_sb = forge.RenderedWorkload {
    name = "api"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        spec = tiers.SimpleBackend {
            image = "ghcr.io/acme/app:v1"
            ports = [8080]
            replicas = 3
        }
    }
}
