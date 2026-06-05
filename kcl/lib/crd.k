"""
Forge KCL — CRD builders.

A thin helper for declaring CRDs that an Operator owns. Projects
either:

  1. Reference an `Operator.crds` list of kind names and let forge's
     `forge add crd` codegen emit a starter CRD manifest into the
     project's tree (the legacy + still-supported path).
  2. OR construct CRD dicts directly via `crd(...)` below and append
     them to `Bundle.additional_manifests` for a hand-typed schema.

The starter CRDs `forge add crd` emits use a permissive
`x-kubernetes-preserve-unknown-fields: true` schema so the CRD
installs immediately; the Go API types in `api/<version>/<lower>_types.go`
stay the source of truth.
"""

# Build a minimal v1 CRD dict. Callers populate `version_schemas`
# (a list of {name: str, schema: {...}} dicts) to layer per-version
# validation onto the resource.
crd = lambda group: str, kind: str, plural: str, singular: str, scope: str = "Namespaced",
                  version_schemas: [{str: any}] = [{
                      "name": "v1alpha1"
                      "schema": {
                          "type": "object"
                          "x-kubernetes-preserve-unknown-fields": True
                      }
                  }] -> any {
    {
        apiVersion = "apiextensions.k8s.io/v1"
        kind = "CustomResourceDefinition"
        metadata = {
            name = "${plural}.${group}"
            labels = {"app.kubernetes.io/managed-by" = "forge"}
        }
        spec = {
            group = group
            scope = scope
            names = {
                kind = kind
                plural = plural
                singular = singular
            }
            versions = [
                {
                    name = vs["name"]
                    served = True
                    storage = True
                    schema = {openAPIV3Schema = vs["schema"]}
                }
                for vs in version_schemas
            ]
        }
    }
}
