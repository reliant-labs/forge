"""
Positive test — the agnostic-core env MAP (`{str: EnvSource}`) + the k8s
adapter projection `forge.env_project`. ADDITIVE alongside EnvVar/env_merge.

Pins the load-bearing claims of FORGE_KCL_IDIOMS.md §3:

  1. ALL THREE CHANNELS project to the correct EnvVar shape
     (value / from_secret->secret_ref+secret_key /
      from_config->config_map_ref+config_map_key).
  2. DETERMINISTIC ORDER — output is sorted by key (env-var name).
  3. NATIVE MAP-MERGE (`base | overrides`) overrides last-wins with NO
     helper — the map makes the dedup footgun structurally impossible.
  4. EMPTY-STRING value is a legal channel (mirrors EnvVar).
  5. RENDERER-COMPATIBLE — env_project's [EnvVar] renders to the SAME k8s
     container env: a hand-written [EnvVar] list does, through the existing
     render_manifests path.
"""

import forge

# ── (1) all three channels ─────────────────────────────────────────
_env = {
    "LOG_LEVEL" = {value = "info"}
    "DATABASE_URL" = {from_secret = {name = "db", key = "url"}}
    "APP_MODE" = {from_config = {name = "app-config", key = "mode"}}
}

_projected = forge.env_project(_env)

# sorted by key: APP_MODE, DATABASE_URL, LOG_LEVEL
assert_count = len(_projected) == 3
assert_order = [e.name for e in _projected] == ["APP_MODE", "DATABASE_URL", "LOG_LEVEL"]

_by_name = {e.name: e for e in _projected}

# value channel
assert_value_channel = _by_name["LOG_LEVEL"].value == "info"
assert_value_no_secret = _by_name["LOG_LEVEL"].secret_ref == "" and _by_name["LOG_LEVEL"].config_map_ref == ""

# from_secret -> secret_ref + secret_key
assert_secret_ref = _by_name["DATABASE_URL"].secret_ref == "db"
assert_secret_key = _by_name["DATABASE_URL"].secret_key == "url"
assert_secret_no_value = _by_name["DATABASE_URL"].value == Undefined

# from_config -> config_map_ref + config_map_key
assert_config_ref = _by_name["APP_MODE"].config_map_ref == "app-config"
assert_config_key = _by_name["APP_MODE"].config_map_key == "mode"
assert_config_no_value = _by_name["APP_MODE"].value == Undefined

# The projected entries are genuine EnvVar instances (pass EnvVar's own check).
assert_is_envvar = typeof(_projected[0]) == "EnvVar"

# ── (2) deterministic order regardless of authoring order ──────────
_scrambled = {
    "ZULU" = {value = "z"}
    "ALPHA" = {value = "a"}
    "MIKE" = {value = "m"}
}
assert_sorted = [e.name for e in forge.env_project(_scrambled)] == ["ALPHA", "MIKE", "ZULU"]

# ── (3) native map-merge, last-wins, no helper ─────────────────────
_base_env = {
    "LOG_LEVEL" = {value = "info"}
    "REGION" = {value = "us-east"}
}
_merged_env = _base_env | {"LOG_LEVEL" = {value = "debug"}, "FEATURE_X" = {value = "on"}}
assert_merge_count = len(_merged_env) == 3
assert_merge_override = _merged_env["LOG_LEVEL"].value == "debug"   # last-wins
assert_merge_preserved = _merged_env["REGION"].value == "us-east"
assert_merge_added = _merged_env["FEATURE_X"].value == "on"
# The map physically CANNOT hold two LOG_LEVEL keys — dedup footgun impossible.
_merged_projected = forge.env_project(_merged_env)
_merged_log = [e for e in _merged_projected if e.name == "LOG_LEVEL"]
assert_merge_one_log = len(_merged_log) == 1
assert_merge_projected_value = _merged_log[0].value == "debug"

# ── (4) empty-string value is a legal channel ──────────────────────
_empty = forge.env_project({"RUNTIME_CLASS_NAME" = {value = ""}})
assert_empty_value_set = _empty[0].value == ""
assert_empty_value_is_value_channel = _empty[0].secret_ref == "" and _empty[0].config_map_ref == ""

# ── (5) renderer-compatible — project == hand-written [EnvVar] ─────
# Same three vars authored two ways: as a keyed map (projected) and as a
# hand-written [EnvVar] list. Render BOTH through render_manifests and assert
# the k8s container env: blocks are IDENTICAL.
_target = forge.ClusterTarget {
    cluster = "k3d-env-project"
    namespace = "env-project-dev"
    registry = "localhost:5050"
}

# The hand-written list, in the SAME sorted order env_project emits.
_handwritten = [
    forge.EnvVar {name = "APP_MODE", config_map_ref = "app-config", config_map_key = "mode"}
    forge.EnvVar {name = "DATABASE_URL", secret_ref = "db", secret_key = "url"}
    forge.EnvVar {name = "LOG_LEVEL", value = "info"}
]

_svc_map = forge.K8sWorkload {
    name = "api-map"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
    env_vars = forge.env_project(_env)
}
_svc_list = forge.K8sWorkload {
    name = "api-list"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
    env_vars = _handwritten
}
_bundle = forge.Bundle {cluster_target = _target, services = [_svc_map, _svc_list]}
manifests = forge.render_manifests(_bundle, "latest", {}, False)

_deploys = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_map_env = _deploys["api-map"].spec.template.spec.containers[0].env
_list_env = _deploys["api-list"].spec.template.spec.containers[0].env

# The rendered container env blocks are byte-identical.
assert_render_equivalent = _map_env == _list_env

# And spot-check the actual k8s shapes the projection produced.
assert_render_count = len(_map_env) == 3
_map_by_name = {e.name: e for e in _map_env}
assert_render_value = _map_by_name["LOG_LEVEL"].value == "info"
assert_render_secret = _map_by_name["DATABASE_URL"].valueFrom.secretKeyRef == {name = "db", key = "url"}
assert_render_config = _map_by_name["APP_MODE"].valueFrom.configMapKeyRef == {name = "app-config", key = "mode"}
