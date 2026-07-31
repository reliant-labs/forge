"""
Positive test — a container env entry carries EXACTLY ONE source, and
every declared source channel actually renders.

Two defects this pins closed, both invisible to
`kubectl apply --dry-run=client` and both fatal against a real cluster:

  * an EnvVar with a literal `value` AND a `secret_ref` rendered BOTH
    `value:` and `valueFrom:`. The API server rejects that entry
    ("may not be specified when `value` is not empty"), so the deploy
    died server-side after CI went green.

  * `field_ref` — the Downward API channel, declared on EnvVar and
    honoured by the legacy render path — was dropped by the component
    expander. The entry rendered as a bare `{name: POD_IP}`, which k8s
    accepts as an EMPTY variable. Silently empty is worse than either
    of the other outcomes.
"""

import forge.components as fc

_components = [
    fc.Server {
        name = "api"
        ports = [fc.ComponentPort {name = "http", port = 8080, expose = True}]
        env_vars = [
            {name = "DATABASE_URL", secret_ref = "db-credentials", secret_key = "database-url"}
            {name = "FEATURE_MAP", config_map_ref = "features", config_map_key = "map"}
            {name = "POD_IP", field_ref = "status.podIP"}
            {name = "LOG_LEVEL", value = "info"}
            # A literal AND a reference. The reference wins: the literal
            # is the placeholder the config projection emits before a
            # field is bound to a Secret, and the binding is the intent.
            {name = "API_TOKEN", value = "", secret_ref = "app-secrets", secret_key = "api-token"}
        ]
    }
]

_env = fc.ComponentEnv {
    namespace = "demo-prod"
    project = "demo"
    network_policies = False
}

manifests = fc.render_components(_components, _env)

_container = [m for m in manifests if m.kind == "Deployment"][0].spec.template.spec.containers[0]
_by_name = {e.name: e for e in _container.env}

# --- Every channel renders its k8s source ---
assert_secret_channel = _by_name["DATABASE_URL"].valueFrom.secretKeyRef == {name = "db-credentials", key = "database-url"}
assert_config_map_channel = _by_name["FEATURE_MAP"].valueFrom.configMapKeyRef == {name = "features", key = "map"}
assert_field_ref_channel = _by_name["POD_IP"].valueFrom.fieldRef == {fieldPath = "status.podIP"}
assert_literal_channel = _by_name["LOG_LEVEL"].value == "info"

# --- Never two sources on one entry ---
# k8s rejects `value` + `valueFrom` together, and rejects it only
# SERVER-side — client dry-run waves it through.
_dual = [e for e in _container.env if "value" in e and "valueFrom" in e]
assert_never_two_sources = len(_dual) == 0
assert_reference_wins_over_literal = _by_name["API_TOKEN"].valueFrom.secretKeyRef.name == "app-secrets"

# --- And no entry is left with NO source at all ---
_sourceless = [e for e in _container.env if "value" not in e and "valueFrom" not in e]
assert_no_sourceless_entries = len(_sourceless) == 0
