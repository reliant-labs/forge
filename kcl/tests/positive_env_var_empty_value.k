"""
Positive test — `EnvVar { name = "FOO", value = "" }` is legal and
renders to `{ name: "FOO", value: "" }` in the Deployment manifest.

Regression: forge used to reject `value = ""` because the EnvVar
check block treated empty string as "unset" and demanded one of
value/secret_ref/config_map_ref. cp-forge needed empty-string
sentinels (RUNTIME_CLASS_NAME="" to skip runtimeClassName; webhook
secret env-gate values) and had to work around it.

The fix makes `value` an OPTIONAL field (no default). Now the
distinction is "field absent" (Undefined) vs "field present but
empty" (""). The validator accepts the latter; the renderer
preserves it through both the JSON contract (top-level `output`)
and the K8s manifest (Deployment.spec...env[].value).
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-empty-value"
    namespace = "empty-value-dev"
    registry = "localhost:5050"
}

_svc = forge.Service {
    name = "api"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
    env_vars = [
        # The new contract: empty-string value is a legal disable
        # sentinel. Pre-fix this would fail the EnvVar check block.
        forge.EnvVar { name = "RUNTIME_CLASS_NAME", value = "" }
        forge.EnvVar { name = "LOG_LEVEL", value = "info" }
        # Secret-ref / config-map-ref entries don't set `value` at
        # all — these stay valid (the rendered manifest omits
        # `value` and emits `valueFrom` instead).
        forge.EnvVar { name = "DB_PASSWORD", secret_ref = "app-secrets" }
    ]
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [_svc]
}

# ── JSON contract (top-level `output` shape) ───────────────────────

output = forge.render(_bundle)
_rendered_env = output.services[0].env_vars

# Three env vars all round-tripped.
assert_env_count = len(_rendered_env) == 3

# Empty-value entry: name is "RUNTIME_CLASS_NAME"; value is the empty
# string (NOT null). The "not null" check is the load-bearing one —
# pre-fix, the renderer's `if ev.value` would have demoted "" to null.
assert_empty_name = _rendered_env[0].name == "RUNTIME_CLASS_NAME"
assert_empty_value_is_empty_string = _rendered_env[0].value == ""
assert_empty_value_not_null = _rendered_env[0].value != None

# Populated entry still renders as a string.
assert_populated_value = _rendered_env[1].value == "info"

# Secret-ref entry has `value == None` (no inline value set).
assert_secret_ref_no_value = _rendered_env[2].value == None
assert_secret_ref_name = _rendered_env[2].secret_ref == "app-secrets"

# ── K8s manifests (Deployment.spec...env shape) ────────────────────

manifests = forge.render_manifests(_bundle, "latest", {}, False)
_deployments = [m for m in manifests if m.kind == "Deployment"]
assert_deployment_count = len(_deployments) == 1

_container_env = _deployments[0].spec.template.spec.containers[0].env
assert_container_env_count = len(_container_env) == 3

# Empty-value entry made it into the Deployment manifest as
# `value: ""` — the load-bearing claim that the brief asked us to
# pin: "renders into the Deployment manifest as { name: 'FOO', value: '' }".
assert_container_empty_name = _container_env[0].name == "RUNTIME_CLASS_NAME"
assert_container_empty_value_present = "value" in _container_env[0]
assert_container_empty_value_is_empty_string = _container_env[0].value == ""

# Populated entry preserved.
assert_container_populated_value = _container_env[1].value == "info"

# Secret-ref entry has NO `value` field (the `if ev.value != Undefined`
# guard in _env_source omits it) — and DOES have valueFrom.
assert_container_secret_no_value = "value" not in _container_env[2]
assert_container_secret_valueFrom = _container_env[2].valueFrom.secretKeyRef.name == "app-secrets"
